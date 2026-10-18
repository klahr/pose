package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAddKeepsCommentsAndSkipsDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pose.yaml")
	orig := `# machine setup
packages:
  pacman: [git, neovim] # editors
dotfiles:
  # shell
  - source: ./files/zshrc
    target: ~/.zshrc
`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	added, _, err := AddPackages(path, "pacman", []string{"git", "ripgrep", "ripgrep"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"ripgrep"}) {
		t.Errorf("added = %v", added)
	}
	if _, _, err := AddPackages(path, "cargo", []string{"bat"}); err != nil {
		t.Fatal(err)
	}
	undo, err := AddDotfile(path, "./files/gitconfig", "~/.gitconfig")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, want := range []string{"# machine setup", "pacman: [git, neovim, ripgrep] # editors", "cargo: [bat]",
		"# shell", "- source: ./files/gitconfig\n    target: ~/.gitconfig\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "gitconfig") {
		t.Errorf("undo left:\n%s", b)
	}
}

func TestAddCreatesFile(t *testing.T) {
	for _, orig := range []string{"", "# empty\n", "-"} {
		path := filepath.Join(t.TempDir(), "hosts", "h.yaml")
		if orig != "-" {
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, []byte(orig), 0o644)
		}
		undo, err := AddDotfile(path, "./files/a", "~/.a")
		if err != nil {
			t.Fatalf("%q: %v", orig, err)
		}
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), "dotfiles:\n  - source: ./files/a\n") {
			t.Errorf("%q: got\n%s", orig, b)
		}
		undo()
		if _, err := os.Stat(path); orig == "-" && err == nil {
			t.Error("undo did not remove the new file")
		}
	}
}
