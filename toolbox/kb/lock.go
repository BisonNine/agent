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
	"slices"
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

// bundlePaths checks a bundle's paths and returns its confining directory
// and the root relative to it ("." when they are one, or when there is no
// Confine).
func bundlePaths(b Bundle) (confine, rel string, err error) {
	if !filepath.IsAbs(b.Root) {
		return "", "", errors.New("the root is not an absolute path")
	}
	root := filepath.Clean(b.Root)
	if b.Confine == "" {
		return root, ".", nil
	}
	if !filepath.IsAbs(b.Confine) {
		return "", "", errors.New("the confining directory is not an absolute path")
	}
	confine = filepath.Clean(b.Confine)
	rel, err = filepath.Rel(confine, root)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return "", "", errors.New("the root is not inside the confining directory")
	}
	return confine, rel, nil
}

// unusable logs why a bundle's directory cannot be used, with the host's
// paths, and returns an error for the caller that names only the bundle:
// errors may reach a model, and host paths are not its business.
func (s *Store) unusable(b Bundle, err error) error {
	s.log.Warn("knowledge base directory cannot be used",
		"kb", b.name, "root", b.Root, "confine", b.Confine, "error", err.Error())
	return fmt.Errorf("%w: %s: its directory cannot be used", ErrUnknownKB, quote(b.name))
}

// openRoot opens a bundle root as an os.Root without creating anything. A
// missing root is errNoRoot.
func (s *Store) openRoot(b Bundle) (*os.Root, error) {
	r, _, err := s.rootAt(b, false, 0)
	return r, err
}

// createRoot opens a bundle root, creating the missing directories with
// dirMode. undo removes the directories it created, for a write that then
// fails.
func (s *Store) createRoot(b Bundle, dirMode fs.FileMode) (r *os.Root, undo func(), err error) {
	r, created, err := s.rootAt(b, true, dirMode)
	return r, func() { s.removeDirs(created) }, err
}

// rootAt opens a bundle root, through its Confine when it has one. From
// Confine down, every directory to the root is checked to be a directory
// and not a symbolic link, and is opened as the directory checked, so the
// root cannot be another bundle or anything outside Confine. With create,
// missing directories are made (with dirMode, whatever the umask); created
// lists them, deepest last. Every failure but a missing root is logged and
// returned as unusable.
func (s *Store) rootAt(b Bundle, create bool, dirMode fs.FileMode) (*os.Root, []string, error) {
	confine, rel, err := bundlePaths(b)
	if err != nil {
		return nil, nil, s.unusable(b, err)
	}
	var created []string
	fail := func(err error) (*os.Root, []string, error) {
		if create {
			s.removeDirs(created)
		}
		if errors.Is(err, errNoRoot) {
			return nil, nil, err
		}
		return nil, nil, s.unusable(b, err)
	}
	if create {
		if created, err = ensureDir(confine, dirMode); err != nil {
			return fail(err)
		}
	}
	cur, err := os.OpenRoot(confine)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(errNoRoot)
	}
	if err != nil {
		return fail(err)
	}
	if rel == "." {
		return cur, created, nil
	}
	path := confine
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		next, made, err := s.openChild(cur, part, create, dirMode)
		closeQuietly(s.log, cur)
		if made {
			created = append(created, path)
		}
		if err != nil {
			return fail(err)
		}
		cur = next
	}
	return cur, created, nil
}

// openChild opens the directory name in parent, refusing a symbolic link or
// anything but a directory, and checking that what it opened is what it
// checked. With create, a missing directory is made; made reports that.
func (s *Store) openChild(parent *os.Root, name string, create bool, dirMode fs.FileMode) (*os.Root, bool, error) {
	made := false
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, false, errNoRoot
		}
		if err = parent.Mkdir(name, dirMode); err == nil {
			made = true
			err = parent.Chmod(name, dirMode)
		} else if errors.Is(err, fs.ErrExist) {
			err = nil // made by a concurrent put
		}
		if err == nil {
			info, err = parent.Lstat(name)
		}
	}
	if err != nil {
		return nil, made, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, made, fmt.Errorf("%s is a symbolic link", name)
	}
	if !info.IsDir() {
		return nil, made, fmt.Errorf("%s is not a directory", name)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, made, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		closeQuietly(s.log, child)
		return nil, made, fmt.Errorf("%s changed while it was opened", name)
	}
	return child, made, nil
}

// removeDirs removes directories a failed write created, deepest first.
// Only empty ones go: anything another writer put there stays.
func (s *Store) removeDirs(dirs []string) {
	for _, d := range slices.Backward(dirs) {
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Debug("directory not removed", "dir", d, "error", err.Error())
		}
	}
}

// ensureDir creates dir and its missing parents, setting each one it
// created to perm (chmod, so the umask does not alter it), and returns
// those it created, deepest last. An existing directory is left as it is.
func ensureDir(dir string, perm fs.FileMode) ([]string, error) {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", dir)
		}
		return nil, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	created := make([]string, 0, 1)
	if parent := filepath.Dir(dir); parent != dir {
		if created, err = ensureDir(parent, perm); err != nil {
			return created, err
		}
	}
	if err := os.Mkdir(dir, perm); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return created, err
		}
		return created, nil // made by a concurrent put
	}
	return append(created, dir), os.Chmod(dir, perm)
}

// mkdirAllIn creates the directory rel (slash-separated) and its missing
// parents inside r, setting each one it created to perm, and returns
// those it created, deepest last.
func mkdirAllIn(r *os.Root, rel string, perm fs.FileMode) ([]string, error) {
	var created []string
	parts := strings.Split(rel, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		info, err := r.Lstat(filepath.FromSlash(p))
		if err == nil {
			if !info.IsDir() {
				return created, fmt.Errorf("%s is not a directory", p)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return created, err
		}
		if err := r.Mkdir(filepath.FromSlash(p), perm); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // made by a concurrent put
			}
			return created, err
		}
		created = append(created, p)
		if err := r.Chmod(filepath.FromSlash(p), perm); err != nil {
			return created, err
		}
	}
	return created, nil
}

// removeDirsIn removes directories inside r that a failed write created,
// deepest first; only empty ones go.
func (s *Store) removeDirsIn(r *os.Root, dirs []string) {
	for _, d := range slices.Backward(dirs) {
		if err := r.Remove(filepath.FromSlash(d)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Debug("directory not removed", "dir", d, "error", err.Error())
		}
	}
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
