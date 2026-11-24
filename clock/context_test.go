package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeContextDeadline(t *testing.T) {
	type key struct{}
	cases := []struct {
		name    string
		advance time.Duration
		parent  bool // cancel the parent
		cancel  bool // call the CancelFunc
		want    error
	}{
		{name: "before the deadline", advance: time.Second - time.Nanosecond},
		{name: "at the deadline", advance: time.Second, want: context.DeadlineExceeded},
		{name: "parent cancelled", parent: true, want: context.Canceled},
		{name: "cancelled", cancel: true, want: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake(epoch)
			parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
			defer cancelParent()
			ctx, cancel := f.WithTimeout(parent, time.Second)
			defer cancel()
			child, cancelChild := context.WithCancel(ctx)
			defer cancelChild()
			if d, ok := ctx.Deadline(); !ok || !d.Equal(epoch.Add(time.Second)) {
				t.Fatalf("deadline = %v, %v", d, ok)
			}
			if ctx.Value(key{}) != "v" {
				t.Fatal("value not inherited from the parent")
			}
			f.Advance(tc.advance)
			if tc.parent {
				cancelParent()
			}
			if tc.cancel {
				cancel()
			}
			if tc.want == nil {
				if err := ctx.Err(); err != nil {
					t.Fatalf("ended early: %v", err)
				}
				return
			}
			<-ctx.Done()
			if err := ctx.Err(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			<-child.Done()
			if err := child.Err(); !errors.Is(err, tc.want) {
				t.Fatalf("child err = %v, want %v", err, tc.want)
			}
			if f.Waiters() != 0 {
				t.Fatalf("waiters = %d after the context ended", f.Waiters())
			}
		})
	}
}

func TestFakeContextEndedParentOrPastDeadline(t *testing.T) {
	f := NewFake(epoch)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, stop := f.WithTimeout(ended, time.Hour)
	defer stop()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", ctx.Err())
	}
	past, stopPast := f.WithDeadline(context.Background(), epoch.Add(-time.Second))
	defer stopPast()
	<-past.Done()
	if !errors.Is(past.Err(), context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", past.Err())
	}
}

// context.Cause is context.DeadlineExceeded once the fake deadline passes,
// for the context and for one derived from it, and stays so when the parent
// is cancelled later with a cause of its own; a cancel or a parent's cause
// that came first is reported as such.
func TestFakeContextCause(t *testing.T) {
	f := NewFake(epoch)
	parent, cancelParent := context.WithCancelCause(context.Background())
	defer cancelParent(nil)
	ctx, cancel := f.WithTimeout(parent, time.Second)
	defer cancel()
	child, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("cause before the deadline = %v", cause)
	}
	f.Advance(time.Second)
	<-child.Done()
	cancelParent(errors.New("too late"))
	for name, c := range map[string]context.Context{"ctx": ctx, "child": child} {
		if cause := context.Cause(c); !errors.Is(cause, context.DeadlineExceeded) {
			t.Errorf("%s cause = %v, want context.DeadlineExceeded", name, cause)
		}
	}

	cancelled, stop := f.WithTimeout(context.Background(), time.Hour)
	stop()
	if cause := context.Cause(cancelled); !errors.Is(cause, context.Canceled) {
		t.Errorf("cancelled cause = %v, want context.Canceled", cause)
	}

	why := errors.New("why")
	p, cancelP := context.WithCancelCause(context.Background())
	byParent, stopByParent := f.WithTimeout(p, time.Hour)
	defer stopByParent()
	cancelP(why)
	<-byParent.Done()
	if cause := context.Cause(byParent); !errors.Is(cause, why) {
		t.Errorf("cause after the parent ended = %v, want the parent's", cause)
	}
}

