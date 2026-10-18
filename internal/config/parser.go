package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	BaseFile = "pose.yaml"
	HostsDir = "hosts"
)

var (
	hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$`)
	managerRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// Package and service names are restricted to characters that never need
	// shell quoting, so templates stay safe even when {package} sits inside quotes.
	packageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._+:/=-]*$`)
	serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]*$`)
	scriptRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// ShortHostname returns os.Hostname() without its domain, lowercased.
func ShortHostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("determine hostname: %w", err)
	}
	return NormalizeHostname(h), nil
}

func NormalizeHostname(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

// ValidHostname reports whether h can name a host overlay file.
func ValidHostname(h string) bool {
	return hostnameRe.MatchString(h)
}

// ExpandHome expands a leading "~" or "~/" to home. "~user" is not supported.
func ExpandHome(p, home string) (string, error) {
	switch {
	case p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:]), nil
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%q: only ~ and ~/ are supported, not ~user", p)
	}
	return p, nil
}

// ContractHome is the inverse of ExpandHome, used for display and state keys.
func ContractHome(p, home string) string {
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// Load reads <root>/pose.yaml, merges <root>/hosts/<hostname>.yaml on top if it
// exists, and validates the result. Each file's includes are merged just before it.
func Load(root, hostname string) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("determine home directory: %w", err)
	}
	// A trailing slash would change every state key derived from ContractHome.
	home = filepath.Clean(home)
	if root, err = ExpandHome(root, home); err != nil {
		return nil, err
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	if !hostnameRe.MatchString(hostname) {
		return nil, fmt.Errorf("invalid hostname %q", hostname)
	}

	base, err := parseFile(filepath.Join(root, BaseFile), root, home)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no %s found in %s", BaseFile, root)
	} else if err != nil {
		return nil, err
	}

	cfg := &Config{
		Root:      root,
		Home:      home,
		Hostname:  hostname,
		Providers: Providers{PackageManagers: BuiltinPackageManagers()},
		Packages:  map[string][]string{},
	}
	if err := cfg.mergeLayer(base, filepath.Join(root, BaseFile)); err != nil {
		return nil, err
	}

	hostPath := filepath.Join(root, HostsDir, hostname+".yaml")
	overlay, err := parseFile(hostPath, root, home)
	switch {
	case err == nil:
		if err := cfg.mergeLayer(overlay, hostPath); err != nil {
			return nil, err
		}
		cfg.HostFile = hostPath
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	if err := cfg.expand(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// mergeLayer merges the files c (parsed from path) includes, then c itself.
func (cfg *Config) mergeLayer(c *Config, path string) error {
	for _, inc := range c.Include {
		p, err := ExpandHome(inc, cfg.Home)
		if err != nil {
			return fmt.Errorf("%s: include: %w", path, err)
		}
		if p == "" {
			return fmt.Errorf("%s: include: empty path", path)
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cfg.Root, p)
		}
		p = filepath.Clean(p)
		if slices.Contains(cfg.Included, p) {
			return fmt.Errorf("%s: %s is included more than once", path, inc)
		}
		ic, err := parseFile(p, cfg.Root, cfg.Home)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: include %s: file not found", path, inc)
		} else if err != nil {
			return err
		}
		if len(ic.Include) > 0 {
			return fmt.Errorf("%s: included files cannot include others", p)
		}
		Merge(cfg, ic)
		cfg.Included = append(cfg.Included, p)
	}
	Merge(cfg, c)
	return nil
}

func parseFile(path, root, home string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var c Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: only one YAML document is supported", path)
	}
	if err := c.resolve(root, home); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// resolve expands paths, applies defaults and rejects duplicates within one file.
func (c *Config) resolve(root, home string) error {
	var errs []error
	targets := map[string]bool{}
	for i := range c.Dotfiles {
		d := &c.Dotfiles[i]
		if d.Source == "" || d.Target == "" {
			errs = append(errs, fmt.Errorf("dotfiles[%d]: source and target are required", i))
			continue
		}
		src, err := ExpandHome(d.Source, home)
		if err != nil {
			errs = append(errs, fmt.Errorf("dotfiles[%d].source: %w", i, err))
			continue
		}
		if !filepath.IsAbs(src) {
			src = filepath.Join(root, src)
		}
		d.SourcePath = filepath.Clean(src)

		dst, err := ExpandHome(d.Target, home)
		if err != nil {
			errs = append(errs, fmt.Errorf("dotfiles[%d].target: %w", i, err))
			continue
		}
		if !filepath.IsAbs(dst) {
			errs = append(errs, fmt.Errorf("dotfiles[%d].target %q: must be absolute or start with ~/", i, d.Target))
			continue
		}
		d.TargetPath = filepath.Clean(dst)
		if targets[d.TargetPath] {
			errs = append(errs, fmt.Errorf("dotfiles[%d]: duplicate target %s", i, d.Target))
		}
		targets[d.TargetPath] = true
	}

	keys := map[string]bool{}
	for i := range c.Services {
		s := &c.Services[i]
		if s.Scope == "" {
			s.Scope = ScopeUser
		}
		if s.State == "" {
			s.State = StateActive
		}
		if s.EnabledOpt != nil {
			s.Enabled = *s.EnabledOpt
		} else {
			s.Enabled = s.State == StateActive
		}
		if keys[s.Key()] {
			errs = append(errs, fmt.Errorf("services[%d]: duplicate service %s", i, s.Key()))
		}
		keys[s.Key()] = true
	}

	scripts := map[string]bool{}
	for i := range c.Scripts {
		s := &c.Scripts[i]
		if s.Phase == "" {
			s.Phase = PhaseAfter
		}
		if scripts[s.Name] {
			errs = append(errs, fmt.Errorf("scripts[%d]: duplicate script %s", i, s.Name))
		}
		scripts[s.Name] = true
	}
	return errors.Join(errs...)
}

// expand replaces each recursive dotfile with one entry per file or symlink
// below its source. It runs after merging, so overlays replace whole trees.
func (c *Config) expand() error {
	var out []Dotfile
	var errs []error
	for _, d := range c.Dotfiles {
		if !d.Recursive {
			out = append(out, d)
			continue
		}
		root, err := filepath.EvalSymlinks(d.SourcePath)
		if err == nil {
			var fi fs.FileInfo
			if fi, err = os.Stat(root); err == nil && !fi.IsDir() {
				err = errors.New("recursive requires a directory")
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("dotfile %s: source %s: %w", d.Target, d.Source, unwrapPathError(err)))
			continue
		}
		err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			f := d
			f.Recursive = false
			f.Source, f.SourcePath = filepath.Join(d.Source, rel), filepath.Join(d.SourcePath, rel)
			f.Target, f.TargetPath = filepath.Join(d.Target, rel), filepath.Join(d.TargetPath, rel)
			out = append(out, f)
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("dotfile %s: %w", d.Target, err))
		}
	}
	c.Dotfiles = out
	return errors.Join(errs...)
}

func (c *Config) validate() error {
	var errs []error

	names := make([]string, 0, len(c.Packages))
	for name := range c.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, mgr := range names {
		pm, ok := c.Providers.PackageManagers[mgr]
		if !ok {
			errs = append(errs, fmt.Errorf("packages.%s: no package manager %q defined under providers.package_managers", mgr, mgr))
			continue
		}
		if !managerRe.MatchString(mgr) {
			errs = append(errs, fmt.Errorf("packages.%s: invalid package manager name", mgr))
		}
		if err := validateTemplates(pm); err != nil {
			errs = append(errs, fmt.Errorf("providers.package_managers.%s: %w", mgr, err))
		}
		for _, pkg := range c.Packages[mgr] {
			if !packageRe.MatchString(pkg) {
				errs = append(errs, fmt.Errorf("packages.%s: invalid package name %q", mgr, pkg))
			}
		}
	}

	// Compare against real paths too: a symlinked parent (including another
	// managed dotfile) can place a lexically harmless target inside the repository.
	realRoot := resolveExisting(c.Root)
	for i, d := range c.Dotfiles {
		if _, err := os.Stat(d.SourcePath); err != nil {
			errs = append(errs, fmt.Errorf("dotfile %s: source %s: %w", d.Target, d.Source, unwrapPathError(err)))
		}
		if !within(d.TargetPath, c.Home) || d.TargetPath == c.Home {
			errs = append(errs, fmt.Errorf("dotfile %s: target must be inside %s", d.Target, c.Home))
		}
		// The target itself may already be our symlink, so only its parent is resolved.
		realTarget := filepath.Join(resolveExisting(filepath.Dir(d.TargetPath)), filepath.Base(d.TargetPath))
		if within(d.TargetPath, c.Root) || within(c.Root, d.TargetPath) ||
			within(realTarget, realRoot) || within(realRoot, realTarget) {
			errs = append(errs, fmt.Errorf("dotfile %s: target must not overlap the repository %s", d.Target, c.Root))
		}
		// Linking would move the source aside and point the target at itself.
		if within(d.SourcePath, d.TargetPath) || within(resolveExisting(d.SourcePath), realTarget) {
			errs = append(errs, fmt.Errorf("dotfile %s: source %s must not be the target or lie inside it", d.Target, d.Source))
		}
		for _, o := range c.Dotfiles[:i] {
			if within(d.TargetPath, o.TargetPath) || within(o.TargetPath, d.TargetPath) {
				errs = append(errs, fmt.Errorf("dotfile %s: target is nested with %s; links would be created through each other", d.Target, o.Target))
			}
		}
	}

	units := map[string]string{}
	for _, s := range c.Services {
		// systemd treats "foo" and "foo.service" as the same unit.
		unit := s.Scope + ":" + strings.TrimSuffix(s.Name, ".service")
		if other, ok := units[unit]; ok {
			errs = append(errs, fmt.Errorf("service %s: same unit as %s; use one name", s.Name, other))
		}
		units[unit] = s.Name
		if !serviceRe.MatchString(s.Name) {
			errs = append(errs, fmt.Errorf("service %q: invalid name", s.Name))
		}
		if s.Scope != ScopeUser && s.Scope != ScopeSystem {
			errs = append(errs, fmt.Errorf("service %s: scope must be %q or %q", s.Name, ScopeUser, ScopeSystem))
		}
		if s.State != StateActive && s.State != StateStopped {
			errs = append(errs, fmt.Errorf("service %s: state must be %q or %q", s.Name, StateActive, StateStopped))
		}
	}

	for _, s := range c.Scripts {
		if !scriptRe.MatchString(s.Name) {
			errs = append(errs, fmt.Errorf("script %q: invalid name", s.Name))
		}
		if strings.TrimSpace(s.Run) == "" {
			errs = append(errs, fmt.Errorf("script %s: run is required", s.Name))
		}
		if s.Phase != PhaseBefore && s.Phase != PhaseAfter {
			errs = append(errs, fmt.Errorf("script %s: phase must be %q or %q", s.Name, PhaseBefore, PhaseAfter))
		}
	}
	return errors.Join(errs...)
}

func validateTemplates(pm PackageManager) error {
	var errs []error
	if pm.List == "" && !strings.Contains(pm.Check, "{package}") {
		errs = append(errs, errors.New("check must contain {package} (or define list)"))
	}
	for name, tmpl := range map[string]string{"install": pm.Install, "remove": pm.Remove} {
		if !strings.Contains(tmpl, "{package}") && !strings.Contains(tmpl, "{packages}") {
			errs = append(errs, fmt.Errorf("%s must contain {package} or {packages}", name))
		}
	}
	return errors.Join(errs...)
}

// TargetContaining returns the dotfile whose target is p or one of its parents,
// comparing real paths too, like the repository overlap check in validate.
func (c *Config) TargetContaining(p string) (Dotfile, bool) {
	realP := resolveExisting(p)
	for _, d := range c.Dotfiles {
		realTarget := filepath.Join(resolveExisting(filepath.Dir(d.TargetPath)), filepath.Base(d.TargetPath))
		if within(p, d.TargetPath) || within(realP, realTarget) {
			return d, true
		}
	}
	return Dotfile{}, false
}

// within reports whether p is dir or lies below it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// appends the remainder unchanged.
func resolveExisting(p string) string {
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func unwrapPathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
