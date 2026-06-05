package briefkasten

import (
	"os"
	"path/filepath"
	"testing"
)

func newDir(t *testing.T) (*DirMailbox, string) {
	t.Helper()
	root := t.TempDir()
	mb, err := NewDirMailbox(root)
	if err != nil {
		t.Fatalf("NewDirMailbox: %v", err)
	}
	return mb, root
}

func drop(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "new", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirMailboxListFetchMarkSeen(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: A\r\n\r\nhi")
	drop(t, root, "b.eml", "From: a@b.c\r\nSubject: B\r\n\r\nhi")

	ids, err := mb.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("unread = %d, want 2", len(ids))
	}

	raw, err := mb.Fetch(ids[0])
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("empty message")
	}

	if err := mb.MarkSeen(ids[0]); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	ids2, _ := mb.ListUnread()
	if len(ids2) != 1 {
		t.Errorf("unread after seen = %d, want 1", len(ids2))
	}
	if _, err := os.Stat(filepath.Join(root, "cur", ids[0])); err != nil {
		t.Errorf("seen message not in cur/: %v", err)
	}
}

func TestDirMailboxRejectsTraversal(t *testing.T) {
	mb, _ := newDir(t)
	if _, err := mb.Fetch("../secrets"); err == nil {
		t.Error("path traversal accepted in Fetch")
	}
	if err := mb.MarkSeen("../../etc/passwd"); err == nil {
		t.Error("path traversal accepted in MarkSeen")
	}
}

func TestDirMailboxFetchUnknown(t *testing.T) {
	mb, _ := newDir(t)
	if _, err := mb.Fetch("nope.eml"); err == nil {
		t.Error("unknown id accepted")
	}
}
