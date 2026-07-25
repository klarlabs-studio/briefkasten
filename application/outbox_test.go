package application_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
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

// gateSender parks inside Send until released — a delivery caught in
// flight, which is when a concurrent recovery does its damage.
type gateSender struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateSender) Send(_ context.Context, _ domain.OutboundMessage) error {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return nil
}

func TestOutboxRecoverRefusedWhileAnotherProcessOwnsTheOutbox(t *testing.T) {
	dir := t.TempDir()
	owner, err := maildir.NewOutboxStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	inflight := domain.OutboundMessage{
		ID: "inflight", To: []string{"a@b.c"}, Subject: "s", Body: "b",
		State: "sending", Attempts: 1,
	}
	if err := owner.Write(inflight); err != nil {
		t.Fatal(err)
	}
	// The owner is mid-delivery: it holds the outbox for the whole of
	// ProcessOnce, wire send included.
	release, err := owner.Lock()
	if err != nil {
		t.Fatal(err)
	}

	// A one-shot command over the same directory. Its own store handle
	// contends exactly as a separate process would.
	client := newDirOutboxAt(t, dir, &fakeSender{})
	if err := client.Recover(); !errors.Is(err, application.ErrOutboxBusy) {
		t.Errorf("Recover of a busy outbox = %v, want ErrOutboxBusy", err)
	}
	msg, err := client.Status("inflight")
	if err != nil {
		t.Fatal(err)
	}
	if msg.State != "sending" {
		t.Errorf("state = %q, want the in-flight message left in sending", msg.State)
	}

	// Once the owner is gone the same call does repair the directory.
	release()
	if err := client.Recover(); err != nil {
		t.Fatalf("Recover of a free outbox: %v", err)
	}
	msg, _ = client.Status("inflight")
	if msg.State != "failed" {
		t.Errorf("state = %q, want failed once nobody owns the outbox", msg.State)
	}
}

func TestOutboxProcessOnceHoldsTheOutboxAcrossTheSend(t *testing.T) {
	dir := t.TempDir()
	sender := &gateSender{entered: make(chan struct{}), release: make(chan struct{})}
	worker := newDirOutboxAt(t, dir, sender)

	id, err := worker.Enqueue(domain.OutboundMessage{To: []string{"a@b.c"}, Subject: "s", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	var unblock sync.Once
	defer unblock.Do(func() { close(sender.release) })

	done := make(chan error, 1)
	go func() {
		_, err := worker.ProcessOnce(context.Background())
		done <- err
	}()
	<-sender.entered // the record is in sending and the wire send is running

	// This is the reported bug: a CLI invocation recovering here used to
	// file the in-flight message as failed, the worker then completed the
	// delivery, and the human retried a message that had already gone out.
	client := newDirOutboxAt(t, dir, &fakeSender{})
	if err := client.Recover(); !errors.Is(err, application.ErrOutboxBusy) {
		t.Errorf("Recover during delivery = %v, want ErrOutboxBusy", err)
	}
	msg, err := client.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if msg.State != "sending" {
		t.Errorf("state during delivery = %q, want sending", msg.State)
	}

	unblock.Do(func() { close(sender.release) })
	if err := <-done; err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	msg, err = worker.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if msg.State != "sent" {
		t.Errorf("state = %q, want sent", msg.State)
	}
	if failed, _ := client.Summary(); len(failed["failed"]) != 0 {
		t.Errorf("failed = %v, want no phantom copy of a delivered message", failed["failed"])
	}
}

func TestOutboxRecoverSurfacesUnreadableRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := maildir.NewOutboxStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(domain.OutboundMessage{ID: "healthy", To: []string{"a@b.c"}, State: "queued"}); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, "queued", "torn.json")
	if err := os.WriteFile(torn, []byte(`{"id":"torn","to":["a@b.c"`), 0o600); err != nil {
		t.Fatal(err)
	}

	ob := application.NewOutbox(store, &fakeSender{})
	err = ob.Recover()
	if err == nil {
		t.Fatal("Recover swallowed an unreadable record")
	}
	if !strings.Contains(err.Error(), "torn") {
		t.Errorf("err = %v, want the damaged record named", err)
	}
	// Reported, not repaired: unsent mail is not recovery's to delete.
	if _, statErr := os.Stat(torn); statErr != nil {
		t.Errorf("damaged record removed by recovery: %v", statErr)
	}
	msg, err := ob.Status("healthy")
	if err != nil || msg.State != "queued" {
		t.Errorf("healthy record = %+v, %v; want untouched in queued", msg, err)
	}
}

