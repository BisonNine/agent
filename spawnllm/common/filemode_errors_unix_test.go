//go:build unix

package common

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestMkdirAllExact_ExistingFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllExact(file, 0o700); !errors.Is(err, errNotDir) {
		t.Fatalf("MkdirAllExact on a file: err = %v, want errNotDir", err)
	}
}

// lockDir gives dir mode for the rest of the test and restores it so the
// temporary directory can be removed.
func lockDir(t *testing.T, dir string, mode fs.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:errcheck // best effort
}

func TestMkdirAllExact_ParentNotWritable(t *testing.T) {
	base := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	lockDir(t, base, 0o500)
	err := MkdirAllExact(filepath.Join(base, "a", "b"), 0o700)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want a permission error", err)
	}
	if _, serr := os.Stat(filepath.Join(base, "a")); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("a folder was created under a read-only parent: %v", serr)
	}
}

func TestMkdirAllExact_ParentNotSearchable(t *testing.T) {
	base := filepath.Join(t.TempDir(), "noexec")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	lockDir(t, base, 0o600)
	if err := MkdirAllExact(filepath.Join(base, "a"), 0o700); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want the stat permission error", err)
	}
}

func TestOpenAppendExact_Directory(t *testing.T) {
	f, err := OpenAppendExact(t.TempDir(), 0o600)
	if err == nil {
		_ = f.Close() //nolint:errcheck // unexpected success
		t.Fatal("OpenAppendExact on a directory: want an error, got nil")
	}
}
