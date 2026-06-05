package application_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixgeelhaar/briefkasten/application"
	"github.com/felixgeelhaar/briefkasten/domain"
	"github.com/felixgeelhaar/briefkasten/infrastructure/maildir"
)

// fakeSender records deliveries and fails on demand.
type fakeSender struct {
	sent []domain.OutboundMessage
	err  error
}

func (f *fakeSender) Send(_ context.Context, msg domain.OutboundMessage) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, msg)
	return nil
}

func TestOutboxEnqueueAndDeliver(t *testing.T) {
	sender := &fakeSender{}
	ob := newDirOutbox(t, sender)

	id, err := ob.Enqueue(domain.OutboundMessage{
		To:      []string{"steuerberater@kanzlei.example"},
		Subject: "Unterlagen 2025",
		Body:    "Anbei die fehlenden Belege.",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}

	msg, err := ob.Status(id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if msg.State != "queued" {
		t.Errorf("state = %q, want queued", msg.State)
	}

	n, err := ob.ProcessOnce(context.Background())
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("processed = %d, want 1", n)
	}
	if len(sender.sent) != 1 || sender.sent[0].Subject != "Unterlagen 2025" {
		t.Fatalf("sent = %+v", sender.sent)
	}

	msg, err = ob.Status(id)
	if err != nil {
		t.Fatalf("Status after send: %v", err)
	}
	if msg.State != "sent" {
		t.Errorf("state = %q, want sent", msg.State)
	}
	if msg.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", msg.Attempts)
	}
}

func TestOutboxFailureKeepsMessageWithError(t *testing.T) {
	sender := &fakeSender{err: errors.New("smtp down")}
	ob := newDirOutbox(t, sender)

	id, err := ob.Enqueue(domain.OutboundMessage{To: []string{"a@b.c"}, Subject: "x", Body: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ob.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}

	msg, err := ob.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if msg.State != "failed" {
		t.Errorf("state = %q, want failed", msg.State)
	}
	if !strings.Contains(msg.Error, "smtp down") {
		t.Errorf("error = %q", msg.Error)
	}

	// Retry moves it back to queued; a recovered sender delivers it.
	sender.err = nil
	if err := ob.Retry(id); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	msg, _ = ob.Status(id)
	if msg.State != "queued" {
		t.Errorf("state after retry = %q, want queued", msg.State)
	}
	if _, err := ob.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg, _ = ob.Status(id)
	if msg.State != "sent" {
		t.Errorf("state = %q, want sent after retry", msg.State)
	}
	if msg.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", msg.Attempts)
	}
}

func TestOutboxInvalidTransitionsRejected(t *testing.T) {
	ob := newDirOutbox(t, &fakeSender{})

	id, err := ob.Enqueue(domain.OutboundMessage{To: []string{"a@b.c"}, Subject: "x", Body: "y"})
	if err != nil {
		t.Fatal(err)
	}
	// Retrying a queued (not failed) message is not a legal transition.
	if err := ob.Retry(id); err == nil {
		t.Error("retry of queued message accepted")
	}

	if _, err := ob.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Retrying a sent message is not legal either.
	if err := ob.Retry(id); err == nil {
		t.Error("retry of sent message accepted")
	}
}

func TestOutboxEnqueueValidation(t *testing.T) {
	ob := newDirOutbox(t, &fakeSender{})
	if _, err := ob.Enqueue(domain.OutboundMessage{Subject: "no recipient"}); err == nil {
		t.Error("message without recipients accepted")
	}
}

func TestOutboxSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	sender := &fakeSender{}

	ob1 := newDirOutboxAt(t, dir, sender)
	id, err := ob1.Enqueue(domain.OutboundMessage{To: []string{"a@b.c"}, Subject: "persist", Body: "me"})
	if err != nil {
		t.Fatal(err)
	}

	// New instance over the same dir sees and delivers the queued message.
	ob2 := newDirOutboxAt(t, dir, sender)
	if _, err := ob2.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg, err := ob2.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if msg.State != "sent" {
		t.Errorf("state = %q, want sent across restart", msg.State)
	}
}

func TestOutboxStatusUnknownID(t *testing.T) {
	ob := newDirOutbox(t, &fakeSender{})
	if _, err := ob.Status("nope"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("err = %v, want domain.ErrBadID", err)
	}
	if _, err := ob.Status("../../etc/passwd"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("traversal err = %v, want domain.ErrBadID", err)
	}
}

func TestDirSenderDeliversIntoMaildir(t *testing.T) {
	target := t.TempDir()
	sender, err := maildir.NewSender(target, "nexa@local.example")
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), domain.OutboundMessage{
		ID:      "m-1",
		To:      []string{"alice@web-acme.de"},
		Subject: "Grüße",
		Body:    "Hallo Welt",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	files, _ := filepath.Glob(filepath.Join(target, "new", "*.eml"))
	if len(files) != 1 {
		t.Fatalf("delivered = %v, want one .eml in new/", files)
	}
	raw, _ := os.ReadFile(files[0])
	for _, want := range []string{"From: nexa@local.example", "To: alice@web-acme.de", "Hallo Welt"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("delivered message missing %q:\n%s", want, raw)
		}
	}
	// RFC 2047 for the non-ASCII subject or raw UTF-8 — either way the
	// subject header must exist.
	if !strings.Contains(string(raw), "Subject: ") {
		t.Errorf("no subject header:\n%s", raw)
	}
}

func newDirOutbox(t *testing.T, sender domain.Sender) *application.Outbox {
	t.Helper()
	return newDirOutboxAt(t, t.TempDir(), sender)
}

func newDirOutboxAt(t *testing.T, dir string, sender domain.Sender) *application.Outbox {
	t.Helper()
	store, err := maildir.NewOutboxStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return application.NewOutbox(store, sender)
}
