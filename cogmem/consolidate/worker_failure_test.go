// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/cogmem/store"
)

// A message that cannot be stored is reported as not stored, with the error.
func TestObserveFailsOnClosedStore(t *testing.T) {
	s := openStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	stored, err := Observe(context.Background(), s, 1, "user", "Alice is travelling.", 0)
	if err == nil || stored {
		t.Fatalf("Observe = %v, %v; want false and the store error", stored, err)
	}
}

// When the run cannot read what it needs, it fails before asking the model,
// with an error naming the step, and leaves the inbox for the next run.
func TestRunOnceFailsBeforeTheModelOnStoreErrors(t *testing.T) {
	cases := []struct {
		name  string
		fault func(t *testing.T, s *store.Store)
		want  string
	}{
		{
			name: "closed store",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				if err := s.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
			},
			want: "consolidate: acquire lease: ",
		},
		{
			name: "watermark unreadable",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				mustExec(t, s, `DROP TABLE consolidation_state`)
			},
			want: "consolidate: get state: ",
		},
		{
			name: "inbox missing",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				mustExec(t, s, `DROP TABLE inbox`)
			},
			want: "consolidate: inbox bounds: ",
		},
		{
			name: "inbox row unreadable",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				mustExec(t, s, `UPDATE inbox SET created_at='not a time' WHERE seq=2`)
			},
			want: "consolidate: inbox range: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := seedInbox(t, openStore(t), sampleMessages())
			tc.fault(t, s)
			m := &fakeModel{raw: emptyOutput}
			_, err := NewWorker(s, m).RunOnce(context.Background(), params())
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("RunOnce err = %v, want it to start %q", err, tc.want)
			}
			if m.calls() != 0 {
				t.Errorf("model called %d times, want 0", m.calls())
			}
		})
	}
}