func TestOutboxRecoverRepairsThreeWayDuplicate(t *testing.T) {
	dir := t.TempDir()
	store, err := maildir.NewOutboxStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// An old SEND crash left a stale queued copy behind; the delivery then
	// succeeded and SUCCEED crashed between Write(sent) and Remove(sending).
	// Repairing only one pair leaves a delivered message sitting in queued
	// or failed as well — which is how a message goes out twice.
	write(t, store, "dup-3", "queued", 0)
	write(t, store, "dup-3", "sending", 1)
	write(t, store, "dup-3", "sent", 1)

	ob := application.NewOutbox(store, &fakeSender{})
	if err := ob.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if states := statesHolding(t, store, "dup-3"); len(states) != 1 || states[0] != "sent" {
		t.Errorf("copies = %v, want exactly one in sent", states)
	}
	msg, err := store.Find("dup-3")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", msg.Attempts)
	}
}

func TestOutboxRecoverKeepsTheLatestCopyOfAThreeWayDuplicate(t *testing.T) {
	dir := t.TempDir()
	store, err := maildir.NewOutboxStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The same three states, reached the other way round: RETRY crashed
	// (failed + queued), the re-queued message was then picked up and SEND
	// crashed too (queued + sending). The set of states cannot tell the two
	// histories apart — the attempt count can, and here the sending copy is
	// the youngest, so the message is in flight, not failed.
	write(t, store, "dup-4", "failed", 1)
	write(t, store, "dup-4", "queued", 1)
	write(t, store, "dup-4", "sending", 2)

	ob := application.NewOutbox(store, &fakeSender{})
	if err := ob.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if states := statesHolding(t, store, "dup-4"); len(states) != 1 || states[0] != "failed" {
		t.Errorf("copies = %v, want exactly one in failed", states)
	}
	msg, err := store.Find("dup-4")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 — the sending copy was the current one", msg.Attempts)
	}
	if !strings.Contains(msg.Error, "restart") {
		t.Errorf("error = %q, want the stranded-delivery note", msg.Error)
	}
}

// write persists one copy of a message under a named state.
func write(t *testing.T, store *maildir.OutboxStore, id, state string, attempts int) {
	t.Helper()
	err := store.Write(domain.OutboundMessage{
		ID: id, To: []string{"a@b.c"}, Subject: "s", Body: "b",
		State: state, Attempts: attempts,
	})
	if err != nil {
		t.Fatalf("write %s/%s: %v", state, id, err)
	}
}

// statesHolding reports every state directory still holding the id.
func statesHolding(t *testing.T, store *maildir.OutboxStore, id string) []string {
	t.Helper()
	var states []string
	for _, state := range domain.OutboxStates {
		ids, err := store.List(state)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range ids {
			if got == id {
				states = append(states, state)
			}
		}
	}
	return states
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

// Queuing a message must not wait out an in-progress delivery run. The
// cross-process lock serialises transitions on records that already
// exist; a brand-new id cannot be one of those, and a caller who blocks
// here experiences an MCP email.send as a hung tool call for as long as
// the send timeout times the retry budget.
func TestOutboxEnqueueDoesNotWaitForDelivery(t *testing.T) {
	dir := t.TempDir()
	owner, err := maildir.NewOutboxStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for a worker parked mid-wire-send: it holds the outbox
	// for the whole of ProcessOnce.
	release, err := owner.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	client := newDirOutboxAt(t, dir, &fakeSender{})
	done := make(chan string, 1)
	go func() {
		id, qerr := client.Enqueue(domain.OutboundMessage{
			To: []string{"a@b.c"}, Subject: "s", Body: "b",
		})
		if qerr != nil {
			done <- ""
			return
		}
		done <- id
	}()

	select {
	case id := <-done:
		if id == "" {
			t.Fatal("Enqueue while the outbox was held returned an error")
		}
		msg, serr := client.Status(id)
		if serr != nil {
			t.Fatalf("Status of the queued message: %v", serr)
		}
		if msg.State != "queued" {
			t.Errorf("state = %q, want queued", msg.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked behind an in-progress delivery run")
	}
}
