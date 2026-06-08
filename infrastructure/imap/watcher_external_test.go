package imap_test

import (
	"bytes"
	"context"
	"net"
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
}
