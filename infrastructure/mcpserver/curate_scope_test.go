package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/server"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// Curation is not an unread-only privilege: a message that was processed
// weeks ago is exactly the kind a human reaches for archive to tidy.
func TestArchiveReadMessageOverMCP(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")
	drop(t, root, "b.eml", "From: a@b.c\r\nSubject: Beta\r\n\r\nbeta")
	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})

	out := callMap(t, client, "email.archive", map[string]any{"id": "a.eml", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("archive of read message = %v, want ok", out)
	}

	all := callMap(t, client, "email.list", map[string]any{"scope": "all"})
	if ids := all["ids"].([]string); len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("all after archiving read mail = %v, want [b.eml]", ids)
	}
}

func TestDeleteReadMessageOverMCP(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")
	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})

	out := callMap(t, client, "email.delete", map[string]any{"id": "a.eml", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("delete of read message = %v, want ok", out)
	}

	all := callMap(t, client, "email.list", map[string]any{"scope": "all"})
	if ids := all["ids"].([]string); len(ids) != 0 {
		t.Errorf("all after deleting read mail = %v, want none", ids)
	}
}

// Read mail still needs the human gate — being already processed does
// not make a soft move unattended work.
func TestCurateReadMessageStillNeedsConfirmation(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")
	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})

	for _, tool := range []string{"email.archive", "email.delete"} {
		_, err := client.CallToolRaw(tool, map[string]any{"id": "a.eml"})
		if err == nil || !strings.Contains(err.Error(), "confirmation required") {
			t.Errorf("%s on read mail without confirm: %v, want the confirmation gate", tool, err)
		}
	}
}

// The tool is annotated idempotent, so acknowledging twice must succeed
// rather than fail on the second pass — an agent that retries a batch
// after a partial failure re-marks what it already processed.
func TestMarkSeenIsIdempotentOverMCP(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")

	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})
	out := callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})
	if out["ok"] != true {
		t.Fatalf("second mark_seen = %v, want ok", out)
	}

	read := callMap(t, client, "email.list", map[string]any{"scope": "read"})
	if ids := read["ids"].([]string); len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("read after re-marking = %v, want [a.eml]", ids)
	}
}

func TestMarkSeenUnknownIDFailsOverMCP(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")

	if _, err := client.CallToolRaw("email.mark_seen", map[string]any{"id": "ghost.eml"}); err == nil {
		t.Error("mark_seen of an unknown id succeeded, want an error")
	}
}

// The bundled UI is the human surface for the same tools, so it must
// reach read mail as well: a scope selector to see it and the curation
// tools to act on it.
func TestInboxUIReachesReadMail(t *testing.T) {
	client, _ := newClient(t)

	page, err := client.ReadResource(InboxUIResourceURI)
	if err != nil {
		t.Fatalf("read UI resource: %v", err)
	}
	for _, want := range []string{
		`id="scope"`,      // the unread/read/all selector
		`value="read"`,    // ... which can ask for read mail
		`value="all"`,     //     ... and the whole mailbox
		"'email.list'",    // scoped listing, not just email.list_unread
		"'email.archive'", // curation actions on any listed message
		"'email.delete'",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("inbox UI does not contain %q — it cannot act on read mail", want)
		}
	}
}

// Both id completions serve surfaces that reach read mail — the inbox
// resource and the draft_reply prompt — so both must suggest read ids;
// otherwise a host can only ever complete the backlog.
func TestIDCompletionsCoverReadMail(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "alpha.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")
	drop(t, root, "beta.eml", "From: a@b.c\r\nSubject: Beta\r\n\r\nbeta")
	if err := mb.MarkSeen(t.Context(), "alpha.eml"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	srv := New(application.NewService(mb, nil))

	for _, tc := range []struct {
		name string
		ref  server.CompletionRef
	}{
		{"resource", server.CompletionRef{Type: server.CompletionRefResource, URI: "email://inbox/{id}"}},
		{"prompt", server.CompletionRef{Type: server.CompletionRefPrompt, Name: "draft_reply"}},
	} {
		res, err := srv.HandleCompletion(t.Context(), tc.ref, server.CompletionArgument{Name: "id", Value: ""})
		if err != nil {
			t.Fatalf("%s completion: %v", tc.name, err)
		}
		got := map[string]bool{}
		for _, v := range res.Values {
			got[v] = true
		}
		if !got["alpha.eml"] {
			t.Errorf("%s completion = %v, want the read message alpha.eml among them", tc.name, res.Values)
		}
		if !got["beta.eml"] {
			t.Errorf("%s completion = %v, want the unread message beta.eml among them", tc.name, res.Values)
		}
	}
}

