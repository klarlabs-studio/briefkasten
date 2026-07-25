package main

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten"
)

// Version reporting used to live in main(), which returned early for it
// and so could be called from a test. It now runs inside run() with an
// injected writer, where TestCLIVersion* covers every spelling — main()
// is a single os.Exit(run(...)) with nothing left to assert.

func TestServeConfigLoadError(t *testing.T) {
	t.Setenv("BRIEFKASTEN_CONFIG", "/no/such/briefkasten.yaml")
	if code := serve(nil); code != 1 {
		t.Errorf("serve with missing config = %d, want 1", code)
	}
}

func TestServeMailboxInitError(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(cfgPath, []byte("backend: carrier-pigeon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIEFKASTEN_CONFIG", cfgPath)
	if code := serve(nil); code != 1 {
		t.Errorf("serve with unknown backend = %d, want 1", code)
	}
}

func TestServeListenError(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "briefkasten.yaml")
	cfg := "addr: \"256.256.256.256:0\"\n" + // unlistenable address
		"maildir: " + filepath.Join(root, "box") + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIEFKASTEN_CONFIG", cfgPath)
	if code := serve(nil); code != 1 {
		t.Errorf("serve with unlistenable addr = %d, want 1", code)
	}
}

// TestServeGracefulShutdown boots the full server — outbox worker and
// maildir watcher included — delivers a pre-queued message through the
// background worker, pushes new mail past the watcher, and stops the
// server with SIGINT, the production shutdown path.
func TestServeGracefulShutdown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	maildirRoot := filepath.Join(root, "box")
	outboxDir := filepath.Join(root, "outbox")
	deliverDir := filepath.Join(root, "delivered")
	cfgPath := filepath.Join(root, "briefkasten.yaml")
	cfg := "addr: \"" + addr + "\"\n" +
		"maildir: " + maildirRoot + "\n" +
		"outbox:\n" +
		"  dir: " + outboxDir + "\n" +
		"  from: me@x.y\n" +
		"  deliver_dir: " + deliverDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIEFKASTEN_CONFIG", cfgPath)

	// Queue an outbound message before the server starts: the background
	// worker (5s tick) must deliver it without any CLI involvement.
	sender, err := briefkasten.NewDirSender(deliverDir, "me@x.y")
	if err != nil {
		t.Fatal(err)
	}
	ob, err := briefkasten.NewOutbox(outboxDir, sender)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Enqueue(briefkasten.OutboundMessage{
		To: []string{"a@b.c"}, Subject: "Backlog", Body: "deliver me",
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() { done <- serve(nil) }()

	// Wait until the server accepts connections.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			if cerr := conn.Close(); cerr != nil {
				t.Error(cerr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// New mail in the maildir drives the watcher's push notification.
	if err := os.WriteFile(filepath.Join(maildirRoot, "new", "fresh.eml"),
		[]byte("From: a@b.c\r\nSubject: Neu\r\n\r\nhi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The outbox worker ticks every 5 seconds; wait for the delivery.
	deadline = time.Now().Add(15 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Join(deliverDir, "new"))
		if err == nil && len(entries) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox worker never delivered the queued message: %v, %v", entries, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("graceful shutdown exit = %d, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not shut down after SIGINT")
	}
}
