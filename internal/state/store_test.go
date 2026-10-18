package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTripAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pose", "state.json")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Put(PackageKey("pacman", "fzf"), Resource{Type: TypePackage, Manager: "pacman", ID: "fzf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, true); err == nil {
		t.Error("second locked open should fail")
	}

	r, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.State.Keys(); len(got) != 1 || got[0] != "package:pacman:fzf" {
		t.Errorf("keys = %v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %v", fi.Mode().Perm())
	}
}
