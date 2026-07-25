package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/protocol"
	"go.klarlabs.de/mcp/server"
	"go.klarlabs.de/mcp/testutil"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

var errBoxBroken = errors.New("mailbox broken")

// brokenBox fails every operation — the adapter must surface the error,
// never swallow it.
type brokenBox struct{}

func (brokenBox) ListUnread(context.Context) ([]string, error)  { return nil, errBoxBroken }
func (brokenBox) Fetch(context.Context, string) ([]byte, error) { return nil, errBoxBroken }
func (brokenBox) MarkSeen(context.Context, string) error        { return errBoxBroken }
func (brokenBox) Folders(context.Context) ([]string, error)     { return nil, errBoxBroken }
func (brokenBox) InFolder(context.Context, string) (domain.Mailbox, error) {
	return nil, errBoxBroken
}
func (brokenBox) Archive(context.Context, string) error { return errBoxBroken }
func (brokenBox) Delete(context.Context, string) error  { return errBoxBroken }

// listOnlyBox lists one unread id but cannot fetch it — the summarize
// prompt must skip it rather than fail the whole summary.
type listOnlyBox struct{ brokenBox }

func (listOnlyBox) ListUnread(context.Context) ([]string, error) { return []string{"ghost.eml"}, nil }

// brokenStore fails every outbox persistence operation.
type brokenStore struct{}

func (brokenStore) Write(domain.OutboundMessage) error { return errBoxBroken }
func (brokenStore) Remove(string, string) error        { return errBoxBroken }
func (brokenStore) Find(string) (domain.OutboundMessage, error) {
	return domain.OutboundMessage{}, errBoxBroken
}
func (brokenStore) List(string) ([]string, error) { return nil, errBoxBroken }

func newBrokenClient(t *testing.T, opts ...Option) *testutil.TestClient {
	t.Helper()
	svc := application.NewService(brokenBox{}, nil)
	return testutil.NewTestClient(t, New(svc, opts...))
}

func TestToolErrorsSurface(t *testing.T) {
	client, _ := newClient(t, WithOutbox(newOutbox(t, &fakeSender{})))

	failing := []struct {
		tool string
		args map[string]any
	}{
		{"email.fetch", map[string]any{"id": "ghost.eml"}},
		{"email.mark_seen", map[string]any{"id": "ghost.eml"}},
		{"email.search", map[string]any{"query": "x", "account": "nope"}},
		{"email.send_status", map[string]any{"id": "ghost"}},
		{"email.send", map[string]any{"to": []string{}, "subject": "s", "body": "b"}},
		{"email.archive", map[string]any{"id": "ghost.eml", "confirm": true}},
		{"email.delete", map[string]any{"id": "ghost.eml", "confirm": true}},
	}
	for _, tc := range failing {
		resp, err := client.CallToolRaw(tc.tool, tc.args)
		if err == nil && (resp == nil || resp.Error == nil) {
			t.Errorf("%s(%v) did not surface an error", tc.tool, tc.args)
		}
	}
}

