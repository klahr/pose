package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/klahr/pose/internal/providers"
	"github.com/klahr/pose/internal/state"
)

// Apply executes the plan in dependency order:
//
//	prune services → prune packages → prune dotfiles → install packages →
//	link dotfiles → run hooks → apply services
//
// State is flushed after every successful action, so an interrupted run resumes
// cleanly with the next plan. Failed actions are reported at the end (and
// retried next run) so that one bad package or link does not block the rest;
// only an interrupt or a failure to write state aborts the run.
func (e *Engine) Apply(ctx context.Context, p *Plan) error {
	// Bookkeeping that does not touch the system.
	for _, a := range p.Actions {
		var err error
		switch a.Kind {
		case Adopt:
			err = e.store.Put(a.Key, a.resource())
		case Forget:
			err = e.store.Delete(a.Key)
		}
		if err != nil {
			return err
		}
	}

	var errs []error
	for _, step := range []func(context.Context, *Plan) error{
		e.pruneServices, e.prunePackages, e.pruneDotfiles,
		e.installPackages, e.linkDotfiles, e.runHooks, e.applyServices,
	} {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := step(ctx, p); err != nil {
			errs = append(errs, err)
			if errors.Is(err, state.ErrSave) {
				return errors.Join(errs...)
			}
		}
	}
	return errors.Join(errs...)
}

