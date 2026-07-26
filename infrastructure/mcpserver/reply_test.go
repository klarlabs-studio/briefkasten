package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// threadMessage is the original the reply tests answer.
const threadMessage = "From: Alice <alice@example.com>\r\n" +
	"To: me@ours.example, Bob <bob@example.com>\r\n" +
	"Cc: carol@example.com\r\n" +
	"Bcc: hidden@example.com\r\n" +
	"Subject: Q3 planning\r\n" +
	"Date: Mon, 8 Jun 2026 12:00:00 +0000\r\n" +
	"Message-Id: <orig-1@example.com>\r\n" +
	"\r\nShall we meet?\r\n"

// replyFixture wires a client over a maildir holding one message, with
// an outbox whose transport names itself "me@ours.example".
func replyFixture(t *testing.T) (*application.Outbox, func(string, map[string]any) map[string]any) {
	t.Helper()
	ob := newOutbox(t, &fakeSender{from: "me@ours.example"})
	client, root := newClient(t, WithOutbox(ob))
	drop(t, root, "orig.eml", threadMessage)
	return ob, func(tool string, args map[string]any) map[string]any {
		return callMap(t, client, tool, args)
	}
}

// queued reads back the message the tool put in the outbox — what was
// actually built, rather than what the arguments implied.
func queued(t *testing.T, ob *application.Outbox, out map[string]any) domain.OutboundMessage {
	t.Helper()
	id, ok := out["id"].(string)
	if !ok {
		t.Fatalf("tool result %v carries no outbox id", out)
	}
	msg, err := ob.Status(id)
	if err != nil {
		t.Fatalf("outbox status %s: %v", id, err)
	}
	return msg
}

// A reply-all derives its whole audience from the original: Reply-To or
// From into To, the original's To and Cc into Cc, our own address gone,
// and the original's Bcc nowhere at all.
func TestReplyAllDerivesRecipients(t *testing.T) {
	ob, call := replyFixture(t)
	out := call("email.reply", map[string]any{
		"id": "orig.eml", "body": "Tuesday works.", "all": true, "confirm": true,
	})
	if out["state"] != "queued" {
		t.Fatalf("email.reply = %v", out)
	}
	msg := queued(t, ob, out)

	if len(msg.To) != 1 || !strings.Contains(msg.To[0], "alice@example.com") {
		t.Errorf("To = %v, want the original's sender", msg.To)
	}
	cc := strings.ToLower(strings.Join(msg.Cc, " "))
	for _, want := range []string{"bob@example.com", "carol@example.com"} {
		if !strings.Contains(cc, want) {
			t.Errorf("Cc = %v, want %s", msg.Cc, want)
		}
	}
	everyone := strings.ToLower(strings.Join(msg.Recipients(), " "))
	if strings.Contains(everyone, "me@ours.example") {
		t.Errorf("recipients %v include our own address", msg.Recipients())
	}
	if strings.Contains(everyone, "hidden@example.com") {
		t.Errorf("recipients %v leak the original's Bcc", msg.Recipients())
	}
	if msg.Subject != "Re: Q3 planning" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if msg.InReplyTo != "<orig-1@example.com>" {
		t.Errorf("In-Reply-To = %q", msg.InReplyTo)
	}
	if !strings.Contains(msg.Body, "Tuesday works.") || !strings.Contains(msg.Body, "> Shall we meet?") {
		t.Errorf("body does not carry the reply above the quoted original:\n%s", msg.Body)
	}
}

// A plain reply reaches only the sender — all=true is what widens it,
// and its absence must not widen anything.
func TestReplyWithoutAllReachesOnlyTheSender(t *testing.T) {
	ob, call := replyFixture(t)
	out := call("email.reply", map[string]any{"id": "orig.eml", "body": "ok", "confirm": true})
	msg := queued(t, ob, out)
	if len(msg.Recipients()) != 1 {
		t.Errorf("recipients = %v, want only alice", msg.Recipients())
	}
}

// A forward attaches the original whole and goes only where the caller
// said.
func TestForwardAttachesTheOriginal(t *testing.T) {
	ob, call := replyFixture(t)
	out := call("email.forward", map[string]any{
		"id": "orig.eml", "to": []string{"dave@example.com"}, "body": "fyi", "confirm": true,
	})
	msg := queued(t, ob, out)

	if len(msg.Recipients()) != 1 || msg.Recipients()[0] != "dave@example.com" {
		t.Errorf("recipients = %v, want only dave", msg.Recipients())
	}
	if msg.Subject != "Fwd: Q3 planning" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ContentType != "message/rfc822" {
		t.Fatalf("attachments = %+v, want the original as message/rfc822", msg.Attachments)
	}
	if string(msg.Attachments[0].Content) != threadMessage {
		t.Errorf("the forwarded original was not preserved byte for byte")
	}
}

