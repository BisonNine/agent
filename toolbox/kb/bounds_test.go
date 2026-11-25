/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Bounds on what one malformed file may cost an operation.
const (
	maxOpBytes    = 64 << 20 // bytes allocated
	maxOpDuration = 5 * time.Second
	maxErrorText  = 8 << 10 // bytes of an error's text
)

// measure runs op and returns the bytes it allocated and how long it took.
func measure(op func()) (uint64, time.Duration) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	op()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, elapsed
}

// malformed are frontmatters built to make a reader do a lot of work.
func malformed(s *Store) map[string]string {
	var bomb strings.Builder
	bomb.WriteString("type: T\nl0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 9; i++ {
		fmt.Fprintf(&bomb, "l%d: &l%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 10), ", "))
	}
	room := s.readCap() - 64
	return map[string]string{
		"recursive list":   "type: T\nv: &a [1, *a]\n",
		"recursive map":    "type: T\nv: &a {k: *a}\n",
		"deep recursion":   "type: T\nv: &a [[[[[[[[[[1, *a]]]]]]]]]]\n",
		"alias bomb":       bomb.String(),
		"long flow list":   "type: T\nv: [" + strings.Repeat("1,", room/2-20) + "1]\n",
		"many problems":    "type: T\n" + strings.Repeat("x: !foo y\n", room/12),
		"duplicate keys":   "type: T\n" + strings.Repeat("dup: 1\n", room/8),
		"deep nesting":     "type: T\nv: " + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + "\n",
		"long key path":    "type: T\n" + strings.Repeat(strings.Repeat("k", 150)+": {", 200) + "z: !foo y" + strings.Repeat("}", 200) + "\n",
		"many sources bad": "type: T\nsources: [" + strings.Repeat("{}, ", room/5) + "{}]\n",
	}
}

func TestMalformedFilesAreCheap(t *testing.T) {
	f := newFixture(t)
	f.put("ok", Frontmatter{"type": "Note"}, "")
	for name, fm := range malformed(f.s) {
		t.Run(name, func(t *testing.T) {
			id := strings.ReplaceAll(name, " ", "-")
			writeFile(t, filepath.Join(f.notes, id+".md"), "---\n"+fm+"---\nbody\n")
			ops := map[string]func() error{
				"get":    func() error { _, err := f.s.Get(f.ctx, f.r, "notes", id); return err },
				"search": func() error { _, err := newStore(t, Options{}).Search(f.ctx, f.r, "notes", "body", 0); return err },
				"verify": func() error { _, err := f.s.Verify(f.ctx, f.r, "notes"); return err },
			}
			for op, run := range ops {
				var err error
				bytes, took := measure(func() { err = run() })
				t.Logf("%s: %d KB, %v", op, bytes>>10, took)
				if bytes > maxOpBytes || took > maxOpDuration {
					t.Errorf("%s allocated %d bytes in %v", op, bytes, took)
				}
				if err != nil && len(err.Error()) > maxErrorText {
					t.Errorf("%s returned %d bytes of error text", op, len(err.Error()))
				}
				if err != nil && op != "get" {
					t.Errorf("%s = %v", op, err)
				}
			}
			fields, err := f.s.Verify(f.ctx, f.r, "notes")
			if err != nil {
				t.Fatal(err)
			}
			var text int
			for _, fe := range fields {
				text += len(fe.Field) + len(fe.Reason)
			}
			if text > maxErrorText {
				t.Errorf("Verify reported %d bytes", text)
			}
			if _, err := f.s.Get(f.ctx, f.r, "notes", id); err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
				t.Errorf("Get = %v", err)
			}
			if err := removeFile(f, id+".md"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecursiveAliasIsRefused(t *testing.T) {
	for _, doc := range []string{"type: T\nv: &a [1, *a]\n", "type: T\nv: &a {k: *a}\n"} {
		_, problems := parseConcept([]byte("---\n" + doc + "---\n"))
		if len(problems) != 1 || !strings.Contains(problems[0].Reason, "alias to a value that contains it") {
			t.Errorf("%q: problems = %+v", doc, problems)
		}
	}
	// A repeated alias that is not recursive is still read.
	c, problems := parseConcept([]byte("---\ntype: T\na: &x [1]\nb: [*x, *x]\n---\n"))
	if b, ok := c.fm["b"].([]any); problems != nil || !ok || len(b) != 2 {
		t.Fatalf("repeated alias: %v %v", c, problems)
	}
}

func TestProblemsAreBounded(t *testing.T) {
	fields := make([]FieldError, 50)
	for i := range fields {
		fields[i] = FieldError{Field: strings.Repeat("f", 1000), Reason: strings.Repeat("r", 1000)}
	}
	e := invalid("kb", "id", fields...)
	if len(e.Fields) != maxProblems || !strings.Contains(e.Fields[maxProblems-1].Reason, "31 more problems") {
		t.Fatalf("fields = %d, last %+v", len(e.Fields), e.Fields[len(e.Fields)-1])
	}
	for _, fe := range e.Fields {
		if len(fe.Field) > maxFieldPath || len(fe.Reason) > maxReason {
			t.Fatalf("field not clipped: %d %d", len(fe.Field), len(fe.Reason))
		}
	}
	if got := clip("ééé", 4); got != "…" {
		t.Fatalf("clip = %q", got)
	}
}
