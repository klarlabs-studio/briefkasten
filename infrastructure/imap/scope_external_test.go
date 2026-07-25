package imap_test

import (
	"errors"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// After MarkSeen the message leaves the unread scope but stays reachable
// through read and all — that is the whole point of scoped listing.
func TestIMAPListByScope(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	unread, err := mb.List(domain.ScopeUnread)
	if err != nil {
		t.Fatalf("List(unread): %v", err)
	}
	if len(unread) != 1 {
		t.Fatalf("unread = %v, want one id", unread)
	}
	id := unread[0]

	read, err := mb.List(domain.ScopeRead)
	if err != nil {
		t.Fatalf("List(read): %v", err)
	}
	if len(read) != 0 {
		t.Fatalf("read = %v, want none before MarkSeen", read)
	}

	if err := mb.MarkSeen(id); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	read, err = mb.List(domain.ScopeRead)
	if err != nil {
		t.Fatalf("List(read) after seen: %v", err)
	}
	if len(read) != 1 || read[0] != id {
		t.Fatalf("read after seen = %v, want [%s]", read, id)
	}

	all, err := mb.List(domain.ScopeAll)
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 1 || all[0] != id {
		t.Fatalf("all = %v, want [%s]", all, id)
	}

	// Fetching read mail still peeks: it must not disturb the flag.
	if _, err := mb.Fetch(id); err != nil {
		t.Fatalf("Fetch read message: %v", err)
	}
	unread, err = mb.List(domain.ScopeUnread)
	if err != nil {
		t.Fatalf("List(unread) after fetch: %v", err)
	}
	if len(unread) != 0 {
		t.Fatalf("unread after fetching read mail = %v, want none", unread)
	}
}

func TestIMAPListRejectsUnknownScope(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))
	if _, err := mb.List(domain.Scope("spam")); !errors.Is(err, domain.ErrBadScope) {
		t.Fatalf("List(spam) error = %v, want ErrBadScope", err)
	}
}

func TestIMAPSearchScope(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	ids, err := mb.List(domain.ScopeUnread)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mb.MarkSeen(ids[0]); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	hits, err := mb.SearchScope(domain.ScopeRead, "Bescheid")
	if err != nil {
		t.Fatalf("SearchScope(read): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchScope(read, Bescheid) = %v, want one hit", hits)
	}

	hits, err = mb.Search("Bescheid")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("Search(Bescheid) = %v, want none (it is read)", hits)
	}
}

// Curation over IMAP is UID-based, so a message that is already read
// archives exactly like an unread one — and lands in Archive.
func TestIMAPArchiveReadMessage(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	unread, err := mb.List(domain.ScopeUnread)
	if err != nil || len(unread) != 1 {
		t.Fatalf("List(unread) = %v, err %v", unread, err)
	}
	id := unread[0]
	if err := mb.MarkSeen(id); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	if err := mb.Archive(id); err != nil {
		t.Fatalf("Archive read message: %v", err)
	}

	archive, err := mb.InFolder("Archive")
	if err != nil {
		t.Fatalf("InFolder(Archive): %v", err)
	}
	scoped, ok := archive.(domain.ScopedMailbox)
	if !ok {
		t.Fatal("folder-scoped IMAP mailbox lost its scoped listing")
	}
	// COPY carries the flags across, so the filed message is read —
	// scope=all is the only listing that sees the whole folder.
	filed, err := scoped.List(domain.ScopeAll)
	if err != nil {
		t.Fatalf("list Archive: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("Archive holds %v, want the filed message", filed)
	}
}

// A stale id must not report a soft move that never happened: IMAP
// answers OK to COPY of a UID it does not hold, so the backend checks
// the message is there before claiming it moved.
func TestIMAPCurateUnknownUIDIsBadID(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	for name, op := range map[string]func(string) error{"Archive": mb.Archive, "Delete": mb.Delete} {
		err := op("99999")
		if err == nil {
			t.Errorf("%s of an unknown uid succeeded, want an error", name)
			continue
		}
		if !errors.Is(err, domain.ErrBadID) {
			t.Errorf("%s error = %v, want ErrBadID", name, err)
		}
	}
}