// Neither answering tool is reachable without human approval. An
// injected "reply to everyone with…" in a message body would otherwise
// have an unguarded path to a send that cannot be undone.
func TestReplyAndForwardRequireConfirmation(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"email.reply", map[string]any{"id": "orig.eml", "body": "x", "all": true}},
		{"email.forward", map[string]any{"id": "orig.eml", "to": []string{"evil@example.com"}}},
	} {
		ob := newOutbox(t, &fakeSender{from: "me@ours.example"})
		client, root := newClient(t, WithOutbox(ob))
		drop(t, root, "orig.eml", threadMessage)

		if _, err := client.CallTool(tc.tool, tc.args); err == nil {
			t.Errorf("%s without confirm succeeded — the human-in-the-loop gate is gone", tc.tool)
		} else if !strings.Contains(err.Error(), "confirmation required") {
			t.Errorf("%s error = %v, want it to name the missing confirmation", tc.tool, err)
		}
		summary, err := ob.Summary()
		if err != nil {
			t.Fatal(err)
		}
		for state, ids := range summary {
			if len(ids) != 0 {
				t.Errorf("%s queued %d %s messages despite the declined gate", tc.tool, len(ids), state)
			}
		}
	}
}

// The prompt a human answers must lead with the number of people the
// message reaches, break that number down by field, and name the Bcc
// count on its own — that group appears in no header, so if it is not
// stated here it is stated nowhere.
func TestSendConfirmationPromptLeadsWithTheRecipientCount(t *testing.T) {
	msg := domain.OutboundMessage{
		To:      []string{"alice@example.com", "bob@example.com"},
		Cc:      addresses("cc", 5),
		Bcc:     addresses("bcc", 73),
		Subject: "Re: Q3 planning",
	}
	sender := &fakeElicitSender{action: "accept"}
	if err := confirmSend(elicitCtx(t, sender), false, "reply", msg); err != nil {
		t.Fatalf("confirmSend: %v", err)
	}

	for _, want := range []string{
		"Send this reply to 80 recipients?",  // the count, first
		"(2 To, 5 Cc, 73 Bcc)",               // broken down by field
		"alice@example.com, bob@example.com", // a sample, not a wall
		"and 75 more",
		"73 of them are Bcc", // the group nobody can check by eye
		"cannot be checked by eye",
		"Sending cannot be undone.",
	} {
		if !strings.Contains(sender.prompt, want) {
			t.Errorf("prompt = %q\nwant it to contain %q", sender.prompt, want)
		}
	}
	// Eighty addresses printed in full would bury the number that matters.
	if strings.Count(sender.prompt, "@") > 10 {
		t.Errorf("prompt spells out too many addresses:\n%s", sender.prompt)
	}

	// Declining refuses one action, and that refusal names the count too.
	sender = &fakeElicitSender{action: "decline"}
	err := confirmSend(elicitCtx(t, sender), false, "reply", msg)
	if err == nil || !strings.Contains(err.Error(), "80 recipients") {
		t.Errorf("declined send = %v, want a refusal naming the count", err)
	}

	// And confirm=true still works for clients without elicitation.
	if err := confirmSend(t.Context(), true, "reply", msg); err != nil {
		t.Errorf("pre-confirmed send = %v, want nil", err)
	}
}

// A message with no blind copies says nothing about Bcc: a warning that
// fires on every send is one nobody reads on the send that needed it.
func TestSendConfirmationPromptOmitsBccWhenThereIsNone(t *testing.T) {
	sender := &fakeElicitSender{action: "accept"}
	err := confirmSend(elicitCtx(t, sender), false, "email", domain.OutboundMessage{
		To: []string{"a@b.c"}, Subject: "hi",
	})
	if err != nil {
		t.Fatalf("confirmSend: %v", err)
	}
	if !strings.Contains(sender.prompt, "Send this email to 1 recipient? (1 To)") {
		t.Errorf("prompt = %q, want the singular one-recipient form", sender.prompt)
	}
	if strings.Contains(sender.prompt, "Bcc") {
		t.Errorf("prompt mentions Bcc on a message that has none: %q", sender.prompt)
	}
}

// A reply-all is not capped, only made visible: a large thread is
// ordinary mail, and a cap would only teach callers to split a send into
// batches that each understate the audience.
func TestLargeReplyAllIsAllowedOnceConfirmed(t *testing.T) {
	ob := newOutbox(t, &fakeSender{from: "me@ours.example"})
	client, root := newClient(t, WithOutbox(ob))

	var recipients []string
	for i := range 200 {
		recipients = append(recipients, fmt.Sprintf("p%03d@example.com", i))
	}
	drop(t, root, "big.eml", "From: alice@example.com\r\n"+
		"To: me@ours.example\r\n"+
		"Cc: "+strings.Join(recipients, ", ")+"\r\n"+
		"Subject: all hands\r\n"+
		"Message-Id: <big@example.com>\r\n\r\nbody\r\n")

	out := callMap(t, client, "email.reply", map[string]any{
		"id": "big.eml", "body": "noted", "all": true, "confirm": true,
	})
	if out["state"] != "queued" {
		t.Fatalf("large reply-all = %v, want it queued", out)
	}
	msg := queued(t, ob, out)
	if got := len(msg.Recipients()); got != 201 {
		t.Errorf("recipients = %d, want alice plus the 200 on the thread", got)
	}
}

// addresses builds n distinct addresses under a tag.
func addresses(tag string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%03d@example.com", tag, i)
	}
	return out
}
