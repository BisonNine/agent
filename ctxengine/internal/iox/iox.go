/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package iox holds small I/O helpers shared by ctxengine's packages.
package iox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// orDiscard returns log, or a logger that discards when it is nil.
func orDiscard(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return log
}

// CloseQuietly closes c and logs at debug level on log (the caller's logger,
// tagged with its component; nil discards) if Close reports an error. It is
// for handles whose close error carries nothing the caller can act on:
// read-only database connections, row iterators, files opened for reading,
// and handles being discarded on an error path. A handle that was written to
// must have its Close error checked by the caller instead, because that is
// where a flush failure surfaces.
func CloseQuietly(log *slog.Logger, c io.Closer) {
	if err := c.Close(); err != nil {
		orDiscard(log).Debug("close failed", "error", err.Error())
	}
}

// EnsureDir creates dir and any missing parents and sets each directory it
// created to perm with chmod, so the umask does not alter it. A directory that
// already exists is left as it is: it may be the host's. An existing dir that
// is not a directory is an error.
func EnsureDir(dir string, perm os.FileMode) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err == nil {
			if d == dir && !info.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat directory: %w", err)
		}
		missing = append(missing, d)
		if parent := filepath.Dir(d); parent == d {
			break
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	for _, d := range missing {
		if err := os.Chmod(d, perm); err != nil {
			return fmt.Errorf("set directory mode: %w", err)
		}
	}
	return nil
}

// SetFileMode sets the existing regular file at path to perm when its
// permission bits differ, whether that tightens or loosens them. A missing
// file is ignored. A symbolic link is not followed, since its target is not
// ctxengine's to change; it is left alone and logged at debug level. Other
// failures are logged as warnings and otherwise ignored. log is the caller's
// logger, tagged with its component; nil discards.
func SetFileMode(log *slog.Logger, path string, perm os.FileMode) {
	log = orDiscard(log)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Warn("file mode check failed",
			"path", path, "error", err.Error())
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		log.Debug("file is a symbolic link; mode not changed",
			"path", path)
		return
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() == perm {
		return
	}
	if err := os.Chmod(path, perm); err != nil {
		log.Warn("file mode change failed",
			"path", path, "mode", fmt.Sprintf("%#o", perm), "error", err.Error())
	}
}
