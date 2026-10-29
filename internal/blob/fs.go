package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FS stores objects as files under a root directory. It is the driver for
// development, tests and deployments that mount a shared volume.
type FS struct {
	root string
}

// NewFS returns a store rooted at dir, creating it when missing.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("blob: fs root is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blob: create root %s: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: resolve root %s: %w", dir, err)
	}
	return &FS{root: abs}, nil
}

// Root returns the absolute root directory.
func (s *FS) Root() string { return s.root }

func (s *FS) pathOf(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

// Put writes to a temporary file next to the target and renames it into
// place, so a concurrent Open sees either the old object or the new one.
func (s *FS) Put(ctx context.Context, key, _ string, r io.Reader, size int64) (err error) {
	target, err := s.pathOf(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("blob: create directory for %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("blob: create temp for %s: %w", key, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	n, err := io.Copy(tmp, r)
	if err != nil {
		return fmt.Errorf("blob: write %s: %w", key, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("blob: write %s: wrote %d bytes, expected %d", key, n, size)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("blob: sync %s: %w", key, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("blob: close %s: %w", key, err)
	}
	if err = os.Chmod(tmp.Name(), 0o640); err != nil {
		return fmt.Errorf("blob: chmod %s: %w", key, err)
	}
	if err = os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("blob: publish %s: %w", key, err)
	}
	return nil
}

// Open returns the object's file.
func (s *FS) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := s.pathOf(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open %s: %w", key, err)
	}
	return f, nil
}
