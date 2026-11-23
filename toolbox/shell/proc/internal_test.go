//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Failures the public API reaches only through a broken disk or a race,
// driven here through the package's own pieces.

// logged is a slog handler that keeps every message.
type logged struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logged) Enabled(context.Context, slog.Level) bool { return true }
func (l *logged) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}
func (l *logged) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logged) WithGroup(string) slog.Handler      { return l }

func (l *logged) has(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.msgs, msg)
}

func newTestManager(t *testing.T) (*Manager, *logged) {
	t.Helper()
	logs := &logged{}
	m, err := New(Options{Dir: t.TempDir(), Logger: slog.New(logs)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m, logs
}

// A job whose files cannot be created does not start, and leaves no file.
func TestLaunchFailsWhenFilesCannotBeCreated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocked func(m *Manager, id string) string
		spec    Spec
	}{
		{"log", (*Manager).logPath, Spec{Owner: "alice", Command: "true"}},
		{"end directory", (*Manager).cwdPath, Spec{Owner: "alice", Command: "true", TrackDir: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			const id = "j0000abcd"
			blocked := tc.blocked(m, id)
			if err := os.MkdirAll(filepath.Join(blocked, "inner"), 0o700); err != nil {
				t.Fatal(err)
			}
			j := &job{m: m, id: id, done: make(chan struct{}), changed: make(chan struct{})}
			if err := m.launch(j, tc.spec); err == nil {
				t.Fatal("launch succeeded")
			}
		})
	}
}

