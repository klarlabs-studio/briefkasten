package briefkasten

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

func TestInboxUIResource(t *testing.T) {
	mb, _ := newDir(t)
	srv := NewServer(mb)
	RegisterUI(srv)
	client := testutil.NewTestClient(t, srv)

	html, err := client.ReadResource(InboxUIResourceURI)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	for _, want := range []string{"<!doctype html", "email.list_unread", "email.mark_seen", "email.send", "tools/call"} {
		if !strings.Contains(html, want) {
			t.Errorf("UI missing %q", want)
		}
	}
}

func TestToolsCarryUIResourceMeta(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Maildir = t.TempDir()
	cfg.Outbox.Dir = filepath.Join(t.TempDir(), "out")
	cfg.Outbox.From = "n@l.example"
	cfg.Outbox.DeliverDir = filepath.Join(t.TempDir(), "del")
	srv, _, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	withUI := map[string]bool{}
	for _, tool := range srv.Tools() {
		if tool.Meta != nil {
			if uri, ok := tool.Meta["ui/resourceUri"].(string); ok && uri == InboxUIResourceURI {
				withUI[tool.Name] = true
			}
		}
	}
	for _, want := range []string{"email.list_unread", "email.send_status"} {
		if !withUI[want] {
			t.Errorf("%s missing ui resource meta (have %v)", want, withUI)
		}
	}
}
