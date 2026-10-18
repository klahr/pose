// Package engine compares desired config, recorded state and the live system,
// and converges the system in a fixed dependency order.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/klahr/pose/internal/config"
	"github.com/klahr/pose/internal/providers"
	"github.com/klahr/pose/internal/state"
)

type Kind int

const (
	Install Kind = iota // + package
	Link                // + symlink
	Update              // ~ dotfile or service
	Prune               // - any resource
	Adopt               // state only: the system already matches, start tracking it
	Forget              // state only: the resource is already gone, stop tracking it
)

// Changes reports whether the action modifies the system.
func (k Kind) Changes() bool { return k <= Prune }

type Action struct {
	Kind Kind
	Type string // state.TypePackage, TypeDotfile or TypeService
	Key  string // state key

	Name    string // package or service name
	Manager string // package manager
	Scope   string // service scope

	Target     string // dotfile target, display form
	Source     string // dotfile source, display form
	TargetPath string
	SourcePath string
	Hash       string // current source hash, only computed when OnChange is set
	PrevHash   string // hash recorded in state
	OnChange   string
	Hook       bool   // run OnChange after linking
	Backup     string // backup recorded in state, restored on prune

	Active  bool // desired service state
	Enabled bool

	Adopted bool // package or service: see state.Resource.Adopted

	Notes []string
}

// resource is the state entry recorded once the action has succeeded.
func (a *Action) resource() state.Resource {
	switch a.Type {
	case state.TypePackage:
		return state.Resource{Type: a.Type, Manager: a.Manager, ID: a.Name, Adopted: a.Adopted}
	case state.TypeDotfile:
		return state.Resource{Type: a.Type, Target: a.TargetPath, Source: a.SourcePath, Hash: a.Hash, OnChange: a.OnChange, Backup: a.Backup}
	default:
		enabled := a.Enabled
		return state.Resource{Type: a.Type, Name: a.Name, Scope: a.Scope, State: stateWord(a.Active), Enabled: &enabled, Adopted: a.Adopted}
	}
}

type Plan struct {
	Actions  []*Action
	Warnings []string
}

func (p *Plan) HasChanges() bool {
	for _, a := range p.Actions {
		if a.Kind.Changes() {
			return true
		}
	}
	return false
}

func (p *Plan) Empty() bool { return len(p.Actions) == 0 }