// Writing to a job's input reports a closed input as the job's end and a
// failed write as an error.
func TestWriteInputFailures(t *testing.T) {
	m, _ := newTestManager(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	j := &job{m: m, input: true, stdin: w, done: make(chan struct{})}
	if err := j.write([]byte("x")); err == nil || errors.Is(err, ErrEnded) || !strings.Contains(err.Error(), "write input") {
		t.Fatalf("write to an input nobody reads = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := j.write([]byte("x")); !errors.Is(err, ErrEnded) {
		t.Fatalf("write to a closed input = %v, want ErrEnded", err)
	}
}

// A record write that fails is logged; once a job's files are removed its
// record is never written again.
func TestSaveRecordFailuresAndRemoved(t *testing.T) {
	m, logs := newTestManager(t)
	j := &job{m: m, id: "j0000abcd", owner: "alice", state: StateExited, done: make(chan struct{})}
	j.removed = true
	m.saveRecord(j)
	if _, err := os.Stat(m.recordPath(j.id)); !os.IsNotExist(err) {
		t.Fatalf("a removed job's record was written: %v", err)
	}
	j.removed = false
	if err := os.MkdirAll(filepath.Join(m.recordPath(j.id)+".tmp", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.saveRecord(j)
	if !logs.has("shell job record not saved") {
		t.Fatal("a failed record write was not logged")
	}
}

// A restored job whose log cannot be looked at is logged and has no output.
func TestRestoreLogUnreadable(t *testing.T) {
	m, logs := newTestManager(t)
	if err := os.WriteFile(filepath.Join(m.dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	j := m.restore(record{ID: "file/j0000abcd", State: StateExited})
	if !logs.has("shell job log not found") || j.out.size != 0 {
		t.Fatalf("restore: size %d, logged %v", j.out.size, logs.msgs)
	}
}

// A process state that is missing is reported as no exit code.
func TestExitStatusWithoutState(t *testing.T) {
	if code, signal := exitStatus(nil); code != -1 || signal != "" {
		t.Fatalf("exitStatus(nil) = %d, %q", code, signal)
	}
}

// An end-directory file that cannot be read gives no end directory.
func TestReadEndDirUnreadable(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "cwd"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readEndDir(f); got != "" {
		t.Fatalf("readEndDir of a closed file = %q", got)
	}
}

// A line longer than maxCarry with no end is not scanned again from its
// start: the next scan begins at the end of the output.
func TestScanLongUnfinishedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test file
	l := newLogFile(path, f, 1<<20)
	if _, err = l.write([]byte("done\n" + strings.Repeat("x", maxCarry+1))); err != nil {
		t.Fatal(err)
	}
	j := &job{out: l}
	line, found, next, err := j.scanLocked(0, regexp.MustCompile("^never$"))
	if err != nil || found || line != "" || next != l.produced {
		t.Fatalf("scan = %q, %v, %d, %v; want next %d", line, found, next, err, l.produced)
	}
}

// readerAtFunc is an io.ReaderAt.
type readerAtFunc func(p []byte, off int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

// The log's failures are errors, never short or corrupted output.
func TestLogFileFailures(t *testing.T) {
	dir := t.TempDir()
	open := func(name string, flag int, content string) *os.File {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, flag, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() }) //nolint:errcheck // test file
		return f
	}

	t.Run("write to a closed file", func(t *testing.T) {
		f := open("closed.log", os.O_RDWR, "")
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := newLogFile(f.Name(), f, 100).write([]byte("x")); err == nil {
			t.Fatal("write succeeded")
		}
	})

	content := strings.Repeat("0123456789", 30)
	compactOver := func(f *os.File, limit int64) *logFile {
		l := newLogFile(f.Name(), f, limit)
		l.size, l.produced = int64(len(content)), int64(len(content))
		return l
	}
	t.Run("compact cannot read", func(t *testing.T) {
		f := open("unreadable.log", os.O_WRONLY, content)
		if err := compactOver(f, 100).compact(); err == nil {
			t.Fatal("compact succeeded")
		}
	})
	t.Run("compact cannot write the tail", func(t *testing.T) {
		f := open("readonly.log", os.O_RDONLY, content)
		if err := compactOver(f, 100).compact(); err == nil {
			t.Fatal("compact succeeded")
		}
	})
	t.Run("compact cannot write the marker", func(t *testing.T) {
		f := open("nomarker.log", os.O_RDONLY, content)
		l := compactOver(f, 0) // no head or tail: only the marker is written
		if err := l.compact(); err == nil {
			t.Fatal("compact succeeded")
		}
	})
	t.Run("compact with nothing to drop", func(t *testing.T) {
		f := open("small.log", os.O_RDWR, "abc")
		l := newLogFile(f.Name(), f, 100)
		l.size, l.produced = 3, 3
		if err := l.compact(); err != nil || l.compacted {
			t.Fatalf("compact = %v, compacted %v", err, l.compacted)
		}
	})

	t.Run("read a missing log", func(t *testing.T) {
		l := newLogFile(filepath.Join(dir, "missing.log"), nil, 100)
		if _, _, err := l.read([]piece{{off: 0, n: 5}}, 0); err == nil {
			t.Fatal("read succeeded")
		}
	})
	t.Run("read a log that is a directory", func(t *testing.T) {
		l := newLogFile(dir, nil, 100)
		if _, _, err := l.read([]piece{{off: 0, n: 50}}, 0); err == nil {
			t.Fatal("read succeeded")
		}
		if _, _, err := l.read([]piece{{off: 0, n: 50}}, 10); err == nil {
			t.Fatal("read of the head succeeded")
		}
		if _, _, err := l.read([]piece{{lit: []byte(strings.Repeat("h", 20))}, {off: 0, n: 50}}, 10); err == nil {
			t.Fatal("read of the tail succeeded")
		}
	})
	t.Run("readRange reports a failed read", func(t *testing.T) {
		boom := errors.New("disk gone")
		r := readerAtFunc(func([]byte, int64) (int, error) { return 0, boom })
		if _, err := readRange(r, []piece{{off: 0, n: 5}}, 0, 5); !errors.Is(err, boom) {
			t.Fatalf("readRange = %v", err)
		}
	})
	t.Run("read keeps head and tail across pieces", func(t *testing.T) {
		f := open("pieces.log", os.O_RDWR, "abcdefghij")
		l := newLogFile(f.Name(), f, 100)
		pieces := []piece{{off: 0, n: 5}, {lit: []byte("[gap]")}, {off: 5, n: 5}}
		got, omitted, err := l.read(pieces, 4)
		want := "a\n[... 11 bytes omitted ...]\nhij"
		if err != nil || string(got) != want || omitted != 11 {
			t.Fatalf("read = %q, %d, %v; want %q", got, omitted, err, want)
		}
		var r io.ReaderAt = f
		if got, err := readRange(r, pieces, 0, 0); err != nil || len(got) != 0 {
			t.Fatalf("readRange of nothing = %q, %v", got, err)
		}
	})
}
