/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package iox

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failCloser reports err from Close.
type failCloser struct{ err error }

func (f failCloser) Close() error { return f.err }

// bufLogger returns a debug-level text logger writing to the returned buffer.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// perm returns the permission bits of path.
func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestCloseQuietly(t *testing.T) {
	log, buf := bufLogger()
	CloseQuietly(log, failCloser{})
	if buf.Len() != 0 {
		t.Fatalf("logged on a clean close: %s", buf)
	}
	CloseQuietly(log, failCloser{err: errors.New("disk gone")})
	if !strings.Contains(buf.String(), "close failed") || !strings.Contains(buf.String(), "disk gone") {
		t.Fatalf("close error not logged: %s", buf)
	}
	// A nil logger discards rather than panicking.
	CloseQuietly(nil, failCloser{err: errors.New("x")})
}

func TestEnsureDirCreatesWithModeAndLeavesExistingAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "a", "b")
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	for _, d := range []string{filepath.Join(root, "a"), dir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %#o, want 0700", d, got)
		}
	}
	// The existing parent is the host's: its mode is untouched.
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("existing root mode = %#o, want 0755", got)
	}

	// An existing directory is left as it is.
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatalf("EnsureDir on existing: %v", err)
	}
	if got := perm(t, dir); got != 0o750 {
		t.Errorf("existing dir mode changed to %#o", got)
	}
}

func TestEnsureDirErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(file, 0o700); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("EnsureDir on a file = %v, want not a directory", err)
	}
	// A path under a regular file cannot be stat'ed as missing.
	if err := EnsureDir(filepath.Join(file, "sub"), 0o700); err == nil {
		t.Fatal("EnsureDir under a file succeeded")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ro := filepath.Join(root, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ro, 0o700); err != nil {
			t.Error(err)
		}
	})
	err := EnsureDir(filepath.Join(ro, "child"), 0o700)
	if err == nil || !strings.Contains(err.Error(), "create directory") {
		t.Fatalf("EnsureDir in a read-only dir = %v, want create directory error", err)
	}
}

func TestSetFileMode(t *testing.T) {
	root := t.TempDir()
	log, buf := bufLogger()

	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	SetFileMode(log, file, 0o600)
	if got := perm(t, file); got != 0o600 {
		t.Fatalf("tighten: mode = %#o, want 0600", got)
	}
	SetFileMode(log, file, 0o640)
	if got := perm(t, file); got != 0o640 {
		t.Fatalf("loosen: mode = %#o, want 0640", got)
	}

	// A missing file is ignored silently.
	SetFileMode(log, filepath.Join(root, "missing"), 0o600)
	if buf.Len() != 0 {
		t.Fatalf("logged for a missing file: %s", buf)
	}

	// A symbolic link is not followed: the target keeps its mode.
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	SetFileMode(log, link, 0o600)
	if got := perm(t, file); got != 0o640 {
		t.Fatalf("link target mode changed to %#o", got)
	}
	if !strings.Contains(buf.String(), "symbolic link") {
		t.Fatalf("symlink not logged: %s", buf)
	}

	// A directory is not a regular file and is left alone.
	sub := filepath.Join(root, "dir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	SetFileMode(nil, sub, 0o700)
	if got := perm(t, sub); got != 0o755 {
		t.Fatalf("directory mode changed to %#o", got)
	}

	// A path that cannot be examined is logged as a warning.
	buf.Reset()
	SetFileMode(log, filepath.Join(file, "under-a-file"), 0o600)
	if !strings.Contains(buf.String(), "file mode check failed") {
		t.Fatalf("lstat failure not logged: %s", buf)
	}
}
