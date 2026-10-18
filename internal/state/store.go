package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DefaultPath is $XDG_DATA_HOME/pose/state.json, falling back to ~/.local/share.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine home directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "pose", "state.json"), nil
}

// Store holds the state in memory and writes it back atomically on every change.
type Store struct {
	path  string
	lock  *os.File
	State *State
}

// Open loads the state at path; a missing file yields empty state. With lock set,
// an exclusive lock is taken first so concurrent applies cannot interleave.
func Open(path string, lock bool) (*Store, error) {
	s := &Store{path: path, State: New()}
	if lock {
		if err := s.acquire(); err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		s.Close()
		return nil, fmt.Errorf("corrupt state file %s: %w", path, err)
	}
	if st.Version != Version {
		s.Close()
		return nil, fmt.Errorf("state file %s has unsupported version %d (want %d)", path, st.Version, Version)
	}
	if st.Resources == nil {
		st.Resources = map[string]Resource{}
	}
	s.State = &st
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) acquire() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("another pose process is running (lock held on %s.lock)", s.path)
		}
		return fmt.Errorf("lock state: %w", err)
	}
	s.lock = f
	return nil
}

// Close releases the lock, if held.
func (s *Store) Close() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

func (s *Store) Put(key string, r Resource) error {
	return s.Update(func(st *State) { st.Resources[key] = r })
}

func (s *Store) Delete(key string) error {
	return s.Update(func(st *State) { delete(st.Resources, key) })
}

// Update applies fn and flushes the result to disk.
func (s *Store) Update(fn func(*State)) error {
	fn(s.State)
	return s.Save()
}

func (s *Store) Save() error {
	s.State.Version = Version
	s.State.LastApplied = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, append(data, '\n')); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// writeAtomic writes to a temp file in the same directory, fsyncs it and renames
// it over path, so readers see either the old or the new file, never a mix.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Persist the rename itself; best effort, not every filesystem supports it.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
