package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// failingSender refuses every delivery so messages end in failed state.
type failingSender struct{ calls int }

func (f *failingSender) Send(context.Context, domain.OutboundMessage) error {
	f.calls++
	return errors.New("smtp down")
}

func TestListUnreadLimit(t *testing.T) {
	client, root := newClient(t)
	for i := 1; i <= 3; i++ {
		drop(t, root, fmt.Sprintf("m%d.eml", i), fmt.Sprintf("From: a@b.c\r\nSubject: Bericht %d\r\n\r\nhi", i))
	}

	out := callMap(t, client, "email.list_unread", map[string]any{"limit": 2})
	if ids := out["ids"].([]string); len(ids) != 2 {
		t.Errorf("limited ids = %v, want 2", ids)
	}
	if out["total"] != 3 {
		t.Errorf("total = %v, want 3 despite limit", out["total"])
	}

	out = callMap(t, client, "email.list_unread", map[string]any{})
	if ids := out["ids"].([]string); len(ids) != 3 {
		t.Errorf("unlimited ids = %v, want 3", ids)
	}
	if out["total"] != 3 {
		t.Errorf("total = %v, want 3", out["total"])
	}
}

func TestSearchLimit(t *testing.T) {
	client, root := newClient(t)
	for i := 1; i <= 3; i++ {
		drop(t, root, fmt.Sprintf("m%d.eml", i), fmt.Sprintf("From: a@b.c\r\nSubject: Bericht %d\r\n\r\nQuittung", i))
	}

	out := callMap(t, client, "email.search", map[string]any{"query": "quittung", "limit": 2})
	if ids := out["ids"].([]string); len(ids) != 2 {
		t.Errorf("limited ids = %v, want 2", ids)
	}
	if out["total"] != 3 {
		t.Errorf("total = %v, want 3 despite limit", out["total"])
	}

	out = callMap(t, client, "email.search", map[string]any{"query": "quittung"})
	if ids := out["ids"].([]string); len(ids) != 3 {
		t.Errorf("unlimited ids = %v, want 3", ids)
	}
	if out["total"] != 3 {
		t.Errorf("total = %v, want 3", out["total"])
	}
}

func TestRetryToolRequeuesFailedSend(t *testing.T) {
	sender := &failingSender{}
	ob := newOutbox(t, sender)
	client, _ := newClient(t, WithOutbox(ob))

	sent := callMap(t, client, "email.send", map[string]any{
		"to": []string{"x@y.z"}, "subject": "s", "body": "b", "confirm": true,
	})
	id, _ := sent["id"].(string)
	if id == "" {
		t.Fatalf("send = %v", sent)
	}
	if _, err := ob.ProcessOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := callMap(t, client, "email.send_status", map[string]any{"id": id})
	if status["state"] != "failed" {
		t.Fatalf("status after failing delivery = %v", status)
	}

	out := callMap(t, client, "email.retry", map[string]any{"id": id})
	if out["state"] != "queued" {
		t.Errorf("retry = %v, want queued", out)
	}
	status = callMap(t, client, "email.send_status", map[string]any{"id": id})
	if status["state"] != "queued" {
		t.Errorf("status after retry = %v", status)
	}

	resp, err := client.CallToolRaw("email.retry", map[string]any{"id": "nope"})
	if err == nil && (resp == nil || resp.Error == nil) {
		t.Error("retry of unknown id accepted")
	}
}

func TestSummarizeInboxCount(t *testing.T) {
	client, root := newClient(t)
	for i := 1; i <= 3; i++ {
		drop(t, root, fmt.Sprintf("m%d.eml", i), fmt.Sprintf("From: a@b.c\r\nSubject: Post %d\r\n\r\nhallo", i))
	}

	result, err := client.GetPrompt("summarize_inbox", map[string]string{"count": "1"})
	if err != nil {
		t.Fatalf("summarize_inbox: %v", err)
	}
	raw, _ := json.Marshal(result)
	text := string(raw)
	if got := strings.Count(text, "--- Message"); got != 1 {
		t.Errorf("embedded messages = %d, want exactly 1", got)
	}
	if !strings.Contains(text, "more unread messages not shown") {
		t.Errorf("prompt silent about the %d hidden messages: %s", 2, text)
	}

	if _, err := client.GetPrompt("summarize_inbox", map[string]string{"count": "abc"}); err == nil {
		t.Error("non-numeric count accepted")
	}
}