// A deadline the clock has reached ends the context before WithDeadline
// returns, as context.WithDeadline does.
func TestFakeContextPastDeadlineEndsAtOnce(t *testing.T) {
	f := NewFake(epoch)
	for _, d := range []time.Time{epoch, epoch.Add(-time.Second)} {
		ctx, cancel := f.WithDeadline(context.Background(), d)
		if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("deadline %v: err = %v right after WithDeadline, want context.DeadlineExceeded", d, err)
		}
		if cause := context.Cause(ctx); !errors.Is(cause, context.DeadlineExceeded) {
			t.Errorf("deadline %v: cause = %v", d, cause)
		}
		cancel()
	}
	if f.Waiters() != 0 {
		t.Fatalf("waiters = %d, want none for an ended context", f.Waiters())
	}
}

// Deadline reports the parent's deadline when it is earlier, and the parent
// ending at it ends the context.
func TestFakeContextEarlierParentDeadline(t *testing.T) {
	f := NewFake(epoch)
	parent, cancelParent := f.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	ctx, cancel := f.WithTimeout(parent, time.Hour)
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || !d.Equal(epoch.Add(time.Second)) {
		t.Fatalf("deadline = %v, %v; want the parent's %v", d, ok, epoch.Add(time.Second))
	}
	later, cancelLater := f.WithTimeout(parent, time.Millisecond)
	defer cancelLater()
	if d, _ := later.Deadline(); !d.Equal(epoch.Add(time.Millisecond)) {
		t.Fatalf("own earlier deadline = %v, want %v", d, epoch.Add(time.Millisecond))
	}
	f.Advance(time.Second)
	<-ctx.Done()
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// A parent's end shows on the context as soon as the parent's cancel
// returns, as on a context.WithDeadline context: Err, context.Cause and Done
// report it at once, for a standard parent and for a fake one (nested), and
// Deadline and Value are unchanged by it.
func TestFakeContextParentEndShowsAtOnce(t *testing.T) {
	type key struct{}
	why := errors.New("why")
	f := NewFake(epoch)

	check := func(t *testing.T, ctx context.Context, wantErr, wantCause error) {
		t.Helper()
		if err := ctx.Err(); !errors.Is(err, wantErr) {
			t.Errorf("err = %v right after the parent ended, want %v", err, wantErr)
		}
		if cause := context.Cause(ctx); !errors.Is(cause, wantCause) {
			t.Errorf("cause = %v right after the parent ended, want %v", cause, wantCause)
		}
		select {
		case <-ctx.Done():
		default:
			t.Error("Done not closed right after the parent ended")
		}
		if d, ok := ctx.Deadline(); !ok || !d.Equal(epoch.Add(time.Hour)) {
			t.Errorf("deadline = %v, %v; want %v", d, ok, epoch.Add(time.Hour))
		}
		if ctx.Value(key{}) != "v" {
			t.Error("value not inherited from the parent")
		}
	}

	t.Run("standard parent", func(t *testing.T) {
		parent, cancel := context.WithCancelCause(context.WithValue(context.Background(), key{}, "v"))
		ctx, stop := f.WithTimeout(parent, time.Hour)
		defer stop()
		cancel(why)
		check(t, ctx, context.Canceled, why)
	})
	t.Run("fake parent", func(t *testing.T) {
		root, cancelRoot := context.WithCancelCause(context.WithValue(context.Background(), key{}, "v"))
		parent, stopParent := f.WithTimeout(root, 2*time.Hour)
		defer stopParent()
		ctx, stop := f.WithTimeout(parent, time.Hour)
		defer stop()
		cancelRoot(why)
		check(t, ctx, context.Canceled, why)
	})
	t.Run("fake parent cancelled", func(t *testing.T) {
		parent, stopParent := f.WithTimeout(context.WithValue(context.Background(), key{}, "v"), 2*time.Hour)
		ctx, stop := f.WithTimeout(parent, time.Hour)
		defer stop()
		stopParent()
		check(t, ctx, context.Canceled, context.Canceled)
	})
}
