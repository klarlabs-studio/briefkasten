package maildir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherFiresOnNewMail(t *testing.T) {
	root := t.TempDir()
	if _, err := New(root); err != nil { // creates root/new + root/cur
		t.Fatal(err)
	}

	w := NewWatcher(root)
	changes := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.Watch(ctx, func() { changes <- struct{}{} }) }()

	// Give the watcher a moment to register before delivering mail.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "new", "m1.eml"),
		[]byte("From: a@b.c\r\nSubject: hi\r\n\r\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changes:
		// got the change signal
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire on new mail")
	}
}

func TestWatcherStopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	if _, err := New(root); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(root)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx, func() {}) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}
}
