package maildir

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// seed drops two messages and marks one seen, leaving one in new/ and
// one in cur/.
func seedReadAndUnread(t *testing.T) *Mailbox {
	t.Helper()
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha body")
	drop(t, root, "b.eml", "From: a@b.c\r\nSubject: Beta\r\n\r\nbeta body")
	if err := mb.MarkSeen(t.Context(), "a.eml"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	return mb
}

func TestListByScope(t *testing.T) {
	mb := seedReadAndUnread(t)

	for _, tc := range []struct {
		scope domain.Scope
		want  []string
	}{
		{domain.ScopeUnread, []string{"b.eml"}},
		{domain.ScopeRead, []string{"a.eml"}},
		{domain.ScopeAll, []string{"a.eml", "b.eml"}},
	} {
		ids, err := mb.List(t.Context(), tc.scope)
		if err != nil {
			t.Fatalf("List(%s): %v", tc.scope, err)
		}
		if len(ids) != len(tc.want) {
			t.Fatalf("List(%s) = %v, want %v", tc.scope, ids, tc.want)
		}
		for i, id := range ids {
			if id != tc.want[i] {
				t.Fatalf("List(%s) = %v, want %v", tc.scope, ids, tc.want)
			}
		}
	}
}

func TestListRejectsUnknownScope(t *testing.T) {
	mb, _ := newDir(t)
	if _, err := mb.List(t.Context(), domain.Scope("archived")); !errors.Is(err, domain.ErrBadScope) {
		t.Fatalf("List(archived) error = %v, want ErrBadScope", err)
	}
}

// Reading a message must never change its state — the whole point of
// widening the scope is that it stays harmless.
func TestFetchReadMessageLeavesItRead(t *testing.T) {
	mb := seedReadAndUnread(t)

	raw, err := mb.Fetch(t.Context(), "a.eml")
	if err != nil {
		t.Fatalf("Fetch read message: %v", err)
	}
	if want := "alpha body"; !strings.Contains(string(raw), want) {
		t.Fatalf("Fetch = %q, want it to contain %q", raw, want)
	}

	unread, err := mb.List(t.Context(), domain.ScopeUnread)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(unread) != 1 || unread[0] != "b.eml" {
		t.Fatalf("unread after fetching read mail = %v, want [b.eml]", unread)
	}
}

func TestFetchUnknownIDStillFails(t *testing.T) {
	mb := seedReadAndUnread(t)
	if _, err := mb.Fetch(t.Context(), "nope.eml"); err == nil {
		t.Fatal("Fetch(nope.eml) = nil error, want failure")
	}
}

func TestSearchScopeCoversReadMail(t *testing.T) {
	mb := seedReadAndUnread(t)

	ids, err := mb.SearchScope(t.Context(), domain.ScopeRead, "alpha")
	if err != nil {
		t.Fatalf("SearchScope: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Fatalf("SearchScope(read, alpha) = %v, want [a.eml]", ids)
	}

	// The unread-only default must not see it.
	ids, err = mb.Search(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("Search(alpha) = %v, want none (it is read)", ids)
	}
}

// Curation reaches read mail too, and stays a soft move.
func TestArchiveReadMessage(t *testing.T) {
	mb := seedReadAndUnread(t)
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive read message: %v", err)
	}
	ids, err := mb.List(t.Context(), domain.ScopeAll)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 1 || ids[0] != "b.eml" {
		t.Fatalf("after archive, all = %v, want [b.eml]", ids)
	}
}

func TestDeleteReadMessage(t *testing.T) {
	mb := seedReadAndUnread(t)
	if err := mb.Delete(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Delete read message: %v", err)
	}
	ids, err := mb.List(t.Context(), domain.ScopeAll)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 1 || ids[0] != "b.eml" {
		t.Fatalf("after delete, all = %v, want [b.eml]", ids)
	}
}

// Acknowledging twice is not an error: the second MarkSeen finds the
// message in cur/ and leaves it there.
func TestMarkSeenIsIdempotent(t *testing.T) {
	mb := seedReadAndUnread(t)

	if err := mb.MarkSeen(t.Context(), "a.eml"); err != nil {
		t.Fatalf("second MarkSeen: %v", err)
	}
	read, err := mb.List(t.Context(), domain.ScopeRead)
	if err != nil {
		t.Fatalf("List(read): %v", err)
	}
	if len(read) != 1 || read[0] != "a.eml" {
		t.Fatalf("read = %v, want [a.eml]", read)
	}
	raw, err := mb.Fetch(t.Context(), "a.eml")
	if err != nil {
		t.Fatalf("Fetch after re-mark: %v", err)
	}
	if !strings.Contains(string(raw), "alpha body") {
		t.Errorf("message content changed: %q", raw)
	}
}

// A message in neither new/ nor cur/ is the caller's mistake, so it
// reports ErrBadID — which resilience never retries and never counts
// against the backend's health.
func TestMarkSeenUnknownIDIsBadID(t *testing.T) {
	mb := seedReadAndUnread(t)

	err := mb.MarkSeen(t.Context(), "nope.eml")
	if err == nil {
		t.Fatal("MarkSeen of unknown id accepted")
	}
	if !errors.Is(err, domain.ErrBadID) {
		t.Errorf("MarkSeen error = %v, want ErrBadID", err)
	}
}

// The dir backend cannot be interrupted mid-syscall, so its whole
// context contract is the check at the door: a caller who has already
// given up gets a context error and the mailbox is not touched. The
// distinction matters upstream — resilience must not read this as the
// disk being broken.
func TestDirMailboxRefusesCancelledContext(t *testing.T) {
	mb := seedReadAndUnread(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for name, op := range map[string]func() error{
		"List":         func() error { _, err := mb.List(ctx, domain.ScopeAll); return err },
		"Fetch":        func() error { _, err := mb.Fetch(ctx, "a.eml"); return err },
		"MarkSeen":     func() error { return mb.MarkSeen(ctx, "b.eml") },
		"Folders":      func() error { _, err := mb.Folders(ctx); return err },
		"InFolder":     func() error { _, err := mb.InFolder(ctx, "work"); return err },
		"Archive":      func() error { return mb.Archive(ctx, "b.eml") },
		"Delete":       func() error { return mb.Delete(ctx, "b.eml") },
		"CurationPlan": func() error { _, err := mb.CurationPlan(ctx); return err },
	} {
		if err := op(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v, want it to wrap context.Canceled", name, err)
		}
	}

	// Nothing moved: the refusals were refusals, not half-done work.
	all, err := mb.List(t.Context(), domain.ScopeAll)
	if err != nil {
		t.Fatalf("List after the cancelled calls: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("mailbox = %v, want both messages untouched", all)
	}
}
