package providers

import (
	"context"
	"strings"

	"github.com/klahr/pose/internal/config"
)

type ServiceStatus struct {
	Exists  bool // the init system knows the unit
	Active  bool
	Enabled bool
	// Static units (no [Install] section, aliases, generated or transient units)
	// cannot be enabled or disabled; their Enabled value is meaningless.
	Static bool
	// Runtime is set for units enabled only until reboot (enable --runtime).
	Runtime bool
}

// ServiceManager abstracts the init system. User-scope operations run as the
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
	return systemd{}, nil
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
	// is-active prints a state even for unknown units; no output means systemctl
	// could not ask at all, e.g. without a user session bus over ssh or su.
	if err != nil && (!exitedNonZero(err) || strings.TrimSpace(res.stdout) == "") {
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
	case "enabled":
		st.Enabled = true
	case "enabled-runtime":
		st.Enabled, st.Runtime = true, true
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
		args := []string{ternary(enabled, "enable", "disable"), name}
		if st.Runtime {
			// A plain disable leaves runtime links in place.
			args = []string{"disable", "--runtime", name}
		}
		if _, err := s.exec(ctx, scope, true, args...); err != nil {
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

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
