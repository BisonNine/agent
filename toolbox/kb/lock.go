/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// keyedMutex serialises work per key. Entries live only while held or
// awaited.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// lock locks key and returns its unlock.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyLock)
	}
	l := k.m[key]
	if l == nil {
		l = &keyLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// errNoRoot: the bundle root does not exist.
var errNoRoot = errors.New("bundle root does not exist")

// bundlePaths checks a bundle's paths and returns its root, its confining
// directory and the root relative to it ("." when they are one, or when
// there is no Confine).
func bundlePaths(b Bundle) (root, confine, rel string, err error) {
	if !filepath.IsAbs(b.Root) {
		return "", "", "", fmt.Errorf("kb: bundle %q: root %q is not an absolute path", b.Name, b.Root)
	}
	root = filepath.Clean(b.Root)
	if b.Confine == "" {
		return root, root, ".", nil
	}
	if !filepath.IsAbs(b.Confine) {
		return "", "", "", fmt.Errorf("kb: bundle %q: confine %q is not an absolute path", b.Name, b.Confine)
	}
	confine = filepath.Clean(b.Confine)
	rel, err = filepath.Rel(confine, root)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return "", "", "", fmt.Errorf("kb: bundle %q: root %q is not inside %q", b.Name, b.Root, b.Confine)
	}
	return root, confine, rel, nil
}

// openRoot opens a bundle root as an os.Root, through its Confine when it
// has one. With create, missing directories are made with dirMode; without
// it a missing root is errNoRoot.
func (s *Store) openRoot(b Bundle, create bool, dirMode fs.FileMode) (*os.Root, error) {
	_, confine, rel, err := bundlePaths(b)
	if err != nil {
		return nil, err
	}
	if create {
		if mkErr := ensureDir(confine, dirMode); mkErr != nil {
			return nil, fmt.Errorf("kb: create bundle %q: %w", b.Name, mkErr)
		}
	}
	top, err := os.OpenRoot(confine)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoRoot
		}
		return nil, fmt.Errorf("kb: open bundle %q: %w", b.Name, err)
	}
	if rel == "." {
		return top, nil
	}
	defer closeQuietly(s.log, top)
	if create {
		if mkErr := mkdirAllIn(top, rel, dirMode); mkErr != nil {
			return nil, fmt.Errorf("kb: create bundle %q: %w", b.Name, mkErr)
		}
	}
	r, err := top.OpenRoot(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoRoot
		}
		return nil, fmt.Errorf("kb: open bundle %q: %w", b.Name, err)
	}
	return r, nil
}

// ensureDir creates dir and its missing parents, setting each one it
// created to perm (chmod, so the umask does not alter it). An existing
// directory is left as it is.
func ensureDir(dir string, perm fs.FileMode) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := ensureDir(parent, perm); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Chmod(dir, perm)
}

// mkdirAllIn creates the directory rel (slash-separated) and its missing
// parents inside r, setting each one it created to perm.
func mkdirAllIn(r *os.Root, rel string, perm fs.FileMode) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := range parts {
		p := filepath.FromSlash(strings.Join(parts[:i+1], "/"))
		info, err := r.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", strings.Join(parts[:i+1], "/"))
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.Mkdir(p, perm); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // made by a concurrent put
			}
			return err
		}
		if err := r.Chmod(p, perm); err != nil {
			return err
		}
	}
	return nil
}

// closeQuietly closes a handle only read from, or one being discarded: its
// close error carries nothing to act on and is logged at debug level.
func closeQuietly(log *slog.Logger, c io.Closer) {
	if err := c.Close(); err != nil {
		log.Debug("close failed", "error", err.Error())
	}
}

// bundleKey identifies a bundle in the index and the caches: its root.
func bundleKey(b Bundle) string { return filepath.Clean(b.Root) }
