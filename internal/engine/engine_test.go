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
	remove                string   // remove template for the fake package manager
	targets               []string // passed to Engine.Target when set
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
	if h.targets != nil {
		if err := e.Target(p, h.targets); err != nil {
			return nil, err
		}
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

func TestMissingManagerDoesNotBlockPrune(t *testing.T) {
	h := newHarness(t)
	h.config("packages:\n  fake: [a]\n")
	h.mustApply()

	h.write("pose.yaml", `providers:
  package_managers:
    fake:
      check: "pose-no-such-command {package}"
      install: "pose-no-such-command {packages}"
      remove: "pose-no-such-command {packages}"
`)
	p := h.mustApply()
	if k := kinds(p)["package:fake:a"]; k != Forget || len(p.Warnings) != 1 {
		t.Fatalf("kind = %v, warnings = %q", k, p.Warnings)
	}
	if _, ok := h.stateResource("package:fake:a"); ok {
		t.Error("package still tracked")
	}

	// A desired package still needs a working manager.
	h.write("pose.yaml", `providers:
  package_managers:
    fake:
      check: "pose-no-such-command {package}"
      install: "pose-no-such-command {packages}"
      remove: "pose-no-such-command {packages}"
packages:
  fake: [a]
`)
	cfg, _ := config.Load(h.root, "host")
	store, _ := state.Open(h.state, false)
	e, _ := New(cfg, store, NewUI(os.Stderr, os.Stderr))
	if _, err := e.Plan(context.Background()); err == nil {
		t.Error("desired package with a missing manager did not fail the plan")
	}
}

func TestMovingPackageBetweenManagersKeepsIt(t *testing.T) {
	h := newHarness(t)
	// "aur" shares the fake manager's database, like pacman and an AUR helper.
	h.config("    aur:\n      check: \"test -f " + h.db + "/{package}\"\n      install: \"cd " + h.db + " && touch {packages}\"\n      remove: \"cd " + h.db + " && rm {packages}\"\npackages:\n  fake: [a]\n")
	h.mustApply()

	h.config("    aur:\n      check: \"test -f " + h.db + "/{package}\"\n      install: \"cd " + h.db + " && touch {packages}\"\n      remove: \"cd " + h.db + " && rm {packages}\"\npackages:\n  aur: [a]\n")
	p := h.mustApply()
	if k := kinds(p); k["package:fake:a"] != Forget || k["package:aur:a"] != Adopt {
		t.Fatalf("move plan = %v", k)
	}
	if !h.installed("a") {
		t.Fatal("moved package was uninstalled")
	}
	if r, _ := h.stateResource("package:aur:a"); r.Adopted {
		t.Error("pose installed the package, but the moved entry is marked adopted")
	}
}

func TestInterruptedPlanReportsOnce(t *testing.T) {
	h := newHarness(t)
	h.config("packages:\n  fake: [a, b, c, d, e, f, g, h, i, j]\n")
	cfg, err := config.Load(h.root, "host")
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(h.state, false)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(cfg, store, NewUI(os.Stderr, os.Stderr))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Plan(ctx); err != context.Canceled {
		t.Errorf("err = %v", err)
	}
}

func TestFailedInstallDoesNotBlockLinksOrServices(t *testing.T) {
	h := newHarness(t)
	h.write("files/a", "a")
	h.write("files/b", "b")
	ro := filepath.Join(h.home, "ro") // ~/ro/.b cannot be created
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	h.svc.units["user:foo"] = &providers.ServiceStatus{Exists: true}
	h.config(`packages:
  fake: [ok, "bad/name"]
dotfiles:
  - {source: ./files/b, target: ~/ro/.b, on_change: touch hook-ran}
  - {source: ./files/a, target: ~/.a}
services:
  - {name: foo}
`)
	_, err := h.run(true)
	if err == nil || !strings.Contains(err.Error(), "installing fake") || !strings.Contains(err.Error(), "ro/.b") {
		t.Fatalf("apply error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".a")); err != nil {
		t.Error("link was blocked by the failed install or link")
	}
	if u := h.svc.units["user:foo"]; !u.Active {
		t.Error("service was blocked by the failed install or link")
	}
	if _, err := os.Stat(filepath.Join(h.root, "hook-ran")); err == nil {
		t.Error("hook ran for a dotfile that failed to link")
	}
	if _, ok := h.stateResource("dotfile:~/ro/.b"); ok {
		t.Error("failed link recorded in state")
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

func TestHomeSpellingDoesNotUnlinkDeclaredDotfile(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(h.home, ".a")
	h.write("files/a", "a")
	h.config("dotfiles:\n  - {source: ./files/a, target: ~/.a}\n")
	h.mustApply()

	t.Setenv("HOME", h.home+"/")
	if p := h.mustApply(); p.HasChanges() {
		t.Errorf("trailing slash on HOME planned changes: %v", kinds(p))
	}

	// State written under an absolute key by an older version.
	s, err := state.Open(h.state, true)
	if err != nil {
		t.Fatal(err)
	}
	r := s.State.Resources["dotfile:~/.a"]
	s.Delete("dotfile:~/.a")
	s.Put("dotfile:"+target, r)
	s.Close()
	if p := h.mustApply(); p.HasChanges() {
		t.Errorf("old key planned changes: %v", kinds(p))
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("declared dotfile was unlinked: %v", err)
	}
}

func TestOriginalBackupSurvivesAppOverwrite(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(h.home, ".a")
	h.write(target, "mine")
	h.write("files/a", "repo")
	h.config("dotfiles:\n  - {source: ./files/a, target: ~/.a}\n")
	h.mustApply()

	// An app saves by writing a new file over the link.
	os.Remove(target)
	h.write(target, "app")
	h.mustApply()

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

func TestPartlySetUpServiceIsNotPruned(t *testing.T) {
	h := newHarness(t)
	h.svc.units["user:pre"] = &providers.ServiceStatus{Exists: true, Active: true} // running, not enabled
	h.config("services:\n  - {name: pre}\n")
	if k := kinds(h.mustApply())["service:user:pre"]; k != Update {
		t.Fatalf("kind = %v", k)
	}

	h.config("")
	if k := kinds(h.mustApply())["service:user:pre"]; k != Forget {
		t.Errorf("service running before pose was planned as %v", k)
	}
	if len(h.svc.pruned) != 0 {
		t.Errorf("pruned = %v", h.svc.pruned)
	}
}

func TestServiceAdoptedWhileStoppedIsPrunedOnceStarted(t *testing.T) {
	h := newHarness(t)
	h.svc.units["user:foo"] = &providers.ServiceStatus{Exists: true}
	h.config("services:\n  - {name: foo, state: stopped}\n")
	h.mustApply()
	h.config("services:\n  - {name: foo}\n")
	h.mustApply()

	h.config("")
	if k := kinds(h.mustApply())["service:user:foo"]; k != Prune {
		t.Errorf("service started by pose was planned as %v", k)
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

func TestScriptsRunInPhaseOrderAndOnlyWhenChanged(t *testing.T) {
	h := newHarness(t)
	h.write("files/a", "a")
	h.config(`dotfiles:
  - {source: ./files/a, target: ~/.a, on_change: echo hook >> log}
scripts:
  - {name: late, run: echo after >> log}
  - {name: early, run: echo before >> log, phase: before}
`)
	p := h.mustApply()
	if k := kinds(p); k["script:early"] != Run || k["script:late"] != Run {
		t.Fatalf("first plan = %v", k)
	}
	if b, _ := os.ReadFile(filepath.Join(h.root, "log")); string(b) != "before\nhook\nafter\n" {
		t.Errorf("log = %q", b)
	}
	if p, _ := h.run(false); p.HasChanges() {
		t.Errorf("unchanged scripts planned again: %v", kinds(p))
	}

	h.config(`scripts:
  - {name: late, run: echo after2 >> log}
`)
	p = h.mustApply()
	if k := kinds(p); k["script:late"] != Run || k["script:early"] != Forget {
		t.Fatalf("second plan = %v", k)
	}
	if b, _ := os.ReadFile(filepath.Join(h.root, "log")); !strings.HasSuffix(string(b), "after\nafter2\n") {
		t.Errorf("log = %q", b)
	}
}

func TestFailedScriptIsRetried(t *testing.T) {
	h := newHarness(t)
	h.config("scripts:\n  - {name: s, run: test -f ok}\n")
	if _, err := h.run(true); err == nil || !strings.Contains(err.Error(), "script s") {
		t.Fatalf("apply error = %v", err)
	}
	if _, ok := h.stateResource("script:s"); ok {
		t.Error("failed script was recorded")
	}
	h.write("ok", "")
	if p := h.mustApply(); kinds(p)["script:s"] != Run {
		t.Errorf("failed script not retried: %v", kinds(p))
	}
}

func TestRecursiveDotfileLinksEachFile(t *testing.T) {
	h := newHarness(t)
	h.write("files/nvim/init.lua", "1")
	h.write("files/nvim/lua/opts.lua", "2")
	h.config("dotfiles:\n  - {source: ./files/nvim, target: ~/.config/nvim, recursive: true}\n")
	p := h.mustApply()
	if k := kinds(p); len(k) != 2 || k["dotfile:~/.config/nvim/init.lua"] != Link || k["dotfile:~/.config/nvim/lua/opts.lua"] != Link {
		t.Fatalf("plan = %v", k)
	}
	if fi, err := os.Lstat(filepath.Join(h.home, ".config/nvim")); err != nil || !fi.IsDir() {
		t.Fatalf("target is not a real directory: %v", err)
	}
	if dest, _ := os.Readlink(filepath.Join(h.home, ".config/nvim/lua/opts.lua")); dest != filepath.Join(h.root, "files/nvim/lua/opts.lua") {
		t.Errorf("link = %q", dest)
	}

	if err := os.Remove(filepath.Join(h.root, "files/nvim/init.lua")); err != nil {
		t.Fatal(err)
	}
	p = h.mustApply()
	if k := kinds(p); len(k) != 1 || k["dotfile:~/.config/nvim/init.lua"] != Prune {
		t.Fatalf("plan after removing a file = %v", k)
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".config/nvim/init.lua")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("link for removed file still there: %v", err)
	}
}

func TestTargetRestrictsPlan(t *testing.T) {
	h := newHarness(t)
	h.write("files/nvim/init.lua", "")
	h.write("files/a", "")
	h.config(`packages:
  fake: [git]
dotfiles:
  - {source: ./files/nvim, target: ~/.config/nvim, recursive: true}
  - {source: ./files/a, target: ~/.config/nvim2}
`)
	h.targets = []string{"dotfile:~/.config/nvim"}
	p := h.mustApply()
	if k := kinds(p); len(k) != 1 || k["dotfile:~/.config/nvim/init.lua"] != Link {
		t.Fatalf("targeted plan = %v", k)
	}
	if h.installed("git") {
		t.Error("untargeted package was installed")
	}

	h.targets = []string{"package:fak"}
	if _, err := h.run(false); err == nil || !strings.Contains(err.Error(), "matches nothing") {
		t.Errorf("partial-word target: err = %v", err)
	}
	h.targets = []string{"package"}
	if p, err := h.run(false); err != nil || len(p.Actions) != 1 || p.Actions[0].Key != "package:fake:git" {
		t.Errorf("type target: %v, %+v", err, p)
	}
}
