package config

import (
	"os"
	"path/filepath"
	"reflect"
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
