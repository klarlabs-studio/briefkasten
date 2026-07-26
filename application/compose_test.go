package application_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
)

// selfNamingSender is a fakeSender that reports its own address, the way
// both shipped transports do.
type selfNamingSender struct {
	fakeSender
	from string
}

func (s *selfNamingSender) From() string { return s.from }

const composeThread = "From: Alice <alice@example.com>\r\n" +
	"To: me@ours.example, Bob <bob@example.com>\r\n" +
	"Cc: carol@example.com\r\n" +
	"Subject: Q3 planning\r\n" +
	"Message-Id: <orig-1@example.com>\r\n" +
	"\r\nShall we meet?\r\n"

// newComposer wires a Composer over a maildir holding one message.
func newComposer(t *testing.T, sender domain.Sender) *application.Composer {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new", "orig.eml"), []byte(composeThread), 0o644); err != nil {
		t.Fatal(err)
	}
	mb, err := maildir.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return application.NewComposer(application.NewService(mb, nil), newDirOutbox(t, sender))
}

// The address a reply must not go to comes from the transport, not from
// an argument the caller could get wrong or omit.
func TestComposerExcludesTheOutboxAddress(t *testing.T) {
	c := newComposer(t, &selfNamingSender{from: "me@ours.example"})

	msg, err := c.PlanReply(context.Background(), application.ReplyRequest{
		ID: "orig.eml", All: true, Body: "Tuesday works.",
	})
	if err != nil {
		t.Fatalf("PlanReply: %v", err)
	}
	for _, addr := range msg.Recipients() {
		if strings.Contains(strings.ToLower(addr), "me@ours.example") {
			t.Errorf("recipients %v include the outbox's own address", msg.Recipients())
		}
	}
	if len(msg.Recipients()) != 3 {
		t.Errorf("recipients = %v, want alice, bob and carol", msg.Recipients())
	}
	if !strings.Contains(msg.Body, "Tuesday works.") || !strings.Contains(msg.Body, "> Shall we meet?") {
		t.Errorf("body = %q, want the reply above the quoted original", msg.Body)
	}
}

// A transport that cannot name itself loses the self-exclusion and
// nothing else: the reply still goes out, and the worst outcome is a
// copy landing back in this mailbox.
func TestComposerToleratesATransportThatCannotNameItself(t *testing.T) {
	c := newComposer(t, &fakeSender{})

	msg, err := c.PlanReply(context.Background(), application.ReplyRequest{
		ID: "orig.eml", All: true, Body: "ok",
	})
	if err != nil {
		t.Fatalf("PlanReply: %v", err)
	}
	if len(msg.Recipients()) != 4 {
		t.Errorf("recipients = %v, want everyone including this mailbox", msg.Recipients())
	}
}

// Planning does not queue anything: the confirmation gate sits between
// the two, and a derivation that enqueued as a side effect would have
// sent the message before anyone was asked.
func TestPlanDoesNotQueue(t *testing.T) {
	sender := &selfNamingSender{from: "me@ours.example"}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new", "orig.eml"), []byte(composeThread), 0o644); err != nil {
		t.Fatal(err)
	}
	mb, err := maildir.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ob := newDirOutbox(t, sender)
	c := application.NewComposer(application.NewService(mb, nil), ob)

	msg, err := c.PlanReply(context.Background(), application.ReplyRequest{ID: "orig.eml", Body: "x"})
	if err != nil {
		t.Fatalf("PlanReply: %v", err)
	}
	summary, err := ob.Summary()
	if err != nil {
		t.Fatal(err)
	}
	for state, ids := range summary {
		if len(ids) != 0 {
			t.Errorf("planning queued %d %s messages", len(ids), state)
		}
	}

	// Sending it afterwards is the ordinary outbox path.
	id, err := c.Send(msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, err := ob.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != "queued" {
		t.Errorf("state = %q, want queued", queued.State)
	}
}

// An HTML reply quotes the original as text, escaped: it is somebody
// else's message, and passing it through as markup would let their mail
// write into ours.
func TestHTMLReplyEscapesTheQuotedOriginal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := "From: alice@example.com\r\nSubject: hi\r\n\r\n<script>alert(1)</script>\r\n"
	if err := os.WriteFile(filepath.Join(root, "new", "orig.eml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	mb, err := maildir.New(root)
	if err != nil {
		t.Fatal(err)
	}
	c := application.NewComposer(application.NewService(mb, nil),
		newDirOutbox(t, &selfNamingSender{from: "me@ours.example"}))

	msg, err := c.PlanReply(context.Background(), application.ReplyRequest{
		ID: "orig.eml", Body: "noted", HTMLBody: "<p>noted</p>",
	})
	if err != nil {
		t.Fatalf("PlanReply: %v", err)
	}
	if strings.Contains(msg.HTMLBody, "<script>") {
		t.Errorf("quoted original was not escaped:\n%s", msg.HTMLBody)
	}
	if !strings.Contains(msg.HTMLBody, "&lt;script&gt;") {
		t.Errorf("quoted original is missing from the HTML alternative:\n%s", msg.HTMLBody)
	}
	// No html_body from the caller means none is invented.
	plain, err := c.PlanReply(context.Background(), application.ReplyRequest{ID: "orig.eml", Body: "noted"})
	if err != nil {
		t.Fatalf("PlanReply: %v", err)
	}
	if plain.HTMLBody != "" {
		t.Errorf("HTMLBody = %q, want none invented", plain.HTMLBody)
	}
}

// A forward planned here carries the original whole and goes only where
// the caller said — the mailbox's own audience never joins it.
func TestComposerPlansAForward(t *testing.T) {
	c := newComposer(t, &selfNamingSender{from: "me@ours.example"})

	msg, err := c.PlanForward(context.Background(), application.ForwardRequest{
		ID: "orig.eml", To: []string{"dave@example.com"}, Body: "fyi",
	})
	if err != nil {
		t.Fatalf("PlanForward: %v", err)
	}
	if len(msg.Recipients()) != 1 || msg.Recipients()[0] != "dave@example.com" {
		t.Errorf("recipients = %v, want only dave", msg.Recipients())
	}
	if msg.Subject != "Fwd: Q3 planning" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if len(msg.Attachments) != 1 || string(msg.Attachments[0].Content) != composeThread {
		t.Errorf("the original was not attached whole")
	}
	if !strings.Contains(msg.Body, "fyi") ||
		!strings.Contains(msg.Body, "---------- Forwarded message ----------") {
		t.Errorf("body = %q, want the note above the forwarded header block", msg.Body)
	}

	// A missing original is the mailbox's error, not an empty forward.
	if _, err := c.PlanForward(context.Background(), application.ForwardRequest{
		ID: "nope.eml", To: []string{"dave@example.com"},
	}); err == nil {
		t.Error("forwarding an unknown id succeeded")
	}
	if _, err := c.PlanReply(context.Background(), application.ReplyRequest{ID: "nope.eml", Body: "x"}); err == nil {
		t.Error("replying to an unknown id succeeded")
	}
}
