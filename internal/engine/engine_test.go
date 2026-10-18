package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klahr/pose/internal/config"
	"github.com/klahr/pose/internal/providers"
	"github.com/klahr/pose/internal/state"
)

// fakeServices is an in-memory init system keyed by "<scope>:<name>".
type fakeServices struct {
	units     map[string]*providers.ServiceStatus
	pruned    []string
	failPrune bool
}

func (f *fakeServices) unit(name, scope string) *providers.ServiceStatus {
	u, ok := f.units[scope+":"+name]
	if !ok {
		u = &providers.ServiceStatus{}
		f.units[scope+":"+name] = u
	}
	return u
}

func (f *fakeServices) Status(_ context.Context, name, scope string) (providers.ServiceStatus, error) {
	return *f.unit(name, scope), nil
}

func (f *fakeServices) Reload(context.Context, string) error { return nil }

func (f *fakeServices) Apply(_ context.Context, name, scope string, active, enabled bool) error {
	u := f.unit(name, scope)
	u.Active = active
	if !u.Static {
		u.Enabled = enabled
	}
	return nil
}

func (f *fakeServices) Prune(ctx context.Context, name, scope string) error {
	if f.failPrune {
		return errors.New("prune failed")
	}
	f.pruned = append(f.pruned, scope+":"+name)
	return f.Apply(ctx, name, scope, false, false)
}

type harness struct {
	t                     *testing.T
	home, root, db, state string
	svc                   *fakeServices
	remove                string // remove template for the fake package manager
}

func newHarness(t *testing.T) *harness {
	home := t.TempDir()
	t.Setenv("HOME", home)
	providers.Output = io.Discard
	h := &harness{
		t: t, home: home, root: filepath.Join(home, "repo"), db: t.TempDir(),
		state: filepath.Join(t.TempDir(), "state.json"),
		svc:   &fakeServices{units: map[string]*providers.ServiceStatus{}},
	}
	h.remove = "cd " + h.db + " && rm {packages}"
	return h
}

func (h *harness) write(path, body string) {
	h.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(h.root, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// config writes pose.yaml with a file-backed "fake" package manager prepended.
func (h *harness) config(body string) {
	h.write("pose.yaml", `providers:
  package_managers:
    fake:
      check: "test -f `+h.db+`/{package}"
      install: "cd `+h.db+` && touch {packages}"
      remove: "`+h.remove+`"
`+body)
}

func (h *harness) installed(pkg string) bool {
	_, err := os.Stat(filepath.Join(h.db, pkg))
	return err == nil
}

// run plans and, with apply set, applies; it returns the plan and the apply error.
func (h *harness) run(apply bool) (*Plan, error) {
	h.t.Helper()
	cfg, err := config.Load(h.root, "host")
	if err != nil {
		h.t.Fatal(err)
	}
	store, err := state.Open(h.state, true)
	if err != nil {
		h.t.Fatal(err)
	}
	defer store.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	defer devnull.Close()
	e, err := New(cfg, store, NewUI(devnull, devnull))
	if err != nil {
		h.t.Fatal(err)
	}
	e.services = h.svc
	p, err := e.Plan(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	if !apply {
		return p, nil
	}
	return p, e.Apply(context.Background(), p)
}

func (h *harness) mustApply() *Plan {
	h.t.Helper()
	p, err := h.run(true)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) stateResource(key string) (state.Resource, bool) {
	h.t.Helper()
	s, err := state.Open(h.state, false)
	if err != nil {
		h.t.Fatal(err)
	}
	r, ok := s.State.Resources[key]
	return r, ok
}

func kinds(p *Plan) map[string]Kind {
	m := map[string]Kind{}
	for _, a := range p.Actions {
		m[a.Key] = a.Kind
	}
	return m
}

func TestAdoptedPackageIsNotPruned(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.db, "pre"), "")
	h.config("packages:\n  fake: [pre, fresh]\n")
	p := h.mustApply()
	if k := kinds(p); k["package:fake:pre"] != Adopt || k["package:fake:fresh"] != Install {
		t.Fatalf("first plan = %v", k)
	}
	if r, _ := h.stateResource("package:fake:pre"); !r.Adopted {
		t.Error("pre-existing package not marked adopted")
	}

	h.config("")
	p = h.mustApply()
	if k := kinds(p); k["package:fake:pre"] != Forget || k["package:fake:fresh"] != Prune {
		t.Fatalf("prune plan = %v", k)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], `"pre"`) {
		t.Errorf("warnings = %q", p.Warnings)
	}
	if !h.installed("pre") || h.installed("fresh") {
		t.Errorf("pre installed = %v, fresh installed = %v", h.installed("pre"), h.installed("fresh"))
	}
}

