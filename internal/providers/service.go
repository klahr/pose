package providers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/klahr/pose/internal/config"
)

type ServiceStatus struct {
	Exists  bool // the init system knows the unit / a plist is installed
	Active  bool
	Enabled bool
	// Static units (no [Install] section, aliases, generated or transient units)
	// cannot be enabled or disabled; their Enabled value is meaningless.
	Static bool
}

// ServiceManager abstracts systemd and launchd. User-scope operations run as the
// current user; system-scope mutations are prefixed with sudo.
type ServiceManager interface {
	Status(ctx context.Context, name, scope string) (ServiceStatus, error)
	// Reload makes the init system pick up changed unit files.
	Reload(ctx context.Context, scope string) error
	// Apply converges the service to the desired state, touching only what differs.
	Apply(ctx context.Context, name, scope string, active, enabled bool) error
	// Prune stops and disables the service.
	Prune(ctx context.Context, name, scope string) error
}

func NewServiceManager() (ServiceManager, error) {
	switch runtime.GOOS {
	case "linux":
		return systemd{}, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return launchd{uid: os.Getuid(), home: home}, nil
	}
	return nil, fmt.Errorf("services are not supported on %s", runtime.GOOS)
}

// systemd

type systemd struct{}

func (systemd) exec(ctx context.Context, scope string, mutate bool, args ...string) (result, error) {
	var argv []string
	switch {
	case scope == config.ScopeUser:
		argv = []string{"systemctl", "--user"}
	case mutate:
		argv = []string{"sudo", "systemctl"}
	default:
		argv = []string{"systemctl"}
	}
	return run(ctx, mutate, "", append(argv, args...)...)
}

func (s systemd) Status(ctx context.Context, name, scope string) (ServiceStatus, error) {
	res, err := s.exec(ctx, scope, false, "is-active", name)
	if err != nil && !exitedNonZero(err) {
		return ServiceStatus{}, err
	}
	st := ServiceStatus{Exists: true, Active: strings.TrimSpace(res.stdout) == "active"}

	res, err = s.exec(ctx, scope, false, "is-enabled", name)
	if err != nil && !exitedNonZero(err) {
		return ServiceStatus{}, err
	}
	// is-enabled also exits 0 for static, indirect, generated, transient and alias.
	out := strings.TrimSpace(res.stdout)
	switch out {
	case "enabled", "enabled-runtime":
		st.Enabled = true
	case "static", "indirect", "generated", "transient", "alias":
		st.Static = true
	}
	if out == "not-found" || strings.Contains(res.stderr, "No such file") || strings.Contains(res.stderr, "not found") {
		st.Exists = false
	}
	return st, nil
}

func (s systemd) Reload(ctx context.Context, scope string) error {
	_, err := s.exec(ctx, scope, true, "daemon-reload")
	return err
}

func (s systemd) Apply(ctx context.Context, name, scope string, active, enabled bool) error {
	st, err := s.Status(ctx, name, scope)
	if err != nil {
		return err
	}
	if enabled != st.Enabled && !st.Static {
		if _, err := s.exec(ctx, scope, true, ternary(enabled, "enable", "disable"), name); err != nil {
			return err
		}
	}
	if active != st.Active {
		if _, err := s.exec(ctx, scope, true, ternary(active, "start", "stop"), name); err != nil {
			return err
		}
	}
	return nil
}

func (s systemd) Prune(ctx context.Context, name, scope string) error {
	return s.Apply(ctx, name, scope, false, false)
}

// launchd. The service name is the job label; its plist is expected in
// ~/Library/LaunchAgents (user) or /Library/LaunchDaemons (system). A job counts
// as active while it is loaded (bootstrapped) in its domain, whether or not a
// process is running right now: on-demand and periodic jobs exit between runs.

type launchd struct {
	uid  int
	home string
}

func (l launchd) domain(scope string) string {
	if scope == config.ScopeSystem {
		return "system"
	}
	return "gui/" + strconv.Itoa(l.uid)
}

func (l launchd) plist(name, scope string) string {
	if scope == config.ScopeSystem {
		return filepath.Join("/Library/LaunchDaemons", name+".plist")
	}
	return filepath.Join(l.home, "Library", "LaunchAgents", name+".plist")
}

func (l launchd) exec(ctx context.Context, scope string, mutate bool, args ...string) (result, error) {
	argv := []string{"launchctl"}
	if mutate && scope == config.ScopeSystem {
		argv = []string{"sudo", "launchctl"}
	}
	return run(ctx, mutate, "", append(argv, args...)...)
}

func (l launchd) status(ctx context.Context, name, scope string) (st ServiceStatus, loaded bool, err error) {
	target := l.domain(scope) + "/" + name
	res, err := l.exec(ctx, scope, false, "print", target)
	if err != nil && !exitedNonZero(err) {
		return st, false, err
	}
	loaded = err == nil
	st.Active = loaded

	res, err = l.exec(ctx, scope, false, "print-disabled", l.domain(scope))
	if err != nil {
		return st, loaded, err
	}
	_, statErr := os.Stat(l.plist(name, scope))
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return st, loaded, statErr
	}
	hasPlist := statErr == nil
	st.Exists = loaded || hasPlist
	st.Enabled = hasPlist && !launchdDisabled(res.stdout, name)
	return st, loaded, nil
}

// launchdDisabled parses `launchctl print-disabled` lines like `"label" => disabled`.
func launchdDisabled(out, name string) bool {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=>\s*(\S+)`)
	m := re.FindStringSubmatch(out)
	return m != nil && (m[1] == "disabled" || m[1] == "true")
}

func (l launchd) Status(ctx context.Context, name, scope string) (ServiceStatus, error) {
	st, _, err := l.status(ctx, name, scope)
	return st, err
}

func (launchd) Reload(context.Context, string) error { return nil }

func (l launchd) Apply(ctx context.Context, name, scope string, active, enabled bool) error {
	st, loaded, err := l.status(ctx, name, scope)
	if err != nil {
		return err
	}
	target := l.domain(scope) + "/" + name
	plist := l.plist(name, scope)
	if (enabled || active) && !st.Exists {
		return fmt.Errorf("launchd job %s: no plist at %s", name, plist)
	}
	if enabled != st.Enabled {
		if _, err := l.exec(ctx, scope, true, ternary(enabled, "enable", "disable"), target); err != nil {
			return err
		}
	}
	switch {
	case active && !loaded:
		if _, err := l.exec(ctx, scope, true, "bootstrap", l.domain(scope), plist); err != nil {
			return err
		}
		if _, err := l.exec(ctx, scope, true, "kickstart", target); err != nil {
			return err
		}
	case !active && loaded:
		if _, err := l.exec(ctx, scope, true, "bootout", target); err != nil {
			return err
		}
	}
	return nil
}

func (l launchd) Prune(ctx context.Context, name, scope string) error {
	return l.Apply(ctx, name, scope, false, false)
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
