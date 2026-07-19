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
