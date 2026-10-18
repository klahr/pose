package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klahr/pose/internal/config"
)

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"ripgrep": "ripgrep",
		"":        "''",
		"a b":     "'a b'",
		"it's":    `'it'\''s'`,
		"$(x)":    "'$(x)'",
	} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPackageManagerBatchAndCheck(t *testing.T) {
	db := t.TempDir()
	Output = os.Stderr
	pm := NewPackageManager("fake", config.PackageManager{
		Check:   "test -f " + db + "/{package}",
		Install: "cd " + db + " && touch {packages}",
		Remove:  "rm " + db + "/{package}",
	})
	ctx := context.Background()
	if err := pm.Install(ctx, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Remove(ctx, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	for pkg, want := range map[string]bool{"a": false, "b": true} {
		if got, err := pm.IsInstalled(ctx, pkg); err != nil || got != want {
			t.Errorf("IsInstalled(%s) = %v, %v", pkg, got, err)
		}
	}
	err := pm.Remove(ctx, []string{"missing"})
	if ce, ok := err.(*CommandError); !ok || ce.ExitCode != 1 || ce.Stderr == "" {
		t.Errorf("want CommandError with captured stderr, got %#v", err)
	}
}

func TestLinkBackupAndUnlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	target := filepath.Join(dir, "deep", "target")
	os.WriteFile(src, []byte("x"), 0o644)
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, []byte("mine"), 0o644)

	if st, _ := InspectDotfile(src, target); st.Status != LinkCollision {
		t.Fatalf("status = %v, want collision", st.Status)
	}
	backup, err := LinkDotfile(src, target, time.Now())
	if err != nil || backup == "" {
		t.Fatalf("link: %q, %v", backup, err)
	}
	if b, _ := os.ReadFile(backup); string(b) != "mine" {
		t.Errorf("backup content = %q", b)
	}
	if st, _ := InspectDotfile(src, target); st.Status != LinkCorrect {
		t.Fatalf("status = %v, want correct", st.Status)
	}
	// Relinking over an existing symlink must not create another backup.
	if backup, err := LinkDotfile(src, target, time.Now()); err != nil || backup != "" {
		t.Fatalf("relink: %q, %v", backup, err)
	}
	if removed, err := UnlinkDotfile(src, target); !removed || err != nil {
		t.Fatalf("unlink: %v, %v", removed, err)
	}
	// A regular file at target must never be removed by prune.
	os.WriteFile(target, []byte("user"), 0o644)
	if removed, _ := UnlinkDotfile(src, target); removed {
		t.Error("removed a regular file")
	}
}

func TestHashPathDirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	h1, err := HashPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a"), []byte("2"), 0o644)
	h2, _ := HashPath(dir)
	os.Chmod(filepath.Join(dir, "a"), 0o755)
	h3, _ := HashPath(dir)
	if h1 == h2 || h2 == h3 {
		t.Errorf("hash did not change: %s %s %s", h1, h2, h3)
	}
}
