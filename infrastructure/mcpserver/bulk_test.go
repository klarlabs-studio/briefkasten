package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// bulkIDs renders n ids, for the cap check.
func bulkIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d.eml", i)
	}
	return ids
}

// failureIDs pulls the ids out of a tool's failed list.
func failureIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, ok := out["failed"].([]map[string]any)
	if !ok {
		t.Fatalf("response has no failed list: %v", out)
	}
	ids := make([]string, 0, len(raw))
	for _, f := range raw {
		ids = append(ids, f["id"].(string))
	}
	return ids
}

// A batch is not all-or-nothing, and the response must say so: the ids
// that moved are named, the ones that did not are named with a reason,
// and there is no blanket ok for the caller to read as "all done".
func TestBulkCurationReportsPerID(t *testing.T) {
	for _, tc := range []struct {
		tool string
		verb string
		dir  string
	}{
		{"email.archive", "archived", ".archive"},
		{"email.delete", "deleted", ".trash"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			client, root := newClient(t)
			drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")
			drop(t, root, "b.eml", "From: x@y\r\nSubject: B\r\n\r\nb")

			out := callMap(t, client, tc.tool, map[string]any{
				"ids": []string{"a.eml", "ghost.eml", "b.eml"}, "confirm": true,
			})
			if moved := out[tc.verb].([]string); !slices.Equal(moved, []string{"a.eml", "b.eml"}) {
				t.Errorf("%s = %v, want the two real messages", tc.verb, moved)
			}
			if got := failureIDs(t, out); !slices.Equal(got, []string{"ghost.eml"}) {
				t.Errorf("failed = %v, want only ghost.eml", got)
			}
			if _, claimed := out["ok"]; claimed {
				t.Error("a partly failed batch reported ok — the whole point is that it cannot")
			}
			if out["total"] != 3 {
				t.Errorf("total = %v, want 3", out["total"])
			}
			for _, name := range []string{"a.eml", "b.eml"} {
				if _, err := os.Stat(filepath.Join(root, tc.dir, "new", name)); err != nil {
					t.Errorf("%s not filed into %s: %v", name, tc.dir, err)
				}
			}
		})
	}
}

// Bulk mark-seen keeps the single-message guarantee: acknowledging read
// mail again succeeds, and an id the mailbox never held is reported
// rather than swallowed.
func TestBulkMarkSeenIdempotentAndPerID(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")
	drop(t, root, "b.eml", "From: x@y\r\nSubject: B\r\n\r\nb")
	ids := []string{"a.eml", "b.eml"}

	for i := range 2 {
		out := callMap(t, client, "email.mark_seen", map[string]any{"ids": ids})
		if marked := out["marked"].([]string); !slices.Equal(marked, ids) {
			t.Fatalf("pass %d marked = %v, want both", i, marked)
		}
		if failed := failureIDs(t, out); len(failed) != 0 {
			t.Fatalf("pass %d failed = %v, want none", i, failed)
		}
	}

	out := callMap(t, client, "email.mark_seen", map[string]any{"ids": []string{"a.eml", "ghost.eml"}})
	if got := failureIDs(t, out); !slices.Equal(got, []string{"ghost.eml"}) {
		t.Errorf("failed = %v, want only ghost.eml", got)
	}
}

// One id keeps the single-message contract it always had, so a client
// that never heard of batches is unaffected.
func TestSingleIDFormUnchanged(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")

	if out := callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"}); out["ok"] != true {
		t.Errorf("single mark_seen = %v, want ok", out)
	}
	if out := callMap(t, client, "email.archive", map[string]any{"id": "a.eml", "confirm": true}); out["ok"] != true {
		t.Errorf("single archive = %v, want ok", out)
	}
}

// Exactly one of id and ids: both would leave the server guessing which
// the human approved, neither is a destructive tool with no target.
func TestIDAndIDsAreExclusive(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")

	for _, tool := range []string{"email.mark_seen", "email.archive", "email.delete"} {
		_, err := client.CallToolRaw(tool, map[string]any{
			"id": "a.eml", "ids": []string{"a.eml"}, "confirm": true,
		})
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Errorf("%s with id and ids = %v, want a both-supplied refusal", tool, err)
		}
		_, err = client.CallToolRaw(tool, map[string]any{"confirm": true})
		if err == nil || !strings.Contains(err.Error(), "no message named") {
			t.Errorf("%s with neither = %v, want a no-target refusal", tool, err)
		}
	}
	// Nothing moved on the way through the refusals.
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); err != nil {
		t.Errorf("message disturbed by refused calls: %v", err)
	}
}

