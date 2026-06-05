package briefkasten

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

func promptServer(t *testing.T) *testutil.TestClient {
	t.Helper()
	mb, root := newDir(t)
	drop(t, root, "m1.eml", "From: spenden@drk.example\r\nSubject: Spende\r\n\r\nDanke für 85 EUR.")
	ob, err := NewOutbox(filepath.Join(t.TempDir(), "out"), &fakeSender{})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mb)
	RegisterResources(srv, mb, ob)
	RegisterPrompts(srv, mb)
	return testutil.NewTestClient(t, srv)
}

func TestPromptsListed(t *testing.T) {
	client := promptServer(t)
	prompts, err := client.ListPrompts()
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	names := map[string]bool{}
	for _, p := range prompts {
		names[p["name"].(string)] = true
	}
	for _, want := range []string{"summarize_inbox", "draft_reply"} {
		if !names[want] {
			t.Errorf("prompt %q missing (have %v)", want, names)
		}
	}
}

func TestSummarizeInboxEmbedsMessages(t *testing.T) {
	client := promptServer(t)
	result, err := client.GetPrompt("summarize_inbox", nil)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "Spende") {
		t.Errorf("prompt does not embed unread message: %s", raw)
	}
}

func TestDraftReplyEmbedsOriginal(t *testing.T) {
	client := promptServer(t)
	result, err := client.GetPrompt("draft_reply", map[string]string{"id": "m1.eml"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	raw, _ := json.Marshal(result)
	for _, want := range []string{"spenden@drk.example", "85 EUR"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("draft_reply missing %q: %s", want, raw)
		}
	}
}

func TestDraftReplyRequiresID(t *testing.T) {
	client := promptServer(t)
	if _, err := client.GetPrompt("draft_reply", nil); err == nil {
		t.Error("missing id accepted")
	}
}

func TestToolAnnotations(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Maildir = t.TempDir()
	cfg.Outbox.Dir = filepath.Join(t.TempDir(), "out")
	cfg.Outbox.From = "n@l.example"
	cfg.Outbox.DeliverDir = filepath.Join(t.TempDir(), "del")
	cfg.RuntimeConfig = true
	srv, _, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	readOnly := map[string]bool{}
	for _, tool := range srv.Tools() {
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint != nil {
			readOnly[tool.Name] = *tool.Annotations.ReadOnlyHint
		}
	}
	for _, want := range []string{"email.list_unread", "email.fetch", "email.send_status", "config.get"} {
		if !readOnly[want] {
			t.Errorf("%s should be annotated read-only (have %v)", want, readOnly)
		}
	}
	if readOnly["email.send"] || readOnly["email.mark_seen"] || readOnly["config.set"] {
		t.Errorf("mutating tools annotated read-only: %v", readOnly)
	}
}

func TestServerInstructions(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Maildir = t.TempDir()
	srv, _, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.Instructions(), "mark_seen") {
		t.Errorf("instructions = %q, want consumption contract explained", srv.Instructions())
	}
}
