package providers

import (
	"context"
	"os"
	"os/exec"
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

func TestMissingCheckCommandIsAnError(t *testing.T) {
	pm := NewPackageManager("broken", config.PackageManager{Check: "pose-no-such-command {package}"})
	if _, err := pm.IsInstalled(context.Background(), "a"); err == nil {
		t.Error("exit 127 was treated as 'not installed'")
	}
}

func TestListIsCachedUntilMutation(t *testing.T) {
	db := t.TempDir()
	list := filepath.Join(db, "list")
	Output = os.Stderr
	pm := NewPackageManager("fake", config.PackageManager{
		List:    "cat " + list,
		Install: "echo {package} >> " + list,
	})
	ctx := context.Background()
	os.WriteFile(list, []byte("a\n"), 0o644)
	if ok, err := pm.IsInstalled(ctx, "a"); !ok || err != nil {
		t.Fatalf("IsInstalled(a) = %v, %v", ok, err)
	}
	os.WriteFile(list, []byte("a\nb\n"), 0o644)
	if ok, _ := pm.IsInstalled(ctx, "b"); ok {
		t.Error("list was not cached")
	}
	if err := pm.Install(ctx, []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := pm.IsInstalled(ctx, "c"); !ok {
		t.Error("cache not invalidated by install")
	}
}

func TestSystemdWithoutUserBusIsAnError(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not installed")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent")
	if st, err := (systemd{}).Status(context.Background(), "pose-test.service", config.ScopeUser); err == nil {
		t.Errorf("unreachable user bus reported as %+v", st)
	}
}

func TestRestoreBackup(t *testing.T) {
	dir := t.TempDir()
	backup, target := filepath.Join(dir, "b"), filepath.Join(dir, "t")
	os.WriteFile(backup, []byte("old"), 0o644)
	os.WriteFile(target, []byte("new"), 0o644)
	if ok, err := RestoreBackup(backup, target); ok || err != nil {
		t.Fatalf("restored over an existing target: %v, %v", ok, err)
	}
	os.Remove(target)
	if ok, err := RestoreBackup(backup, target); !ok || err != nil {
		t.Fatalf("restore: %v, %v", ok, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Errorf("target = %q", b)
	}
}
