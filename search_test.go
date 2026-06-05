package briefkasten

import (
	"bytes"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/felixgeelhaar/mcp-go/testutil"
)

type memLiteral struct {
	*bytes.Reader
	size int64
}

func (l memLiteral) Size() int64 { return l.size }

// startIMAPServerMulti runs an in-memory IMAP server with several unseen
// messages in INBOX.
func startIMAPServerMulti(t *testing.T, msgs map[string]string) string {
	t.Helper()
	user := imapmemserver.NewUser("alice", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for _, body := range msgs {
		raw := []byte(body)
		if _, err := user.Append("INBOX", memLiteral{bytes.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestDirMailboxSearch(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: drk@spenden.example\r\nSubject: Spende\r\n\r\nDanke für 85 EUR")
	drop(t, root, "b.eml", "From: shop@example.org\r\nSubject: Rechnung\r\n\r\nBetrag 12 EUR")

	ids, err := mb.Search("Spende")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("ids = %v", ids)
	}

	// Case-insensitive.
	ids, _ = mb.Search("rechnung")
	if len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("case-insensitive ids = %v", ids)
	}

	ids, _ = mb.Search("nirgends")
	if len(ids) != 0 {
		t.Errorf("no-match ids = %v", ids)
	}
}

func TestIMAPMailboxSearch(t *testing.T) {
	addr := startIMAPServerMulti(t, map[string]string{
		"spende":   "From: drk@spenden.example\r\nSubject: Spende\r\n\r\nDanke",
		"rechnung": "From: shop@example.org\r\nSubject: Rechnung\r\n\r\nBetrag",
	})
	mb, err := NewIMAPMailbox(IMAPConfig{Addr: addr, Username: "alice", Password: "secret", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}

	ids, serr := mb.Search("Rechnung")
	err = serr
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("ids = %v, want one match", ids)
	}
}

func TestSearchTool(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: drk@spenden.example\r\nSubject: Spende\r\n\r\nDanke")
	drop(t, root, "b.eml", "From: shop@example.org\r\nSubject: Rechnung\r\n\r\nBetrag")
	client := testutil.NewTestClient(t, NewServer(mb))

	out := callMap(t, client, "email.search", map[string]any{"query": "Spende"})
	ids := out["ids"].([]string)
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("ids = %v", ids)
	}
}

func TestSearchToolFallbackWithoutSearcher(t *testing.T) {
	// plainBox lacks Search: the tool falls back to list+fetch+contains.
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: x@y.z\r\nSubject: Spende\r\n\r\nDanke")
	client := testutil.NewTestClient(t, NewServer(plainBox{mb}))

	out := callMap(t, client, "email.search", map[string]any{"query": "spende"})
	ids := out["ids"].([]string)
	if len(ids) != 1 {
		t.Errorf("fallback ids = %v", ids)
	}

	out = callMap(t, client, "email.search", map[string]any{"query": "nirgends"})
	if n := len(out["ids"].([]string)); n != 0 {
		t.Errorf("fallback no-match = %d", n)
	}
}

// plainBox hides every capability beyond the base Mailbox.
type plainBox struct{ inner Mailbox }

func (p plainBox) ListUnread() ([]string, error) { return p.inner.ListUnread() }
func (p plainBox) Fetch(id string) ([]byte, error) {
	return p.inner.Fetch(id)
}
func (p plainBox) MarkSeen(id string) error { return p.inner.MarkSeen(id) }
