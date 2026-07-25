package imap_test

import (
	"testing"

	"go.klarlabs.de/briefkasten/domain"

	bimap "go.klarlabs.de/briefkasten/infrastructure/imap"
)

// filedIn reports how many messages sit in the named mailbox.
func filedIn(t *testing.T, addr, folder string) int {
	t.Helper()
	mb, err := bimap.New(bimap.Config{
		Addr: addr, Username: "alice", Password: "secret", Insecure: true, Mailbox: folder,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := mb.List(domain.ScopeAll)
	if err != nil {
		t.Fatalf("list %s: %v", folder, err)
	}
	return len(ids)
}

// firstUID returns the one seeded message's id.
func firstUID(t *testing.T, mb *bimap.Mailbox) string {
	t.Helper()
	ids, err := mb.ListUnread()
	if err != nil || len(ids) == 0 {
		t.Fatalf("ListUnread = %v, err %v", ids, err)
	}
	return ids[0]
}

// An existing folder must be reused, not shadowed by a newly created one
// — filing into a second folder the user never sees is the failure this
// whole resolution path exists to prevent.
func TestIMAPCurationReusesExistingFolder(t *testing.T) {
	addr := startIMAPServer(t, "Archive")
	mb := newTestIMAPMailbox(t, addr)

	if err := mb.Archive(firstUID(t, mb)); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if n := filedIn(t, addr, "Archive"); n != 1 {
		t.Errorf("Archive holds %d messages, want 1", n)
	}
}

// With nothing to file into, the folder is created and used.
func TestIMAPCurationCreatesMissingFolder(t *testing.T) {
	addr := startIMAPServer(t)
	mb := newTestIMAPMailbox(t, addr)

	if err := mb.Delete(firstUID(t, mb)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := filedIn(t, addr, "Trash"); n != 1 {
		t.Errorf("Trash holds %d messages, want 1", n)
	}
}

// The operator override wins over everything the server says, which is
// the escape hatch for layouts that defy discovery.
func TestIMAPCurationHonoursConfiguredFolders(t *testing.T) {
	addr := startIMAPServer(t, "Papierkorb", "Trash")
	mb, err := bimap.New(bimap.Config{
		Addr: addr, Username: "alice", Password: "secret", Insecure: true,
		TrashFolder: "Papierkorb",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := mb.Delete(firstUID(t, mb)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := filedIn(t, addr, "Papierkorb"); n != 1 {
		t.Errorf("configured Papierkorb holds %d messages, want 1", n)
	}
	if n := filedIn(t, addr, "Trash"); n != 0 {
		t.Errorf("default Trash holds %d messages, want 0 — the override was ignored", n)
	}
}
