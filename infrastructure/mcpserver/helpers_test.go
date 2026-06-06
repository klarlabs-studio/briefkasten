package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/mcp/testutil"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
)

// newDir prepares a maildir backend in a temp dir.
func newDir(t *testing.T) (*maildir.Mailbox, string) {
	t.Helper()
	root := t.TempDir()
	mb, err := maildir.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return mb, root
}

// drop places an unread .eml into the maildir.
func drop(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "new", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeSender records deliveries.
type fakeSender struct{ sent []domain.OutboundMessage }

func (f *fakeSender) Send(_ context.Context, msg domain.OutboundMessage) error {
	f.sent = append(f.sent, msg)
	return nil
}

// newOutbox builds an application outbox over a temp dir store.
func newOutbox(t *testing.T, sender domain.Sender) *application.Outbox {
	t.Helper()
	store, err := maildir.NewOutboxStore(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	return application.NewOutbox(store, sender)
}

// newClient builds a test client over a maildir-backed server.
func newClient(t *testing.T, opts ...Option) (*testutil.TestClient, string) {
	t.Helper()
	mb, root := newDir(t)
	svc := application.NewService(mb, nil)
	return testutil.NewTestClient(t, New(svc, opts...)), root
}

// callMap invokes a tool via the test client and returns the handler's
// map result (testutil passes handler return values through unserialized).
func callMap(t *testing.T, client *testutil.TestClient, name string, args map[string]any) map[string]any {
	t.Helper()
	resp, err := client.CallToolRaw(name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if resp.Error != nil {
		t.Fatalf("%s: %v", name, resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	out, ok := content[0]["text"].(map[string]any)
	if !ok {
		t.Fatalf("%s: unexpected payload %T", name, content[0]["text"])
	}
	return out
}