func TestPruneFailureDoesNotBlockInstalls(t *testing.T) {
	h := newHarness(t)
	h.config("packages:\n  fake: [old]\n")
	h.mustApply()

	h.remove = "exit 3 # {packages}"
	h.write("files/a", "a")
	h.config("packages:\n  fake: [new]\ndotfiles:\n  - {source: ./files/a, target: ~/.a}\n")
	if _, err := h.run(true); err == nil || !strings.Contains(err.Error(), "uninstalling fake") {
		t.Fatalf("apply error = %v", err)
	}
	if !h.installed("new") {
		t.Error("install was blocked by the failed prune")
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".a")); err != nil {
		t.Error("link was blocked by the failed prune")
	}
	if _, ok := h.stateResource("package:fake:old"); !ok {
		t.Error("failed prune dropped the package from state")
	}
}

func TestDotfileEditWithoutHookIsNoChange(t *testing.T) {
	h := newHarness(t)
	h.write("files/a", "1")
	h.config("dotfiles:\n  - {source: ./files/a, target: ~/.a}\n")
	h.mustApply()
	h.write("files/a", "2")
	if p, _ := h.run(false); p.HasChanges() {
		t.Errorf("edit without on_change planned changes: %v", kinds(p))
	}
}

func TestFailedHookIsRetried(t *testing.T) {
	h := newHarness(t)
	h.write("files/a", "1")
	h.config("dotfiles:\n  - {source: ./files/a, target: ~/.a, on_change: test -f ok}\n")
	if _, err := h.run(true); err == nil {
		t.Fatal("hook should have failed")
	}
	p, _ := h.run(false)
	if k := kinds(p)["dotfile:~/.a"]; k != Update || !p.Actions[0].Hook {
		t.Fatalf("after failed hook: kind %v, actions %+v", k, p.Actions)
	}
	h.write("ok", "")
	h.mustApply()
	if p, _ := h.run(false); p.HasChanges() {
		t.Errorf("still changes after successful hook: %v", kinds(p))
	}

	h.write("files/a", "2")
	if p, _ := h.run(false); kinds(p)["dotfile:~/.a"] != Update || !p.Actions[0].Hook {
		t.Errorf("edit with on_change did not trigger hook: %+v", p.Actions)
	}
}

func TestBackupRestoredOnPrune(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(h.home, ".a")
	h.write(target, "mine")
	h.write("files/a", "repo")
	h.config("dotfiles:\n  - {source: ./files/a, target: ~/.a, on_change: \"true\"}\n")
	h.mustApply()
	if r, _ := h.stateResource("dotfile:~/.a"); r.Backup == "" {
		t.Fatal("backup not recorded in state (or lost when the hook ran)")
	}

	h.config("")
	h.mustApply()
	if b, err := os.ReadFile(target); err != nil || string(b) != "mine" {
		t.Errorf("target after prune = %q, %v", b, err)
	}
}

func TestStaticServiceConverges(t *testing.T) {
	h := newHarness(t)
	h.svc.units["user:foo"] = &providers.ServiceStatus{Exists: true, Static: true}
	h.config("services:\n  - {name: foo}\n") // wants enabled, which a static unit cannot be
	h.mustApply()
	if p, _ := h.run(false); p.HasChanges() {
		t.Errorf("static unit never converges: %v", kinds(p))
	}
}

func TestAdoptedServiceIsNotPruned(t *testing.T) {
	h := newHarness(t)
	h.svc.units["user:pre"] = &providers.ServiceStatus{Exists: true, Active: true, Enabled: true}
	h.svc.units["user:mine"] = &providers.ServiceStatus{Exists: true}
	h.config("services:\n  - {name: pre}\n  - {name: mine}\n")
	h.mustApply()

	h.config("")
	p := h.mustApply()
	if k := kinds(p); k["service:user:pre"] != Forget || k["service:user:mine"] != Prune {
		t.Fatalf("prune plan = %v", k)
	}
	if len(h.svc.pruned) != 1 || h.svc.pruned[0] != "user:mine" {
		t.Errorf("pruned = %v", h.svc.pruned)
	}
}

func TestServicePruneFailureIsReportedAndRetried(t *testing.T) {
	h := newHarness(t)
	h.config("services:\n  - {name: foo}\n")
	h.mustApply()

	h.svc.failPrune = true
	h.config("")
	if _, err := h.run(true); err == nil {
		t.Fatal("expected prune error")
	}
	if _, ok := h.stateResource("service:user:foo"); !ok {
		t.Error("failed prune dropped the service from state")
	}
}