// Approving a move is only meaningful if you know where it goes, so the
// elicitation prompt names the destination.
func TestConfirmationPromptNamesDestination(t *testing.T) {
	sender := &fakeElicitSender{action: "accept"}
	ctx := elicitCtx(t, sender)

	if err := confirmCuration(ctx, false, "delete", []string{"m1.eml"}, "INBOX.Trash"); err != nil {
		t.Fatalf("confirmCuration: %v", err)
	}
	if !strings.Contains(sender.prompt, "INBOX.Trash") {
		t.Errorf("prompt = %q, want it to name the destination folder", sender.prompt)
	}
	if !strings.Contains(sender.prompt, "never destroyed") {
		t.Errorf("prompt = %q, want it to still say the move is reversible", sender.prompt)
	}
}

// A backend that cannot report a destination must still be able to ask.
func TestConfirmationPromptWithoutDestination(t *testing.T) {
	sender := &fakeElicitSender{action: "accept"}
	ctx := elicitCtx(t, sender)

	if err := confirmCuration(ctx, false, "archive", []string{"m1.eml"}, ""); err != nil {
		t.Fatalf("confirmCuration: %v", err)
	}
	if !strings.Contains(sender.prompt, "never destroyed") {
		t.Errorf("prompt = %q, want the reversibility wording", sender.prompt)
	}
}

// The folders resource carries the curation plan, so a client can show
// where mail would go without spending a tool call or moving anything.
func TestFoldersResourceReportsCurationPlan(t *testing.T) {
	client, _ := newClient(t)

	text, err := client.ReadResource("email://folders")
	if err != nil {
		t.Fatalf("read email://folders: %v", err)
	}
	var payload struct {
		Folders  []string            `json:"folders"`
		Curation domain.CurationPlan `json:"curation"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("folders payload not JSON: %v (%q)", err, text)
	}
	if len(payload.Folders) == 0 {
		t.Error("folders list is empty")
	}
	if payload.Curation.Trash.Folder != ".trash" || payload.Curation.Archive.Folder != ".archive" {
		t.Errorf("curation = %+v, want the maildir destinations", payload.Curation)
	}
	if payload.Curation.Trash.Route != domain.RouteFixed {
		t.Errorf("route = %q, want %q for the dir backend", payload.Curation.Trash.Route, domain.RouteFixed)
	}
}

// The inbox UI's bridge must accept answers only from its host. Any frame
// that can post to it could otherwise resolve a pending call with a
// fabricated result, and the reply is rendered as mail — forging the
// answers is enough to put arbitrary content in front of the human, even
// though a foreign frame cannot invoke a tool.
func TestInboxUIAcceptsRepliesOnlyFromItsHost(t *testing.T) {
	client, _ := newClient(t)

	page, err := client.ReadResource(InboxUIResourceURI)
	if err != nil {
		t.Fatalf("read UI resource: %v", err)
	}
	if !strings.Contains(page, "ev.source !== window.parent") {
		t.Error("the bridge does not check the message source — any frame can answer a pending call")
	}
	// A sandboxed host reports origin "null", which identifies nobody, so
	// the wildcard has to survive for those. Pinning it would break the
	// bridge rather than tighten it.
	if !strings.Contains(page, "hostOrigin || '*'") {
		t.Error("outbound calls are not targeted at the learned host origin")
	}
	if !strings.Contains(page, `ev.origin !== 'null'`) {
		t.Error("a sandboxed host's \"null\" origin must not be pinned as the target")
	}
}

// TestInboxUIShowsTheOutbox covers the gap that made a failed send
// invisible: email.send/reply/forward return an id and then deliver
// asynchronously, so a message that never left was only discoverable by
// polling email.send_status for an id the UI had already forgotten.
func TestInboxUIShowsTheOutbox(t *testing.T) {
	client, _ := newClient(t)

	page, err := client.ReadResource(InboxUIResourceURI)
	if err != nil {
		t.Fatalf("read UI resource: %v", err)
	}
	for _, want := range []string{
		`id="outbox"`,      // the panel itself
		"'email://outbox'", // read as a resource, not polled per id
		"'email.retry'",    // ... and a failure can be re-queued from it
		"'failed'",         // failures are listed first, not buried under sent
	} {
		if !strings.Contains(page, want) {
			t.Errorf("inbox UI does not contain %q — a failed send stays invisible", want)
		}
	}
}
