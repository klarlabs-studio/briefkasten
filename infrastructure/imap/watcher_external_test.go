package imap_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	bimap "go.klarlabs.de/briefkasten/infrastructure/imap"
)

// startIMAPServerWithUser runs an in-memory IMAP server with an empty INBOX and
// returns the address plus the user, so a test can deliver mail mid-session.
func startIMAPServerWithUser(t *testing.T) (string, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser("alice", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), user
}

func TestIMAPWatcherFiresOnNewMail(t *testing.T) {
	addr, user := startIMAPServerWithUser(t)

	w := bimap.NewWatcher(bimap.Config{
		Addr:     addr,
		Username: "alice",
		Password: "secret",
		Insecure: true,
	})
	changes := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Watch(ctx, func() { changes <- struct{}{} }) }()

	// Let the watcher connect, select INBOX, and enter IDLE before delivery.
	time.Sleep(300 * time.Millisecond)

	raw := []byte(testMessage)
	if _, err := user.Append("INBOX", literal{bytes.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changes:
		// IDLE delivered the unilateral mailbox update
	case <-time.After(3 * time.Second):
		t.Fatal("IMAP watcher did not fire on new mail via IDLE")
	}
}

func TestIMAPWatcherStopsOnContextCancel(t *testing.T) {
	addr, _ := startIMAPServerWithUser(t)
	w := bimap.NewWatcher(bimap.Config{Addr: addr, Username: "alice", Password: "secret", Insecure: true})
	var retries atomic.Int32
	w.SetRetryNotify(func(int, time.Duration, error) { retries.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx, func() {}) }()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}
	// Shutdown is not a failure: it must not look like one to the operator.
	if n := retries.Load(); n != 0 {
		t.Errorf("cancel reported %d retries, want 0", n)
	}
}

// retry is one reconnect attempt as reported through SetRetryNotify.
type retry struct {
	attempt int
	delay   time.Duration
	err     error
}

// recordRetries wires w's retry hook to a buffered channel so a test can assert
// on the attempts without racing the watcher goroutine.
func recordRetries(w *bimap.Watcher) <-chan retry {
	ch := make(chan retry, 16)
	w.SetRetryNotify(func(attempt int, delay time.Duration, err error) {
		select {
		case ch <- retry{attempt, delay, err}:
		default:
		}
	})
	return ch
}

func TestIMAPWatcherStopsOnAuthFailure(t *testing.T) {
	addr, _ := startIMAPServerWithUser(t)
	w := bimap.NewWatcher(bimap.Config{
		Addr:     addr,
		Username: "alice",
		Password: "wrong",
		Insecure: true,
	})
	// Short enough that a retrying watcher would report several attempts well
	// inside the deadline below.
	w.SetBackoff(10*time.Millisecond, 20*time.Millisecond)
	var retries atomic.Int32
	w.SetRetryNotify(func(int, time.Duration, error) { retries.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx, func() {}) }()

	select {
	case err := <-done:
		var ierr *imap.Error
		if !errors.As(err, &ierr) || ierr.Code != imap.ResponseCodeAuthenticationFailed {
			t.Fatalf("Watch returned %v, want an AUTHENTICATIONFAILED imap.Error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch kept reconnecting after the credential was rejected")
	}
	if n := retries.Load(); n != 0 {
		t.Errorf("retried %d times on a permanent failure, want 0", n)
	}
}

func TestIMAPWatcherStopsOnMissingMailbox(t *testing.T) {
	addr, _ := startIMAPServerWithUser(t)
	w := bimap.NewWatcher(bimap.Config{
		Addr:     addr,
		Username: "alice",
		Password: "secret",
		Mailbox:  "Nonexistent",
		Insecure: true,
	})
	w.SetBackoff(10*time.Millisecond, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx, func() {}) }()

	select {
	case err := <-done:
		var ierr *imap.Error
		if !errors.As(err, &ierr) || ierr.Code != imap.ResponseCodeNonExistent {
			t.Fatalf("Watch returned %v, want a NONEXISTENT imap.Error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch kept reconnecting to a mailbox that does not exist")
	}
}

func TestIMAPWatcherBackoffGrowsAndCaps(t *testing.T) {
	w := bimap.NewWatcher(bimap.Config{
		Addr:     deadAddr(t),
		Username: "alice",
		Password: "secret",
		Insecure: true,
	})
	const unit = 10 * time.Millisecond
	w.SetBackoff(unit, 4*unit)
	retries := recordRetries(w)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Watch(ctx, func() {}) }()

	// Doubling until the cap, then flat — never a flat 5s loop, and never
	// unbounded growth.
	want := []time.Duration{unit, 2 * unit, 4 * unit, 4 * unit, 4 * unit}
	for i, wantDelay := range want {
		select {
		case got := <-retries:
			if got.delay != wantDelay {
				t.Fatalf("retry %d delay = %v, want %v", i+1, got.delay, wantDelay)
			}
			if got.attempt != i+1 {
				t.Errorf("retry %d reported attempt %d", i+1, got.attempt)
			}
			if got.err == nil {
				t.Errorf("retry %d reported no error", i+1)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d retries reported, want %d", i, len(want))
		}
	}
}

func TestIMAPWatcherRecoversFromTransientFailure(t *testing.T) {
	backend, user := startIMAPServerWithUser(t)
	// The first two dials are hung up on, as a restarting server would.
	addr, conns := startFlakyProxy(t, backend, 2)

	w := bimap.NewWatcher(bimap.Config{
		Addr:     addr,
		Username: "alice",
		Password: "secret",
		Insecure: true,
	})
	w.SetBackoff(10*time.Millisecond, 40*time.Millisecond)
	retries := recordRetries(w)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan struct{}, 4)
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx, func() { changes <- struct{}{} }) }()

	select {
	case got := <-retries:
		if got.err == nil {
			t.Fatal("retry reported without an error")
		}
	case err := <-done:
		t.Fatalf("Watch returned %v; a dropped connection must be retried", err)
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not retry a dropped connection")
	}

	waitFor(t, 5*time.Second, func() bool { return conns.Load() > 2 })
	// Let the recovered session select INBOX and enter IDLE before delivery.
	time.Sleep(300 * time.Millisecond)

	raw := []byte(testMessage)
	if _, err := user.Append("INBOX", literal{bytes.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
		// The watcher reconnected and IDLE is live again.
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire after recovering from a dropped connection")
	}
}

// deadAddr returns a loopback address nothing listens on, so dialling it fails
// immediately instead of hanging.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// startFlakyProxy fronts backend with a listener that hangs up on the first
// drop connections and proxies the rest, standing in for a server that is
// briefly unreachable. The counter reports accepted connections.
func startFlakyProxy(t *testing.T, backend string, drop int32) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns atomic.Int32
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			if conns.Add(1) <= drop {
				_ = client.Close()
				continue
			}
			go proxyConn(client, backend)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), &conns
}

// proxyConn pipes client to backend in both directions until either side ends.
func proxyConn(client net.Conn, backend string) {
	upstream, err := net.Dial("tcp", backend)
	if err != nil {
		_ = client.Close()
		return
	}
	closeBoth := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	go func() {
		_, _ = io.Copy(upstream, client)
		closeBoth()
	}()
	_, _ = io.Copy(client, upstream)
	closeBoth()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
