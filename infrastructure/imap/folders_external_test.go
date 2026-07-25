package imap_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"

	bimap "go.klarlabs.de/briefkasten/infrastructure/imap"
)

// folderNames lists what the server holds, which is the only honest
// check that a create or a delete landed: the tool's own answer is not
// evidence of anything.
func folderNames(t *testing.T, mb *bimap.Mailbox) []string {
	t.Helper()
	names, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	return names
}

func TestIMAPCreateFolderIsIdempotent(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if names := folderNames(t, mb); !slices.Contains(names, "Work") {
		t.Fatalf("folders = %v, want Work", names)
	}

	// Creating it again is the state the caller asked for, not an error
	// — the same idempotence MarkSeen has.
	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("second CreateFolder: %v", err)
	}
	if err := mb.CreateFolder(t.Context(), "INBOX"); err != nil {
		t.Fatalf("CreateFolder(INBOX): %v", err)
	}
	if n := len(folderNames(t, mb)); n != 2 {
		t.Errorf("folders = %v, want the inbox and Work only", folderNames(t, mb))
	}
}

func TestIMAPCreateFolderRejectsPatterns(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	for _, name := range []string{"Work*", "", "Work\r\nLOGOUT"} {
		if err := mb.CreateFolder(t.Context(), name); !errors.Is(err, domain.ErrBadFolder) {
			t.Errorf("CreateFolder(%q) = %v, want ErrBadFolder", name, err)
		}
	}
}

func TestIMAPDeleteFolderRemovesAnEmptyOne(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t, "Work"))

	if err := mb.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if names := folderNames(t, mb); slices.Contains(names, "Work") {
		t.Errorf("folders = %v, want Work gone", names)
	}
}

// The refusal that matters: a folder holding mail is never emptied on
// the caller's behalf, and the count is stated so they can decide what
// to do with the messages.
func TestIMAPDeleteFolderRefusesAFolderHoldingMail(t *testing.T) {
	addr := startIMAPServer(t, "Work")
	// Fill Work through the curation path, which is the only writer this
	// backend has: pinning the archive there files a real message into it.
	filler, err := bimap.New(bimap.Config{
		Addr: addr, Username: "alice", Password: "secret", Insecure: true, ArchiveFolder: "Work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Archive(t.Context(), firstUID(t, filler)); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	mb := newTestIMAPMailbox(t, addr)
	err = mb.DeleteFolder(t.Context(), "Work")
	if !errors.Is(err, domain.ErrFolderNotEmpty) {
		t.Fatalf("DeleteFolder = %v, want ErrFolderNotEmpty", err)
	}
	if !strings.Contains(err.Error(), "1 message") {
		t.Errorf("refusal %q does not state the count", err)
	}
	if !strings.Contains(err.Error(), "archive or delete them first") {
		t.Errorf("refusal %q does not say what to do instead", err)
	}

	// The folder and its message survived the refusal.
	if names := folderNames(t, mb); !slices.Contains(names, "Work") {
		t.Errorf("folders = %v, want Work still there", names)
	}
	if n := filedIn(t, addr, "Work"); n != 1 {
		t.Errorf("Work holds %d messages, want the mail untouched", n)
	}
}

func TestIMAPDeleteFolderRefusesTheInbox(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	err := mb.DeleteFolder(t.Context(), "INBOX")
	if !errors.Is(err, domain.ErrFolderProtected) {
		t.Fatalf("DeleteFolder(INBOX) = %v, want ErrFolderProtected", err)
	}
	if n := len(folderNames(t, mb)); n != 1 {
		t.Errorf("folders = %v, want the inbox intact", folderNames(t, mb))
	}
}

// Removing the folder curation files into would break archive and
// delete, so it is refused and the error says which folder plays which
// role — resolved by the same path curation resolves it, never by a
// second list of names that could disagree.
func TestIMAPDeleteFolderRefusesTheCurationDestinations(t *testing.T) {
	addr := startIMAPServer(t, "Archive", "Trash")
	mb := newTestIMAPMailbox(t, addr)

	for name, tool := range map[string]string{"Archive": "email.archive", "Trash": "email.delete"} {
		err := mb.DeleteFolder(t.Context(), name)
		if !errors.Is(err, domain.ErrFolderProtected) {
			t.Fatalf("DeleteFolder(%q) = %v, want ErrFolderProtected", name, err)
		}
		if !strings.Contains(err.Error(), tool) {
			t.Errorf("refusal %q does not name %s", err, tool)
		}
		if !strings.Contains(err.Error(), "email.archive and email.delete") {
			t.Errorf("refusal %q does not say what it would break", err)
		}
	}

	// A configured override moves the protection with it: the folder the
	// operator pinned is the one that must survive.
	pinned, err := bimap.New(bimap.Config{
		Addr: addr, Username: "alice", Password: "secret", Insecure: true, TrashFolder: "Archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.DeleteFolder(t.Context(), "Archive"); !errors.Is(err, domain.ErrFolderProtected) {
		t.Errorf("DeleteFolder of the pinned trash = %v, want ErrFolderProtected", err)
	}
}

func TestIMAPDeleteFolderRefusesAFolderWithChildren(t *testing.T) {
	// The in-memory server's hierarchy delimiter is "/".
	mb := newTestIMAPMailbox(t, startIMAPServer(t, "Work", "Work/2026"))

	err := mb.DeleteFolder(t.Context(), "Work")
	if !errors.Is(err, domain.ErrFolderNotEmpty) {
		t.Fatalf("DeleteFolder = %v, want ErrFolderNotEmpty", err)
	}
	if !strings.Contains(err.Error(), "Work/2026") {
		t.Errorf("refusal %q does not name the subfolder", err)
	}
	// The leaf deletes fine, which is the path the caller is told to take.
	if err := mb.DeleteFolder(t.Context(), "Work/2026"); err != nil {
		t.Fatalf("DeleteFolder of the leaf: %v", err)
	}
	if err := mb.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("DeleteFolder of the emptied parent: %v", err)
	}
}

func TestIMAPDeleteFolderRejectsUnknownNames(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	for _, name := range []string{"Ghost", "Work*", ""} {
		if err := mb.DeleteFolder(t.Context(), name); !errors.Is(err, domain.ErrBadFolder) {
			t.Errorf("DeleteFolder(%q) = %v, want ErrBadFolder", name, err)
		}
	}
}

// A refusal is the caller's request being wrong, not the session's
// health: the pooled connection has to survive it, or every refused
// delete would cost the next caller a handshake.
func TestIMAPRefusedDeleteKeepsTheConnection(t *testing.T) {
	addr, ln := startCountedIMAPServer(t, "Work", "Work/2026")
	mb := newTestIMAPMailbox(t, addr)

	if err := mb.DeleteFolder(t.Context(), "Work"); !errors.Is(err, domain.ErrFolderNotEmpty) {
		t.Fatalf("DeleteFolder = %v, want ErrFolderNotEmpty", err)
	}
	dials := ln.accepted()
	if err := mb.CreateFolder(t.Context(), "Later"); err != nil {
		t.Fatalf("CreateFolder after a refusal: %v", err)
	}
	if got := ln.accepted(); got != dials {
		t.Errorf("dials = %d, want the connection reused (%d)", got, dials)
	}
}
