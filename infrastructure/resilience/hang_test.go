package resilience_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/infrastructure/imap"
	"go.klarlabs.de/briefkasten/infrastructure/resilience"
)

// hangBudget is the per-call timeout these tests give the wrapper. Short
// enough to keep the suite fast, long enough that a loaded machine
// cannot mistake scheduling noise for a missed deadline.
const hangBudget = 200 * time.Millisecond

// listenAndIgnore starts a TCP listener that accepts connections and
// then says nothing at all — no IMAP greeting, no tagged reply, ever.
//
// This is the failure the resilience wrapper exists for and the one a
// mock backend cannot reproduce: the socket is up, the dial succeeds,
// and the client blocks in a read that has no deadline of its own. A
// server that refused the connection would fail fast and prove nothing.
func listenAndIgnore(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		// Hold every accepted connection open until the listener closes;
		// dropping them would let the client see EOF and fail early,
		// which is the opposite of what is being tested.
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	return ln.Addr().String()
}

func hungMailbox(t *testing.T, cfg resilience.Config) *resilience.Mailbox {
	t.Helper()
	mb, err := imap.New(imap.Config{
		Addr:     listenAndIgnore(t),
		Username: "u",
		Password: "p",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("imap.New: %v", err)
	}
	return resilience.Wrap(mb, cfg)
}

// A backend that never answers must cost the caller one budget, not one
// per retry attempt. The elapsed time is the assertion: an error value
// alone would pass just as happily while the request sat there for the
// full three attempts.
func TestResilientBoundsHungBackendToOneBudget(t *testing.T) {
	// MaxAttempts is deliberately left at the default (3) — the point is
	// that the default retry policy does not turn one hang into three.
	r := hungMailbox(t, resilience.Config{OpTimeout: hangBudget, InitialDelay: time.Millisecond})

	start := time.Now()
	_, err := r.ListUnread(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ListUnread against a silent server returned nil error")
	}
	// A timeout must read as a timeout, not as a backend fault: this is
	// what tells a caller to wait and try again rather than to go looking
	// for a broken mailbox.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed < hangBudget {
		t.Errorf("elapsed = %s, want at least the %s budget — the call failed for some other reason",
			elapsed, hangBudget)
	}
	if elapsed >= 2*hangBudget {
		t.Errorf("elapsed = %s, want well under 2x the %s budget; ~3x means retry re-hung on the same dead server",
			elapsed, hangBudget)
	}
}

// A caller that gives up mid-flight must be released promptly, not held
// until the operation's own budget runs out. This is the disconnected
// MCP client: the round trip it abandoned has to stop.
func TestResilientCancelReleasesCallerMidFlight(t *testing.T) {
	// A budget far longer than the test is willing to wait, so only the
	// cancellation can end the call.
	r := hungMailbox(t, resilience.Config{OpTimeout: 30 * time.Second, InitialDelay: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(hangBudget/4, cancel)
	defer timer.Stop()

	start := time.Now()
	_, err := r.ListUnread(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed >= hangBudget {
		t.Errorf("elapsed = %s, want the call to end with the cancellation (~%s), not to wait out its budget",
			elapsed, hangBudget/4)
	}
}

// Cancellation says nothing about the server, so it must not open the
// breaker. Otherwise one client that keeps hanging up fast-fails every
// other client's mail.
func TestResilientCancellationDoesNotTripBreaker(t *testing.T) {
	r := hungMailbox(t, resilience.Config{OpTimeout: 30 * time.Second, InitialDelay: time.Millisecond})

	// Comfortably past the default trip threshold of 5 consecutive
	// failures.
	var wg sync.WaitGroup
	for range 7 {
		ctx, cancel := context.WithCancel(context.Background())
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(hangBudget / 20)
			cancel()
		}()
		if _, err := r.ListUnread(ctx); !errors.Is(err, context.Canceled) {
			wg.Wait()
			t.Fatalf("err = %v, want context.Canceled — a cancelled call must not be counted as a backend fault", err)
		}
	}
	wg.Wait()

	// The breaker is still closed, so a real call reaches the backend and
	// fails on its own deadline rather than being refused outright.
	ctx, cancel := context.WithTimeout(context.Background(), hangBudget)
	defer cancel()
	if _, err := r.ListUnread(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error; the breaker opened on cancellations", err)
	}
}