func mustExec(t *testing.T, s *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// Plumbing rows written straight into the inbox (by an older host) never
// reach the model: an inbox holding only those is idle.
func TestRunOnceIdleWhenInboxHoldsOnlyPlumbing(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	for seq, role := range map[int64]string{1: "tool", 2: "system"} {
		if err := s.AppendInbox(ctx, s.DB(), seq, role, "plumbing"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	m := &fakeModel{raw: emptyOutput}
	res, err := NewWorker(s, m).RunOnce(ctx, params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "idle" || res.SeqStart != 1 || res.Applied != 0 {
		t.Fatalf("result = %+v, want idle from seq 1", res)
	}
	if m.calls() != 0 {
		t.Fatalf("model called %d times for plumbing only", m.calls())
	}
}

// Dedupe and retention are housekeeping: when they fail the run still
// consolidates, and each failure is logged as a warning naming the run.
func TestRunOnceHousekeepingFailuresDoNotStopTheRun(t *testing.T) {
	logs := installCapture(t)
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	dup := addMemory(t, s, domainID, store.TypeRule, "Run make test after changes.")
	oldEvent := addMemory(t, s, domainID, store.TypeEvent, "Bob checked the build.")
	oldRetired := addMemory(t, s, domainID, store.TypeFact, "Alice used to work remotely.")
	if err := s.RetireMemory(context.Background(), s.DB(), oldRetired.ID, "superseded"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	backdate(t, s, oldEvent.ID, 45)
	backdate(t, s, oldRetired.ID, 120)
	seedInbox(t, s, sampleMessages())
	refuse(t, s, "no_dedupe", "BEFORE UPDATE ON memories WHEN NEW.status='retired'")
	refuse(t, s, "no_purge", "BEFORE DELETE ON memories")

	m := &fakeModel{raw: emptyOutput}
	res, err := NewWorker(s, m, WithLogger(logs.logger()), WithRetention(30, 90)).RunOnce(context.Background(), params())
	if err != nil || res.Status != "ok" {
		t.Fatalf("RunOnce = %+v, %v; want ok", res, err)
	}
	for _, msg := range []string{"dedupe active memories failed", "purge expired events failed", "purge retired memories failed"} {
		e := logs.wait(t, msg)
		if e.level != "warn" || e.fields["id"] != "alice" || e.fields["error"] == "" {
			t.Errorf("log %q = %+v, want a warning naming alice with the error", msg, e)
		}
	}
	present := countMemories(t, s)
	for _, id := range []string{memoryID, dup.ID, oldEvent.ID, oldRetired.ID} {
		if !present[id] {
			t.Errorf("memory %s was deleted although its purge failed", id)
		}
	}
	if got := watermark(t, s).ConsolidatedSeq; got != 2 {
		t.Errorf("watermark = %d, want 2", got)
	}
}

// A per-agent file that is a copy of the engine prompt is ignored with a
// warning, and the model gets the built-in prompt alone.
func TestRunOnceWarnsAboutCopiedPrompt(t *testing.T) {
	logs := installCapture(t)
	s := seedInbox(t, openStore(t), sampleMessages())
	ws := t.TempDir()
	if err := os.WriteFile(PromptPath(ws), []byte(DefaultPrompt()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := params()
	p.Workspace = ws
	m := &fakeModel{raw: emptyOutput}
	if _, err := NewWorker(s, m, WithLogger(logs.logger())).RunOnce(context.Background(), p); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var warned bool
	for _, msg := range logs.messages() {
		if strings.HasPrefix(msg, "warn: Ignoring per-agent consolidation instructions: ") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about the copied prompt; logged: %v", logs.messages())
	}
	if reqs := m.snapshot(); len(reqs) != 1 || reqs[0].System != DefaultPrompt() {
		t.Errorf("system prompt sent was not the built-in one alone")
	}
}

// Applying a batch and advancing its watermark are one transaction: when the
// watermark cannot be saved, nothing of the batch is applied, the failure is
// recorded on the run, and the inbox is kept. The next run then applies the
// batch exactly once.
func TestRunOnceWatermarkFailureAppliesNothing(t *testing.T) {
	s := seedInbox(t, openStore(t), sampleMessages())
	domainID, _ := seedDomain(t, s)
	before := storeSnapshot(t, s)
	refuse(t, s, "no_state_insert", "BEFORE INSERT ON consolidation_state")
	refuse(t, s, "no_state_update", "BEFORE UPDATE ON consolidation_state")
	out := fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"add","domain":%q,"type":"rule","text":"Alice runs gofmt before committing.","confidence":0.95,"evidence":{"seq_start":1,"seq_end":2}}],"conflict_ledger":[]}`, domainID)
	ctx := context.Background()

	res, err := NewWorker(s, &fakeModel{raw: out}).RunOnce(ctx, params())
	if err == nil || !strings.HasPrefix(err.Error(), "consolidate: apply: set watermark: ") {
		t.Fatalf("RunOnce err = %v, want the watermark error", err)
	}
	if res.Status != "error" || res.Applied != 0 {
		t.Errorf("result = %+v, want error with nothing applied", res)
	}
	if after := storeSnapshot(t, s); after != before {
		t.Errorf("store changed by a failed run:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if run := lastRun(t, s); run.Status != "error" || run.OpsApplied != 0 || !strings.HasPrefix(run.Error, "set watermark: ") {
		t.Errorf("run = status %q applied %d error %q, want error, 0, the watermark error", run.Status, run.OpsApplied, run.Error)
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Errorf("inbox = %v, want both messages kept", left)
	}
	if got := watermark(t, s).ConsolidatedSeq; got != 0 {
		t.Errorf("watermark = %d, want 0", got)
	}

	mustExec(t, s, `DROP TRIGGER no_state_insert`)
	mustExec(t, s, `DROP TRIGGER no_state_update`)
	res, err = NewWorker(s, &fakeModel{raw: out}).RunOnce(ctx, params())
	if err != nil || res.Status != "ok" || res.Applied != 1 {
		t.Fatalf("retry = %+v, %v; want ok with 1 applied", res, err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM memories WHERE text='Alice runs gofmt before committing.'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("memory stored %d times, want once", n)
	}
	if got := watermark(t, s).ConsolidatedSeq; got != 2 {
		t.Errorf("watermark after retry = %d, want 2", got)
	}
}

// When the covered inbox rows cannot be deleted the run still stands: the
// watermark advances and the failure is recorded as the run's error.
func TestRunOnceDrainFailureIsRecorded(t *testing.T) {
	s := seedInbox(t, openStore(t), sampleMessages())
	refuse(t, s, "no_drain", "BEFORE DELETE ON inbox")

	res, err := NewWorker(s, &fakeModel{raw: emptyOutput}).RunOnce(context.Background(), params())
	if err != nil || res.Status != "ok" {
		t.Fatalf("RunOnce = %+v, %v; want ok", res, err)
	}
	if got := watermark(t, s).ConsolidatedSeq; got != 2 {
		t.Errorf("watermark = %d, want 2", got)
	}
	run := lastRun(t, s)
	if run.Status != "ok" || !strings.HasPrefix(run.Error, "drain inbox: ") {
		t.Errorf("run = status %q error %q, want ok with the drain error", run.Status, run.Error)
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Errorf("inbox = %v, want the undeleted rows", left)
	}
}

// A debug dump directory that cannot be created does not fail the run.
func TestRunOnceDebugDumpFailureIsIgnored(t *testing.T) {
	s := seedInbox(t, openStore(t), sampleMessages())
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dump := filepath.Join(file, "dump")
	res, err := NewWorker(s, &fakeModel{raw: emptyOutput}, WithDebugDump(dump)).RunOnce(context.Background(), params())
	if err != nil || res.Status != "ok" {
		t.Fatalf("RunOnce = %+v, %v; want ok", res, err)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Errorf("dump directory exists under a file")
	}
}

// A run without a label still takes and releases the lease.
func TestRunOnceWithoutLabelReleasesLease(t *testing.T) {
	s := seedInbox(t, openStore(t), sampleMessages())
	p := params()
	p.ID = ""
	res, err := NewWorker(s, &fakeModel{raw: emptyOutput}).RunOnce(context.Background(), p)
	if err != nil || res.Status != "ok" {
		t.Fatalf("RunOnce = %+v, %v; want ok", res, err)
	}
	ok, err := s.AcquireLease(context.Background(), s.DB(), leaseName, "bob", leaseTTL)
	if err != nil || !ok {
		t.Fatalf("lease after the run: ok=%v err=%v, want it free", ok, err)
	}
}
