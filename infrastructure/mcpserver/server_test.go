package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/testutil"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

func TestToolContractRoundTrip(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "msg1.eml", "From: a@b.c\r\nSubject: Quittung\r\n\r\nhi")

	tools, err := client.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"email.list_unread", "email.fetch", "email.mark_seen", "email.search", "email.archive", "email.delete"} {
		if !names[want] {
			t.Errorf("tool %q missing", want)
		}
	}

	listed := callMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "msg1.eml" {
		t.Fatalf("ids = %v", ids)
	}

	fetched := callMap(t, client, "email.fetch", map[string]any{"id": "msg1.eml"})
	raw, err := base64.StdEncoding.DecodeString(fetched["raw"].(string))
	if err != nil || !strings.HasPrefix(string(raw), "From:") {
		t.Errorf("raw = %q err = %v", raw, err)
	}

	found := callMap(t, client, "email.search", map[string]any{"query": "quittung"})
	if n := len(found["ids"].([]string)); n != 1 {
		t.Errorf("search = %d", n)
	}

	callMap(t, client, "email.mark_seen", map[string]any{"id": "msg1.eml"})
	listed = callMap(t, client, "email.list_unread", map[string]any{})
	if n := len(listed["ids"].([]string)); n != 0 {
		t.Errorf("unread after seen = %d", n)
	}
}

func TestFolderAndAccountRouting(t *testing.T) {
	mb, root := newDir(t)
	if err := os.MkdirAll(filepath.Join(root, "steuern", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "steuern", "new", "s.eml"),
		[]byte("From: amt@fa.example\r\nSubject: Bescheid\r\n\r\nSteuer"), 0o644); err != nil {
		t.Fatal(err)
	}
	business, bizRoot := newDir(t)
	drop(t, bizRoot, "b.eml", "From: c@d\r\nSubject: Business\r\n\r\nx")

	svc := application.NewService(mb, map[string]domain.Mailbox{"business": business})
	client := testutil.NewTestClient(t, New(svc))

	out := callMap(t, client, "email.list_unread", map[string]any{"folder": "steuern"})
	if ids := out["ids"].([]string); len(ids) != 1 || ids[0] != "s.eml" {
		t.Errorf("folder ids = %v", ids)
	}
	out = callMap(t, client, "email.list_unread", map[string]any{"account": "business"})
	if ids := out["ids"].([]string); len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("account ids = %v", ids)
	}
	if _, err := client.CallToolRaw("email.list_unread", map[string]any{"account": "nope"}); err == nil {
		t.Error("unknown account accepted")
	}
}

func TestCurationHITL(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: Old\r\n\r\na")

	if _, err := client.CallToolRaw("email.delete", map[string]any{"id": "a.eml"}); err == nil || !strings.Contains(err.Error(), "confirmation required") {
		t.Fatalf("unconfirmed delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); err != nil {
		t.Fatal("message gone despite refusal")
	}
	out := callMap(t, client, "email.delete", map[string]any{"id": "a.eml", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("confirmed delete = %v", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "new", "a.eml")); err != nil {
		t.Errorf("not in trash: %v", err)
	}
}

func TestSendToolsAndOutboxResources(t *testing.T) {
	sender := &fakeSender{}
	ob := newOutbox(t, sender)
	mb, _ := newDir(t)
	svc := application.NewService(mb, nil)
	client := testutil.NewTestClient(t, New(svc, WithOutbox(ob)))

	sent := callMap(t, client, "email.send", map[string]any{
		"to": []string{"x@y.z"}, "subject": "s", "body": "b",
	})
	id, _ := sent["id"].(string)
	if id == "" || sent["state"] != "queued" {
		t.Fatalf("send = %v", sent)
	}
	if _, err := ob.ProcessOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := callMap(t, client, "email.send_status", map[string]any{"id": id})
	if status["state"] != "sent" {
		t.Errorf("status = %v", status)
	}

	text, err := client.ReadResource("email://outbox")
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string][]string
	if err := json.Unmarshal([]byte(text), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary["sent"]) != 1 {
		t.Errorf("summary = %v", summary)
	}
}

func TestResourcesPromptsUI(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "m1.eml", "From: spenden@drk.example\r\nSubject: Spende\r\n\r\nDanke f\u00fcr 85 EUR.")

	text, err := client.ReadResource("email://inbox")
	if err != nil || !strings.Contains(text, "m1.eml") {
		t.Errorf("inbox = %q err = %v", text, err)
	}
	text, err = client.ReadResource("email://inbox/m1.eml")
	if err != nil || !strings.Contains(text, "Subject: Spende") {
		t.Errorf("message = %q err = %v", text, err)
	}
	text, err = client.ReadResource("email://folders")
	if err != nil || !strings.Contains(text, "INBOX") {
		t.Errorf("folders = %q err = %v", text, err)
	}
	text, err = client.ReadResource("email://accounts")
	if err != nil || !strings.Contains(text, "default") {
		t.Errorf("accounts = %q err = %v", text, err)
	}
	html, err := client.ReadResource(InboxUIResourceURI)
	if err != nil || !strings.HasPrefix(html, "<!doctype html") {
		t.Errorf("ui err = %v", err)
	}

	prompts, err := client.ListPrompts()
	if err != nil || len(prompts) != 2 {
		t.Errorf("prompts = %v err = %v", prompts, err)
	}
	result, err := client.GetPrompt("summarize_inbox", nil)
	raw, _ := json.Marshal(result)
	if err != nil || !strings.Contains(string(raw), "Spende") {
		t.Errorf("summarize err = %v", err)
	}
}

func TestAnnotationsAndMeta(t *testing.T) {
	mb, _ := newDir(t)
	srv := New(application.NewService(mb, nil), WithOutbox(newOutbox(t, &fakeSender{})))

	readOnly, destructive, withUI := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, tool := range srv.Tools() {
		if tool.Annotations != nil {
			if tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint {
				readOnly[tool.Name] = true
			}
			if tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
				destructive[tool.Name] = true
			}
		}
		if tool.Meta != nil {
			if uri, ok := tool.Meta["ui/resourceUri"].(string); ok && uri == InboxUIResourceURI {
				withUI[tool.Name] = true
			}
		}
	}
	for _, want := range []string{"email.list_unread", "email.fetch", "email.search", "email.send_status"} {
		if !readOnly[want] {
			t.Errorf("%s not read-only", want)
		}
	}
	for _, want := range []string{"email.archive", "email.delete"} {
		if !destructive[want] {
			t.Errorf("%s not destructive", want)
		}
	}
	if !withUI["email.list_unread"] || !withUI["email.send_status"] {
		t.Errorf("ui meta missing: %v", withUI)
	}
	if !strings.Contains(Instructions, "confirm") {
		t.Error("instructions silent on confirmation")
	}
}
