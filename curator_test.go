package briefkasten

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

func TestDirMailboxArchiveAndDelete(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: x@y\r\n\r\na")
	drop(t, root, "b.eml", "From: x@y\r\n\r\nb")

	if err := mb.Archive("a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".archive", "new", "a.eml")); err != nil {
		t.Errorf("archived file missing: %v", err)
	}

	if err := mb.Delete("b.eml"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "new", "b.eml")); err != nil {
		t.Errorf("trashed file missing: %v", err)
	}

	ids, _ := mb.ListUnread()
	if len(ids) != 0 {
		t.Errorf("unread after curation = %v", ids)
	}

	// Dot-dirs stay hidden from folders.
	folders, _ := mb.Folders()
	for _, f := range folders {
		if f == ".archive" || f == ".trash" {
			t.Errorf("hidden folder leaked: %v", folders)
		}
	}

	// Traversal rejected.
	if err := mb.Delete("../../etc/passwd"); err == nil {
		t.Error("traversal delete accepted")
	}
}

func TestIMAPMailboxArchiveAndDelete(t *testing.T) {
	user := imapmemserver.NewUser("alice", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"From: a@b\r\nSubject: One\r\n\r\n1", "From: a@b\r\nSubject: Two\r\n\r\n2"} {
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

	mb, err := NewIMAPMailbox(IMAPConfig{Addr: ln.Addr().String(), Username: "alice", Password: "secret", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}

	ids, err := mb.ListUnread()
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids = %v err = %v", ids, err)
	}

	// Archive creates the folder when missing and moves the message.
	if err := mb.Archive(ids[0]); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := mb.Delete(ids[1]); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	remaining, _ := mb.ListUnread()
	if len(remaining) != 0 {
		t.Errorf("unread after curation = %v", remaining)
	}

	folders, err := mb.Folders()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range folders {
		have[f] = true
	}
	if !have["Archive"] || !have["Trash"] {
		t.Errorf("folders = %v, want Archive + Trash created", folders)
	}
}

func TestWrappersForwardCurator(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: x@y\r\n\r\na")
	drop(t, root, "b.eml", "From: x@y\r\n\r\nb")

	sw := NewSwitchable(mb)
	if err := sw.Archive("a.eml"); err != nil {
		t.Errorf("Switchable.Archive: %v", err)
	}

	r := Resilient(mb, ResilienceConfig{InitialDelay: time.Millisecond})
	if err := r.Delete("b.eml"); err != nil {
		t.Errorf("Resilient.Delete: %v", err)
	}
}