// The cap bounds what a single confirmation can authorise. It is refused
// with the number named, never trimmed to the first hundred.
func TestBulkCapIsEnforcedAtTheTool(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")

	for _, tool := range []string{"email.mark_seen", "email.archive", "email.delete"} {
		_, err := client.CallToolRaw(tool, map[string]any{
			"ids": bulkIDs(domain.MaxBulkIDs + 1), "confirm": true,
		})
		if err == nil || !strings.Contains(err.Error(), "100") {
			t.Errorf("%s over the cap = %v, want a refusal naming the cap", tool, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); err != nil {
		t.Errorf("message disturbed by an over-cap batch: %v", err)
	}
}

// A batch is one human gesture authorising many destructive moves, so it
// is gated exactly like a single one — and refused just as hard without
// confirmation.
func TestBulkCurationRefusedWithoutConfirmation(t *testing.T) {
	for _, tool := range []string{"email.archive", "email.delete"} {
		client, root := newClient(t)
		drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")
		drop(t, root, "b.eml", "From: x@y\r\nSubject: B\r\n\r\nb")

		_, err := client.CallToolRaw(tool, map[string]any{"ids": []string{"a.eml", "b.eml"}})
		if err == nil || !strings.Contains(err.Error(), "confirmation required") {
			t.Errorf("unconfirmed %s = %v, want a confirmation refusal", tool, err)
		}
		for _, name := range []string{"a.eml", "b.eml"} {
			if _, err := os.Stat(filepath.Join(root, "new", name)); err != nil {
				t.Errorf("%s moved despite the refusal: %v", name, err)
			}
		}
	}
}

// What the human is asked has to state the blast radius the one "yes"
// covers: how many messages, where they are going, and which ones.
func TestBulkConfirmationPromptNamesCountAndDestination(t *testing.T) {
	sender := &fakeElicitSender{action: "accept"}
	ctx := elicitCtx(t, sender)
	ids := []string{"a.eml", "b.eml", "c.eml"}

	if err := confirmCuration(ctx, false, "delete", ids, ".trash"); err != nil {
		t.Fatalf("confirmCuration: %v", err)
	}
	for _, want := range []string{"3 messages", ".trash", "never destroyed", "a.eml, b.eml, c.eml"} {
		if !strings.Contains(sender.prompt, want) {
			t.Errorf("prompt = %q, want it to contain %q", sender.prompt, want)
		}
	}

	// A long batch still states the count; the ids are summarised rather
	// than turned into a wall the human scrolls past.
	sender = &fakeElicitSender{action: "accept"}
	if err := confirmCuration(elicitCtx(t, sender), false, "archive", bulkIDs(30), "Archive"); err != nil {
		t.Fatalf("confirmCuration: %v", err)
	}
	for _, want := range []string{"30 messages", "Archive", "and 20 more"} {
		if !strings.Contains(sender.prompt, want) {
			t.Errorf("prompt = %q, want it to contain %q", sender.prompt, want)
		}
	}

	// Declining a batch is refused as one action, naming the count.
	sender = &fakeElicitSender{action: "decline"}
	err := confirmCuration(elicitCtx(t, sender), false, "delete", ids, ".trash")
	if err == nil || !strings.Contains(err.Error(), "3 messages") {
		t.Errorf("declined batch = %v, want a refusal naming the count", err)
	}
}

// The destination in the prompt is the real one the backend would file
// into, not a guess — that is the whole reason it is asked for.
func TestCurationDestinationComesFromThePlan(t *testing.T) {
	mb, _ := newDir(t)
	svc := newServiceOver(mb)

	if got := curationDestination(t.Context(), svc, false, "", "", "delete"); got != ".trash" {
		t.Errorf("delete destination = %q, want .trash", got)
	}
	if got := curationDestination(t.Context(), svc, false, "", "", "archive"); got != ".archive" {
		t.Errorf("archive destination = %q, want .archive", got)
	}
	// An already-confirmed call must not pay for a round trip it cannot use.
	if got := curationDestination(t.Context(), svc, true, "", "", "delete"); got != "" {
		t.Errorf("pre-confirmed destination = %q, want none", got)
	}
}

// An oversized batch must be refused before the human is asked to
// approve it — a prompt for 101 messages that then errors wastes the one
// gesture the gate exists to spend well.
func TestBulkCapRefusedBeforeElicitation(t *testing.T) {
	mb, _ := newDir(t)
	srv := New(newServiceOver(mb))
	tool, ok := srv.GetTool("email.delete")
	if !ok {
		t.Fatal("email.delete not registered")
	}
	sender := &fakeElicitSender{action: "accept"}
	args, err := json.Marshal(map[string]any{"ids": bulkIDs(domain.MaxBulkIDs + 1)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = tool.Execute(elicitCtx(t, sender), args)
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("over-cap delete = %v, want a refusal naming the cap", err)
	}
	if sender.prompt != "" {
		t.Errorf("the human was asked before the batch was rejected: %q", sender.prompt)
	}
}
