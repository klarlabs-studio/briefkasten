package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"

	"go.klarlabs.de/fortify/ferrors"
)

// flakyMailbox fails listErr/fetchErr times before succeeding.
type flakyMailbox struct {
	mu        sync.Mutex
	failures  int
	listCalls int
	fetchErr  error
	slow      time.Duration
}

func (f *flakyMailbox) ListUnread(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	f.listCalls++
	slow := f.slow
	f.mu.Unlock()

	// A slow backend honours the context, exactly as domain.Mailbox
	// requires — a fake that slept through it would let the wrapper's
	// timeout look enforced while the caller was still held.
	if slow > 0 {
		timer := time.NewTimer(slow)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("connection reset")
	}
	return []string{"1"}, nil
}

func (f *flakyMailbox) Fetch(context.Context, string) ([]byte, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return []byte("From: a@b\r\n\r\nhi"), nil
}

func (f *flakyMailbox) MarkSeen(context.Context, string) error { return nil }

func (f *flakyMailbox) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func TestResilientRetriesTransientFailure(t *testing.T) {
	mb := &flakyMailbox{failures: 2}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	ids, err := r.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v", ids)
	}
	if got := mb.calls(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestResilientDoesNotRetryBadID(t *testing.T) {
	mb := &flakyMailbox{fetchErr: fmt.Errorf("%w: nope", domain.ErrBadID)}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	_, err := r.Fetch(t.Context(), "nope")
	if !errors.Is(err, domain.ErrBadID) {
		t.Fatalf("err = %v, want domain.ErrBadID", err)
	}
}

func TestResilientCircuitOpensAfterConsecutiveFailures(t *testing.T) {
	mb := &flakyMailbox{failures: 1000}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond, MaxAttempts: 1})

	// Default trip threshold is 5 consecutive failures.
	for i := 0; i < 5; i++ {
		if _, err := r.ListUnread(t.Context()); err == nil {
			t.Fatal("want failure")
		}
	}
	_, err := r.ListUnread(t.Context())
	if !errors.Is(err, ferrors.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if got := mb.calls(); got != 5 {
		t.Errorf("backend calls = %d, want 5 (fast-fail once open)", got)
	}
}

// The timeout has to bound the clock, not just decorate the error. The
// backend here would take 400ms; the wrapper allows 20ms, and the
// assertions are the elapsed time and the attempt count — an error value
// alone passed just as well when nothing was enforced at all.
func TestResilientTimesOutSlowBackend(t *testing.T) {
	const op = 20 * time.Millisecond
	// Twenty times the budget, so an elapsed time anywhere near it can
	// only mean the call ran to completion.
	mb := &flakyMailbox{slow: 20 * op}
	// MaxAttempts is left at the default (3): a timed-out attempt must
	// not be retried into a second and third identical wait.
	r := Wrap(mb, Config{OpTimeout: op, InitialDelay: time.Millisecond})

	start := time.Now()
	_, err := r.ListUnread(t.Context())
	elapsed := time.Since(start)

	if !errors.Is(err, ferrors.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	// The same error is a context deadline, which is how callers above
	// the wrapper tell a lapsed budget from a broken backend.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed >= mb.slow {
		t.Errorf("elapsed = %s, want far below the backend's %s — the call was not actually bounded",
			elapsed, mb.slow)
	}
	if got := mb.calls(); got != 1 {
		t.Errorf("attempts = %d, want 1: a call that used its whole budget must not be retried into another", got)
	}
}