// pruneServices keeps going past failed services; see Apply.
func (e *Engine) pruneServices(ctx context.Context, p *Plan) error {
	var errs []error
	for _, a := range p.filter(state.TypeService, Prune) {
		if ctx.Err() != nil {
			break
		}
		e.ui.Step(a, "stopping and disabling")
		if err := e.services.Prune(ctx, a.Name, a.Scope); err != nil {
			errs = append(errs, fmt.Errorf("prune service %s: %w", a.Name, err))
			continue
		}
		if err := e.store.Delete(a.Key); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) prunePackages(ctx context.Context, p *Plan) error {
	return e.mutatePackages(ctx, p.filter(state.TypePackage, Prune), false)
}

func (e *Engine) installPackages(ctx context.Context, p *Plan) error {
	return e.mutatePackages(ctx, p.filter(state.TypePackage, Install), true)
}

// mutatePackages installs or removes packages, batched per manager. After a
// failed batch every package is re-checked so state reflects partial progress,
// and the remaining managers still run; their failures are joined.
func (e *Engine) mutatePackages(ctx context.Context, actions []*Action, install bool) error {
	var managers []string
	byManager := map[string][]*Action{}
	for _, a := range actions {
		if _, ok := byManager[a.Manager]; !ok {
			managers = append(managers, a.Manager)
		}
		byManager[a.Manager] = append(byManager[a.Manager], a)
	}

	verb := "uninstalling"
	if install {
		verb = "installing"
	}
	var errs []error
	for _, mgr := range managers {
		if ctx.Err() != nil {
			break
		}
		group := byManager[mgr]
		pm := e.packages[mgr]
		names := make([]string, len(group))
		for i, a := range group {
			names[i] = a.Name
		}
		e.ui.StepGroup(group[0].Kind, fmt.Sprintf("%s %s (%s)", verb, strings.Join(names, ", "), mgr))

		var err error
		if install {
			err = pm.Install(ctx, names)
		} else {
			err = pm.Remove(ctx, names)
		}

		done := group
		if err != nil {
			done = nil
			// Use a fresh context, as the run may have been interrupted, but
			// bound it so a hanging check cannot block the exit.
			cctx, cancel := context.WithTimeout(context.Background(), recheckTimeout)
			for _, a := range group {
				ok, cerr := pm.IsInstalled(cctx, a.Name)
				if cerr == nil && ok == install {
					done = append(done, a)
				}
			}
			cancel()
		}
		if serr := e.store.Update(func(st *state.State) {
			for _, a := range done {
				if install {
					st.Resources[a.Key] = a.resource()
				} else {
					delete(st.Resources, a.Key)
				}
			}
		}); serr != nil {
			return errors.Join(append(errs, err, serr)...)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s packages: %w", verb, mgr, err))
		}
	}
	return errors.Join(errs...)
}

const recheckTimeout = 30 * time.Second

// pruneDotfiles keeps going past failed dotfiles; see Apply.
func (e *Engine) pruneDotfiles(_ context.Context, p *Plan) error {
	var errs []error
	for _, a := range p.filter(state.TypeDotfile, Prune) {
		e.ui.Step(a, "removing symlink")
		removed, err := providers.UnlinkDotfile(a.SourcePath, a.TargetPath)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !removed {
			e.ui.Warn("%s changed since planning; left in place", a.Target)
		} else if a.Backup != "" {
			restored, err := providers.RestoreBackup(a.Backup, a.TargetPath)
			if err != nil {
				// The symlink is gone either way; report and stop tracking it.
				errs = append(errs, err)
			} else if restored {
				e.ui.Note("restored %s from %s", a.Target, a.Backup)
			}
		}
		if err := e.store.Delete(a.Key); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

// linkDotfiles keeps going past failed links; see Apply.
func (e *Engine) linkDotfiles(_ context.Context, p *Plan) error {
	now := time.Now()
	var errs []error
	for _, a := range p.filter(state.TypeDotfile, Link, Update) {
		e.ui.Step(a, "linking")
		backup, err := providers.LinkDotfile(a.SourcePath, a.TargetPath, now)
		if backup != "" {
			e.ui.Note("backed up existing file to %s", backup)
		}
		if err != nil {
			a.Hook = false // not linked, so its hook must not run or be recorded
			errs = append(errs, err)
			continue
		}
		// Keep the first backup while it exists: it holds the file from before pose,
		// later ones what an app wrote over the link. On the action, so runHooks
		// records it too.
		if _, err := os.Lstat(a.Backup); backup != "" && (a.Backup == "" || err != nil) {
			a.Backup = backup
		}
		r := a.resource()
		if a.Hook {
			// Record the new hash only once the hook has run, so a failed hook is retried.
			r.Hash = a.PrevHash
		}
		if err := e.store.Put(a.Key, r); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

// runHooks runs each distinct on_change command once, in config order.
func (e *Engine) runHooks(ctx context.Context, p *Plan) error {
	var commands []string
	byCommand := map[string][]*Action{}
	for _, a := range p.filter(state.TypeDotfile, Link, Update) {
		if !a.Hook {
			continue
		}
		if _, ok := byCommand[a.OnChange]; !ok {
			commands = append(commands, a.OnChange)
		}
		byCommand[a.OnChange] = append(byCommand[a.OnChange], a)
	}

	var errs []error
	for _, cmd := range commands {
		if ctx.Err() != nil {
			break
		}
		e.ui.StepGroup(Update, fmt.Sprintf("running hook %q", cmd))
		if err := providers.RunHook(ctx, cmd, e.cfg.Root); err != nil {
			errs = append(errs, fmt.Errorf("hook %q: %w", cmd, err))
			continue
		}
		if err := e.store.Update(func(st *state.State) {
			for _, a := range byCommand[cmd] {
				st.Resources[a.Key] = a.resource()
			}
		}); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// applyServices keeps going past failed services; see Apply.
func (e *Engine) applyServices(ctx context.Context, p *Plan) error {
	actions := p.filter(state.TypeService, Update)
	reloaded := map[string]bool{}
	for _, a := range actions {
		if !reloaded[a.Scope] {
			if err := e.services.Reload(ctx, a.Scope); err != nil {
				return fmt.Errorf("reload %s services: %w", a.Scope, err)
			}
			reloaded[a.Scope] = true
		}
	}
	var errs []error
	for _, a := range actions {
		if ctx.Err() != nil {
			break
		}
		e.ui.Step(a, "updating")
		if err := e.services.Apply(ctx, a.Name, a.Scope, a.Active, a.Enabled); err != nil {
			errs = append(errs, fmt.Errorf("apply service %s: %w", a.Name, err))
			continue
		}
		if err := e.store.Put(a.Key, a.resource()); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}