func TestArchiveToolConfirmAndExecute(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: Old\r\n\r\na")

	// Without confirmation the archive is refused and the message survives.
	if _, err := client.CallToolRaw("email.archive", map[string]any{"id": "a.eml"}); err == nil ||
		!strings.Contains(err.Error(), "confirmation required") {
		t.Fatalf("unconfirmed archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); err != nil {
		t.Fatal("message gone despite refusal")
	}

	out := callMap(t, client, "email.archive", map[string]any{"id": "a.eml", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("confirmed archive = %v", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".archive", "new", "a.eml")); err != nil {
		t.Errorf("not archived: %v", err)
	}
}

// fakeElicitSender scripts the client's answer to an elicitation
// request, and records the prompt the human would have been shown.
type fakeElicitSender struct {
	action string
	err    error
	prompt string
}

func (f *fakeElicitSender) SendRequest(_ context.Context, req *protocol.Request) (*protocol.Response, error) {
	var params struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(req.Params, &params); err == nil {
		f.prompt = params.Message
	}
	if f.err != nil {
		return nil, f.err
	}
	return &protocol.Response{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Result:  map[string]any{"action": f.action},
	}, nil
}

type noopNotifier struct{}

func (noopNotifier) SendNotification(string, any) error { return nil }

func elicitCtx(t *testing.T, sender server.RequestSender) context.Context {
	t.Helper()
	session := server.NewSession("s1", sender, noopNotifier{},
		server.WithClientCapabilities(server.ClientCapabilities{Elicitation: true}))
	return server.ContextWithSession(t.Context(), session)
}

func TestConfirmCurationElicitation(t *testing.T) {
	// The user accepts: the operation proceeds.
	ctx := elicitCtx(t, &fakeElicitSender{action: "accept"})
	if err := confirmCuration(ctx, false, "delete", []string{"m1.eml"}, "INBOX.Trash"); err != nil {
		t.Errorf("accepted elicitation = %v, want nil", err)
	}

	// The user declines: a non-retryable refusal naming the action.
	ctx = elicitCtx(t, &fakeElicitSender{action: "decline"})
	err := confirmCuration(ctx, false, "delete", []string{"m1.eml"}, "INBOX.Trash")
	if err == nil || !strings.Contains(err.Error(), "declined by user") {
		t.Errorf("declined elicitation = %v, want declined error", err)
	}

	// The elicitation transport fails: the model is told to ask the human.
	ctx = elicitCtx(t, &fakeElicitSender{err: errors.New("transport down")})
	err = confirmCuration(ctx, false, "delete", []string{"m1.eml"}, "INBOX.Trash")
	if err == nil || !strings.Contains(err.Error(), "confirmation elicitation failed") {
		t.Errorf("failed elicitation = %v, want elicitation-failed error", err)
	}

	// confirm=true bypasses elicitation entirely.
	if err := confirmCuration(t.Context(), true, "delete", []string{"m1.eml"}, "INBOX.Trash"); err != nil {
		t.Errorf("pre-confirmed = %v, want nil", err)
	}
}

func TestResourceErrorsSurface(t *testing.T) {
	client := newBrokenClient(t)
	for _, uri := range []string{
		"email://inbox",
		"email://inbox/ghost.eml",
		"email://folders",
	} {
		if _, err := client.ReadResource(uri); err == nil {
			t.Errorf("%s over a broken mailbox did not error", uri)
		}
	}
}

func TestOutboxResources(t *testing.T) {
	// Without an outbox: the summary reads empty, per-message reads error.
	client, _ := newClient(t)
	text, err := client.ReadResource("email://outbox")
	if err != nil || text != "{}" {
		t.Errorf("outbox without outbox = %q, %v; want {}", text, err)
	}
	if _, err := client.ReadResource("email://outbox/some-id"); err == nil ||
		!strings.Contains(err.Error(), "outbox not configured") {
		t.Errorf("outbox/{id} without outbox = %v, want not-configured error", err)
	}

	// With an outbox: a queued message is readable by id.
	ob := newOutbox(t, &fakeSender{})
	client, _ = newClient(t, WithOutbox(ob))
	id, err := ob.Enqueue(domain.OutboundMessage{To: []string{"a@b.c"}, Subject: "S", Body: "B"})
	if err != nil {
		t.Fatal(err)
	}
	text, err = client.ReadResource("email://outbox/" + id)
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(text), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["id"] != id || msg["state"] != "queued" {
		t.Errorf("outbox message = %v", msg)
	}
	if _, err := client.ReadResource("email://outbox/ghost"); err == nil {
		t.Error("unknown outbox id accepted")
	}

	// A failing store surfaces through the summary resource.
	broken := application.NewOutbox(brokenStore{}, &fakeSender{})
	client, _ = newClient(t, WithOutbox(broken))
	if _, err := client.ReadResource("email://outbox"); err == nil {
		t.Error("outbox summary over a broken store did not error")
	}
}

func TestParseHeadersKeepsUndecodableWord(t *testing.T) {
	raw := []byte("Subject: =?x-no-such-charset?q?geheim?=\r\n\r\nbody")
	got := parseHeaders(raw)
	if got["subject"] != "=?x-no-such-charset?q?geheim?=" {
		t.Errorf("subject = %q, want the raw encoded word kept verbatim", got["subject"])
	}
}

func TestJSONResourceMarshalError(t *testing.T) {
	if _, err := jsonResource("email://x", make(chan int)); err == nil {
		t.Error("unmarshalable payload accepted")
	}
}

func TestModuleVersionNeverEmpty(t *testing.T) {
	if v := moduleVersion(); v == "" {
		t.Error("moduleVersion returned an empty version")
	}
}
