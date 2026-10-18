package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type LinkStatus int

const (
	LinkMissing   LinkStatus = iota // nothing at target
	LinkCorrect                     // symlink to source
	LinkElsewhere                   // symlink to something else
	LinkCollision                   // regular file or directory
)

type DotfileStatus struct {
	Status LinkStatus
	Dest   string // current symlink destination, for LinkCorrect and LinkElsewhere
}

func InspectDotfile(source, target string) (DotfileStatus, error) {
	fi, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return DotfileStatus{Status: LinkMissing}, nil
	}
	if err != nil {
		return DotfileStatus{}, fmt.Errorf("inspect %s: %w", target, err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return DotfileStatus{Status: LinkCollision}, nil
	}
	dest, err := os.Readlink(target)
	if err != nil {
		return DotfileStatus{}, fmt.Errorf("read link %s: %w", target, err)
	}
	st := DotfileStatus{Status: LinkElsewhere, Dest: dest}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(target), dest)
	}
	if filepath.Clean(dest) == filepath.Clean(source) {
		st.Status = LinkCorrect
	}
	return st, nil
}

// LinkDotfile points target at source. A regular file or directory at target is
// first moved to <target>.backup.<timestamp>; an existing symlink is replaced
// atomically by renaming a fresh symlink over it.
func LinkDotfile(source, target string, now time.Time) (backup string, err error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create parent of %s: %w", target, err)
	}
	fi, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("inspect %s: %w", target, err)
	case fi.Mode()&fs.ModeSymlink == 0:
		if backup, err = backupPath(target, now); err != nil {
			return "", err
		}
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("back up %s: %w", target, err)
		}
	}

	tmp := fmt.Sprintf("%s.pose-%d.tmp", target, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(source, tmp); err != nil {
		return backup, fmt.Errorf("create symlink for %s: %w", target, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return backup, fmt.Errorf("link %s: %w", target, err)
	}
	return backup, nil
}

func backupPath(target string, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.backup.%s", target, now.Format("20060102-150405"))
	p := base
	for i := 1; ; i++ {
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		p = fmt.Sprintf("%s.%d", base, i)
	}
}

// UnlinkDotfile removes target only if it is still a symlink to source. Anything
// else (a file the user put there, a link they repointed) is left untouched.
func UnlinkDotfile(source, target string) (removed bool, err error) {
	st, err := InspectDotfile(source, target)
	if err != nil || st.Status != LinkCorrect {
		return false, err
	}
	if err := os.Remove(target); err != nil {
		return false, fmt.Errorf("remove %s: %w", target, err)
	}
	return true, nil
}

// HashPath returns a SHA-256 over a file, or over a directory tree's names,
// modes, file contents and symlink destinations. Symlinks are not followed
// below the root.
func HashPath(path string) (string, error) {
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	h := sha256.New()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "L %s %s\x00", rel, dest)
		case d.IsDir():
			fmt.Fprintf(h, "D %s %o\x00", rel, info.Mode().Perm())
		case d.Type().IsRegular():
			fmt.Fprintf(h, "F %s %o\x00", rel, info.Mode().Perm())
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
		default:
			fmt.Fprintf(h, "O %s %v\x00", rel, info.Mode().Type())
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RunHook runs an on_change command through the shell in dir.
func RunHook(ctx context.Context, command, dir string) error {
	_, err := runShell(ctx, true, dir, command)
	return err
}
