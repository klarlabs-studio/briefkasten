package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

func newDir(t *testing.T) (*Mailbox, string) {
	t.Helper()
	root := t.TempDir()
	mb, err := New(root)
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

	ids, err := mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("unread = %d, want 2", len(ids))
	}

	raw, err := mb.Fetch(t.Context(), ids[0])
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("empty message")
	}

	if err := mb.MarkSeen(t.Context(), ids[0]); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	ids2, _ := mb.ListUnread(t.Context())
	if len(ids2) != 1 {
		t.Errorf("unread after seen = %d, want 1", len(ids2))
	}
	if _, err := os.Stat(filepath.Join(root, "cur", ids[0])); err != nil {
		t.Errorf("seen message not in cur/: %v", err)
	}
}

func TestDirMailboxRejectsTraversal(t *testing.T) {
	mb, _ := newDir(t)
	if _, err := mb.Fetch(t.Context(), "../secrets"); err == nil {
		t.Error("path traversal accepted in Fetch")
	}
	if err := mb.MarkSeen(t.Context(), "../../etc/passwd"); err == nil {
		t.Error("path traversal accepted in MarkSeen")
	}
}

// An id in neither new/ nor cur/ is the caller's mistake: resilience
// keys off ErrBadID to keep it off the retry path and out of the circuit
// breaker, so a bad id must never look like backend ill-health.
func TestDirMailboxFetchUnknownIsBadID(t *testing.T) {
	mb, _ := newDir(t)

	_, err := mb.Fetch(t.Context(), "nope.eml")
	if err == nil {
		t.Fatal("unknown id accepted")
	}
	if !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Fetch error = %v, want ErrBadID", err)
	}
}

// The mirror image: an unreadable mailbox is the backend failing. If it
// were flattened into ErrBadID the retry and breaker would never see it,
// and the operator would be told their id was wrong.
func TestDirMailboxFetchIOErrorIsNotBadID(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	mb, root := newDir(t)
	drop(t, root, "a.eml", "hi")
	if err := mb.MarkSeen(t.Context(), "a.eml"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	// Unreadable in cur/, absent from new/: the new/ miss must not mask
	// the real failure behind it.
	if err := os.Chmod(filepath.Join(root, "cur", "a.eml"), 0o000); err != nil {
		t.Fatal(err)
	}

	_, err := mb.Fetch(t.Context(), "a.eml")
	if err == nil {
		t.Fatal("Fetch of an unreadable message succeeded")
	}
	if errors.Is(err, domain.ErrBadID) {
		t.Errorf("Fetch error = %v, want a real I/O error, not ErrBadID", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("Fetch error = %v, want it to carry the permission error", err)
	}
}
