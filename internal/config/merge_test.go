package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestLoadMergesHostOverlay(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("files/a", "a")
	write("files/b", "b")
	write("pose.yaml", `
providers:
  package_managers:
    pacman:
      install: "sudo pacman -S {packages}"
packages:
  pacman: [ripgrep, fzf]
dotfiles:
  - source: ./files/a
    target: ~/.a
    on_change: base
services:
  - name: syncthing
    enabled: false
`)
	write("hosts/thinkpad.yaml", `
packages:
  pacman: [fzf, tmux]
dotfiles:
  - source: files/b
    target: ~/.a
services:
  - name: syncthing
    state: stopped
  - name: syncthing
    scope: system
`)

	cfg, err := Load(root, NormalizeHostname("ThinkPad.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Packages["pacman"], []string{"ripgrep", "fzf", "tmux"}; !reflect.DeepEqual(got, want) {
		t.Errorf("packages = %v, want %v", got, want)
	}
	pm := cfg.Providers.PackageManagers["pacman"]
	if pm.Install != "sudo pacman -S {packages}" || pm.Check == "" {
		t.Errorf("provider not merged per field: %+v", pm)
	}
	if len(cfg.Dotfiles) != 1 || cfg.Dotfiles[0].SourcePath != filepath.Join(root, "files/b") ||
		cfg.Dotfiles[0].TargetPath != filepath.Join(home, ".a") || cfg.Dotfiles[0].OnChange != "" {
		t.Errorf("dotfiles = %+v", cfg.Dotfiles)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("services = %+v", cfg.Services)
	}
	if s := cfg.Services[0]; s.Key() != "user:syncthing" || s.State != StateStopped || s.Enabled {
		t.Errorf("user service = %+v", s)
	}
	if s := cfg.Services[1]; s.Key() != "system:syncthing" || s.State != StateActive || !s.Enabled {
		t.Errorf("system service = %+v", s)
	}
}

func TestExpandContractHome(t *testing.T) {
	for in, want := range map[string]string{"~": "/h", "~/x/y": "/h/x/y", "/abs": "/abs", "rel": "rel"} {
		got, err := ExpandHome(in, "/h")
		if err != nil || got != want {
			t.Errorf("ExpandHome(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ExpandHome("~bob/x", "/h"); err == nil {
		t.Error("~bob should be rejected")
	}
	if got := ContractHome("/h/.config", "/h"); got != "~/.config" {
		t.Errorf("ContractHome = %q", got)
	}
	if got := ContractHome("/home2/x", "/h"); got != "/home2/x" {
		t.Errorf("ContractHome = %q", got)
	}
}

func TestNestedAndSymlinkedTargetsRejected(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(root, "files", "nvim"), 0o755)
	os.WriteFile(filepath.Join(root, "files", "init.lua"), nil, 0o644)

	os.WriteFile(filepath.Join(root, "pose.yaml"), []byte(`
dotfiles:
  - {source: ./files/nvim, target: ~/.config/nvim}
  - {source: ./files/init.lua, target: ~/.config/nvim/init.lua}
`), 0o644)
	if _, err := Load(root, "h"); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("nested targets: err = %v", err)
	}

	// A parent directory that is a symlink into the repository.
	os.MkdirAll(filepath.Join(home, ".config"), 0o755)
	os.Symlink(filepath.Join(root, "files", "nvim"), filepath.Join(home, ".config", "nvim"))
	os.WriteFile(filepath.Join(root, "pose.yaml"), []byte(`
dotfiles:
  - {source: ./files/init.lua, target: ~/.config/nvim/init.lua}
`), 0o644)
	if _, err := Load(root, "h"); err == nil || !strings.Contains(err.Error(), "overlap the repository") {
		t.Errorf("symlinked parent: err = %v", err)
	}
}

func TestSourceInsideTargetRejected(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config", "nvim"), 0o755)
	os.WriteFile(filepath.Join(home, ".bashrc"), nil, 0o644)
	os.WriteFile(filepath.Join(home, ".config", "nvim", "init.lua"), nil, 0o644)

	for _, d := range []string{
		"{source: ~/.bashrc, target: ~/.bashrc}",
		"{source: ~/.config/nvim/init.lua, target: ~/.config/nvim}",
	} {
		os.WriteFile(filepath.Join(root, "pose.yaml"), []byte("dotfiles:\n  - "+d+"\n"), 0o644)
		if _, err := Load(root, "h"); err == nil || !strings.Contains(err.Error(), "must not be the target") {
			t.Errorf("%s: err = %v", d, err)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ yaml, want string }{
		{"packages:\n  pacman: [git]\n---\npackages:\n  pacman: [vim]\n", "only one YAML document"},
		{"services:\n  - {name: foo}\n  - {name: foo.service, state: stopped}\n", "same unit as foo"},
		{"scripts:\n  - {name: a, run: x}\n  - {name: a, run: y}\n", "duplicate script a"},
		{"dotfiles:\n  - {source: ./pose.yaml, target: ~/.x, recursive: true}\n", "recursive requires a directory"},
		{"scripts:\n  - {name: a}\n", "run is required"},
		{"scripts:\n  - {name: a, run: x, phase: during}\n", "phase must be"},
		{"scripts:\n  - {name: 'a b', run: x}\n", "invalid name"},
	} {
		os.WriteFile(filepath.Join(root, "pose.yaml"), []byte(tc.yaml), 0o644)
		if _, err := Load(root, "h"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v", tc.yaml, err)
		}
	}
	for _, ok := range []string{"", "# empty\n", "---\npackages:\n  pacman: [git]\n"} {
		os.WriteFile(filepath.Join(root, "pose.yaml"), []byte(ok), 0o644)
		if _, err := Load(root, "h"); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestTargetContaining(t *testing.T) {
	home := t.TempDir()
	c := &Config{Dotfiles: []Dotfile{{Target: "~/.local/share", TargetPath: filepath.Join(home, ".local", "share")}}}
	if d, ok := c.TargetContaining(filepath.Join(home, ".local", "share", "pose", "state.json")); !ok || d.Target != "~/.local/share" {
		t.Errorf("state below a target: %v, %v", d, ok)
	}
	if _, ok := c.TargetContaining(filepath.Join(home, ".local", "bin", "pose")); ok {
		t.Error("sibling of a target reported as contained")
	}
}

func TestPacmanListMatchesCheck(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "pacman"), []byte(`#!/bin/sh
case "$*" in
  -Qi) printf 'Name            : bash\nProvides        : sh\n\nName            : i3-wm\nProvides        : None\n\nName            : libfoo\nProvides        : libfoo.so=1-64  foo-git=2\n' ;;
  -Sg) printf 'i3\nkde\n' ;;
  "-Sg i3 kde") printf 'i3 i3-wm\nkde dolphin\nkde i3-wm\n' ;;
  *) exit 1 ;;
esac
`), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := exec.Command("/bin/sh", "-c", BuiltinPackageManagers()["pacman"].List).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(out))
	sort.Strings(got)
	if want := "bash foo-git i3 i3-wm libfoo libfoo.so sh"; strings.Join(got, " ") != want {
		t.Errorf("list = %q, want %q", got, want)
	}
}

func TestBuiltinTemplatesAreValidShell(t *testing.T) {
	for name, pm := range BuiltinPackageManagers() {
		for field, tmpl := range map[string]string{"check": pm.Check, "list": pm.List, "explicit": pm.Explicit, "install": pm.Install, "remove": pm.Remove} {
			script := strings.NewReplacer("{packages}", "a b", "{package}", "a").Replace(tmpl)
			if out, err := exec.Command("/bin/sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Errorf("%s.%s: %v: %s", name, field, err, out)
			}
		}
	}
}

func TestIncludesMergeBeforeTheirFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pose.yaml", "packages:\n  pacman: [git]\n")
	write("groups/laptop.yaml", "packages:\n  pacman: [tlp]\nservices:\n  - {name: tlp, scope: system}\n")
	write("hosts/h.yaml", "include: [groups/laptop.yaml]\nservices:\n  - {name: tlp, scope: system, state: stopped}\n")

	cfg, err := Load(root, "h")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Packages["pacman"], []string{"git", "tlp"}; !reflect.DeepEqual(got, want) {
		t.Errorf("packages = %v, want %v", got, want)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].State != StateStopped {
		t.Errorf("overlay did not win over its include: %+v", cfg.Services)
	}
	if want := []string{filepath.Join(root, "groups/laptop.yaml")}; !reflect.DeepEqual(cfg.Included, want) {
		t.Errorf("included = %v", cfg.Included)
	}

	for body, want := range map[string]string{
		"include: [groups/missing.yaml]\n":                      "file not found",
		"include: [groups/laptop.yaml, ./groups/laptop.yaml]\n": "included more than once",
		"include: [groups/nested.yaml]\n":                       "cannot include others",
	} {
		write("groups/nested.yaml", "include: [groups/laptop.yaml]\n")
		write("hosts/h.yaml", body)
		if _, err := Load(root, "h"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v", body, err)
		}
	}
}
