package imap

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"
)

// silentServer accepts connections and never writes a byte — no
// greeting, no tagged reply. It reproduces the one failure a mock cannot:
// the socket is up and the client is blocked in a read that go-imap
// arms no deadline for.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
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
			// Held open: closing would give the client an EOF to fail on,
			// which is the opposite of the case under test.
			conns = append(conns, c)
		}
	}()
	return ln.Addr().String()
}

func silentMailbox(t *testing.T) *Mailbox {
	t.Helper()
	mb, err := New(Config{Addr: silentServer(t), Username: "u", Password: "p", Insecure: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mb
}

// A deadline must abort the command in flight rather than expire quietly
// while the caller keeps waiting for connectTimeout.
func TestIMAPDeadlineAbortsCommand(t *testing.T) {
	mb := silentMailbox(t)
	const budget = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_, err := mb.ListUnread(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	// connectTimeout is 30s; anything near it means the deadline was
	// carried but never acted on.
	if elapsed >= 4*budget {
		t.Errorf("elapsed = %s, want close to the %s deadline", elapsed, budget)
	}

	// The connection that hung must not be waiting in the cache for the
	// next caller: a session abandoned mid-command is of unknown state,
	// and handing it on is how one timeout becomes two.
	mb.mu.Lock()
	cached := mb.idle
	mb.mu.Unlock()
	if cached != nil {
		t.Error("a timed-out connection was returned to the cache")
	}
}

// Cancellation is the disconnected client: the command aborts and the
// error says who gave up, so nothing upstream mistakes it for a fault.
func TestIMAPCancelAbortsCommand(t *testing.T) {
	mb := silentMailbox(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	err := mb.MarkSeen(ctx, "1")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, domain.ErrBadID) {
		t.Error("a cancelled call reported a bad id; the caller's id was never judged")
	}
	if elapsed >= time.Second {
		t.Errorf("elapsed = %s, want the cancellation to end the call immediately", elapsed)
	}

	mb.mu.Lock()
	cached := mb.idle
	mb.mu.Unlock()
	if cached != nil {
		t.Error("a cancelled connection was returned to the cache")
	}
}

// A context that is already done must not even reach the network: there
// is nobody to answer to, and dialling would cost the caller a connect
// timeout for an answer they will never read.
func TestIMAPCancelledContextSkipsDial(t *testing.T) {
	mb := silentMailbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := mb.Folders(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed >= 100*time.Millisecond {
		t.Errorf("elapsed = %s, want an immediate refusal", elapsed)
	}
}