func (p *Plan) Count(k Kind) int {
	n := 0
	for _, a := range p.Actions {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func (p *Plan) filter(typ string, kinds ...Kind) []*Action {
	var out []*Action
	for _, a := range p.Actions {
		if a.Type != typ {
			continue
		}
		for _, k := range kinds {
			if a.Kind == k {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

type Engine struct {
	cfg      *config.Config
	store    *state.Store
	ui       *UI
	packages map[string]*providers.PackageManager
	services providers.ServiceManager
}

func New(cfg *config.Config, store *state.Store, ui *UI) (*Engine, error) {
	e := &Engine{cfg: cfg, store: store, ui: ui, packages: map[string]*providers.PackageManager{}}
	for name, def := range cfg.Providers.PackageManagers {
		e.packages[name] = providers.NewPackageManager(name, def)
	}

	needServices := len(cfg.Services) > 0
	for _, r := range store.State.Resources {
		needServices = needServices || r.Type == state.TypeService
	}
	svc, err := providers.NewServiceManager()
	if err != nil && needServices {
		return nil, err
	}
	e.services = svc
	store.State.Hostname = cfg.Hostname
	return e, nil
}

// Plan computes the actions needed to converge the system. It does not modify anything.
func (e *Engine) Plan(ctx context.Context) (*Plan, error) {
	p := &Plan{}
	for _, step := range []func(context.Context, *Plan) error{e.planPackages, e.planDotfiles, e.planServices} {
		if err := step(ctx, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (e *Engine) planPackages(ctx context.Context, p *Plan) error {
	type item struct {
		mgr, pkg, key    string
		desired, adopted bool
	}
	var items []item
	desired := map[string]bool{}
	for _, mgr := range sortedKeys(e.cfg.Packages) {
		for _, pkg := range e.cfg.Packages[mgr] {
			key := state.PackageKey(mgr, pkg)
			desired[key] = true
			items = append(items, item{mgr, pkg, key, true, false})
		}
	}
	for _, key := range e.store.State.Keys() {
		r := e.store.State.Resources[key]
		if r.Type != state.TypePackage || desired[key] {
			continue
		}
		if _, ok := e.packages[r.Manager]; !ok {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"cannot prune package %q: package manager %q is no longer defined; keeping it in state", r.ID, r.Manager))
			continue
		}
		items = append(items, item{r.Manager, r.ID, key, false, r.Adopted})
	}

	installed := make([]bool, len(items))
	errs := make([]error, len(items))
	parallel(len(items), 8, func(i int) {
		installed[i], errs[i] = e.packages[items[i].mgr].IsInstalled(ctx, items[i].pkg)
	})
	// A package only left to prune must not block every run once its manager is
	// gone; there is nothing left to uninstall it with.
	missing := make([]bool, len(items))
	for i, err := range errs {
		if err != nil && !items[i].desired && providers.ManagerMissing(err) {
			missing[i], errs[i] = true, nil
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	for i, it := range items {
		_, tracked := e.store.State.Resources[it.key]
		a := &Action{Type: state.TypePackage, Key: it.key, Name: it.pkg, Manager: it.mgr}
		switch {
		case it.desired && !installed[i]:
			a.Kind = Install
		case it.desired && !tracked:
			a.Kind, a.Adopted = Adopt, true
		case it.desired:
			continue
		case missing[i]:
			a.Kind = Forget
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"package %q (%s) is no longer in config, but package manager %q is not available; forgetting it without uninstalling", it.pkg, it.mgr, it.mgr))
		case installed[i] && it.adopted:
			a.Kind = Forget
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"package %q (%s) is no longer in config but was installed before pose tracked it; leaving it installed", it.pkg, it.mgr))
		case installed[i]:
			a.Kind = Prune
			a.Notes = []string{"will be uninstalled, missing from config"}
		default:
			a.Kind = Forget
		}
		p.Actions = append(p.Actions, a)
	}
	return nil
}

func (e *Engine) planDotfiles(_ context.Context, p *Plan) error {
	desired := map[string]bool{}
	desiredTargets := map[string]bool{}
	for _, d := range e.cfg.Dotfiles {
		target := config.ContractHome(d.TargetPath, e.cfg.Home)
		key := state.DotfileKey(target)
		desired[key] = true
		desiredTargets[d.TargetPath] = true

		// The hash only decides whether on_change must run; content edits are
		// otherwise live already, since the target is a symlink.
		var hash string
		if d.OnChange != "" {
			var err error
			if hash, err = providers.HashPath(d.SourcePath); err != nil {
				return err
			}
		}
		st, err := providers.InspectDotfile(d.SourcePath, d.TargetPath)
		if err != nil {
			return err
		}
		prior, tracked := e.store.State.Resources[key]

		a := &Action{
			Type: state.TypeDotfile, Key: key,
			Target: target, Source: e.displaySource(d.SourcePath),
			TargetPath: d.TargetPath, SourcePath: d.SourcePath,
			Hash: hash, PrevHash: prior.Hash, OnChange: d.OnChange, Backup: prior.Backup,
		}
		changed := true
		switch st.Status {
		case providers.LinkMissing:
			a.Kind = Link
		case providers.LinkCollision:
			a.Kind = Link
			a.Notes = append(a.Notes, fmt.Sprintf("existing %s will be backed up to %s.backup.<timestamp>", target, target))
		case providers.LinkElsewhere:
			a.Kind = Update
			a.Notes = append(a.Notes, fmt.Sprintf("symlink currently points to %q", st.Dest))
		case providers.LinkCorrect:
			switch {
			case !tracked:
				a.Kind, changed = Adopt, false
			case d.OnChange != "" && prior.Hash == "":
				// Linked by an earlier run whose hook failed or was interrupted,
				// or on_change was added to an existing dotfile.
				a.Kind = Update
				a.Notes = append(a.Notes, "on_change hook has not run for the current content")
			case d.OnChange != "" && prior.Hash != hash:
				a.Kind = Update
				a.Notes = append(a.Notes, fmt.Sprintf("file modified (hash: %s -> %s)", shortHash(prior.Hash), shortHash(hash)))
			case prior.Hash != hash || prior.OnChange != d.OnChange || prior.Source != d.SourcePath || prior.Target != d.TargetPath:
				a.Kind, changed = Adopt, false
			default:
				continue
			}
		}
		if changed && d.OnChange != "" {
			a.Hook = true
			a.Notes = append(a.Notes, fmt.Sprintf("trigger hook: %q", d.OnChange))
		}
		p.Actions = append(p.Actions, a)
	}

	for _, key := range e.store.State.Keys() {
		r := e.store.State.Resources[key]
		if r.Type != state.TypeDotfile || desired[key] {
			continue
		}
		st, err := providers.InspectDotfile(r.Source, r.Target)
		if err != nil {
			return err
		}
		a := &Action{
			Type: state.TypeDotfile, Key: key,
			Target: config.ContractHome(r.Target, e.cfg.Home), Source: e.displaySource(r.Source),
			TargetPath: r.Target, SourcePath: r.Source, Backup: r.Backup,
		}
		switch {
		case desiredTargets[r.Target]:
			// Tracked under another key (e.g. $HOME was spelled differently); the
			// desired entry owns the target now, so it must not be unlinked.
			a.Kind = Forget
		case st.Status == providers.LinkCorrect:
			a.Kind = Prune
			a.Notes = []string{"symlink will be removed, missing from config"}
			if r.Backup != "" {
				a.Notes = append(a.Notes, fmt.Sprintf("backup %s will be restored if still present", config.ContractHome(r.Backup, e.cfg.Home)))
			}
		case st.Status == providers.LinkMissing:
			a.Kind = Forget
		default:
			a.Kind = Forget
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s is no longer in config but was replaced outside pose; leaving it in place", a.Target))
		}
		p.Actions = append(p.Actions, a)
	}
	return nil
}

func (e *Engine) planServices(ctx context.Context, p *Plan) error {
	desired := map[string]bool{}
	for _, s := range e.cfg.Services {
		key := state.ServiceKey(s.Scope, s.Name)
		desired[key] = true
		st, err := e.services.Status(ctx, s.Name, s.Scope)
		if err != nil {
			return fmt.Errorf("query service %s: %w", s.Name, err)
		}
		active := s.State == config.StateActive
		prior, tracked := e.store.State.Resources[key]
		a := &Action{Type: state.TypeService, Key: key, Name: s.Name, Scope: s.Scope, Active: active, Enabled: s.Enabled, Adopted: prior.Adopted}
		enabledDiffers := st.Enabled != s.Enabled && !st.Static

		switch {
		case st.Active != active || enabledDiffers:
			a.Kind = Update
			if st.Active != active {
				a.Notes = append(a.Notes, fmt.Sprintf("state: %s -> %s", stateWord(st.Active), s.State))
			}
			if enabledDiffers {
				a.Notes = append(a.Notes, fmt.Sprintf("enabled: %t -> %t", st.Enabled, s.Enabled))
			}
			if !st.Exists {
				a.Notes = append(a.Notes, "unit not found yet; it must be installed by the time services are applied")
			}
		case !tracked || prior.State != s.State || prior.Enabled == nil || *prior.Enabled != s.Enabled:
			a.Kind = Adopt
			a.Adopted = !tracked || prior.Adopted
		default:
			continue
		}
		p.Actions = append(p.Actions, a)
	}

	for _, key := range e.store.State.Keys() {
		r := e.store.State.Resources[key]
		if r.Type != state.TypeService || desired[key] {
			continue
		}
		st, err := e.services.Status(ctx, r.Name, r.Scope)
		if err != nil {
			return fmt.Errorf("query service %s: %w", r.Name, err)
		}
		a := &Action{Type: state.TypeService, Key: key, Name: r.Name, Scope: r.Scope, Kind: Forget}
		switch {
		case (st.Active || st.Enabled) && r.Adopted:
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"service %s (%s) is no longer in config but was already set up before pose tracked it; leaving it as is", r.Name, r.Scope))
		case st.Active || st.Enabled:
			a.Kind = Prune
			a.Notes = []string{"will be stopped and disabled, missing from config"}
		}
		p.Actions = append(p.Actions, a)
	}
	return nil
}

// displaySource shows sources inside the repository as ./relative paths.
func (e *Engine) displaySource(p string) string {
	if rel, err := filepath.Rel(e.cfg.Root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return "./" + rel
	}
	return config.ContractHome(p, e.cfg.Home)
}

func stateWord(active bool) string {
	if active {
		return config.StateActive
	}
	return config.StateStopped
}

func shortHash(h string) string {
	if h == "" {
		return "none"
	}
	return h[:min(6, len(h))]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parallel runs fn(0..n-1) with at most limit calls in flight.
func parallel(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
