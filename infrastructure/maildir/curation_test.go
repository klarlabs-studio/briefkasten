package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

func TestDirMailboxSearch(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Invoice for May\r\n\r\nhi")
	drop(t, root, "b.eml", "From: a@b.c\r\nSubject: Lunch\r\n\r\nyour INVOICE is attached")
	drop(t, root, "c.eml", "From: a@b.c\r\nSubject: Standup\r\n\r\nnotes")

	hits, err := mb.Search(t.Context(), "invoice")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search hits = %v, want 2 matches", hits)
	}
	if hits[0] != "a.eml" || hits[1] != "b.eml" {
		t.Errorf("Search hits = %v, want [a.eml b.eml]", hits)
	}
}

func TestDirMailboxSearchNoMatch(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: A\r\n\r\nhi")

	hits, err := mb.Search(t.Context(), "nothing-here")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Search hits = %v, want none", hits)
	}
}

func TestDirMailboxFoldersFresh(t *testing.T) {
	mb, _ := newDir(t)

	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 1 || folders[0] != "INBOX" {
		t.Errorf("Folders = %v, want [INBOX]", folders)
	}
}

func TestDirMailboxFoldersListsSubMaildirs(t *testing.T) {
	mb, root := newDir(t)

	sub, err := mb.InFolder(t.Context(), "work")
	if err != nil {
		t.Fatalf("InFolder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "work", "new", "w.eml"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := sub.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread in folder: %v", err)
	}
	if len(ids) != 1 || ids[0] != "w.eml" {
		t.Errorf("folder unread = %v, want [w.eml]", ids)
	}

	// Archiving creates the .archive maildir, which is listed under its
	// exposed name — archived mail a client cannot see is mail it has
	// lost.
	drop(t, root, "a.eml", "hi")
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 3 || folders[0] != "INBOX" || folders[1] != "Archive" || folders[2] != "work" {
		t.Errorf("Folders = %v, want [INBOX Archive work]", folders)
	}
}

// Unrelated dot-directories are not mail: sync state and editor
// droppings must not turn up in a folder listing.
func TestDirMailboxFoldersHidesForeignDotDirs(t *testing.T) {
	mb, root := newDir(t)
	if err := os.MkdirAll(filepath.Join(root, ".notmuch", "new"), 0o700); err != nil {
		t.Fatal(err)
	}

	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 1 || folders[0] != "INBOX" {
		t.Errorf("Folders = %v, want [INBOX]", folders)
	}
}

// Archived mail must stay reachable: listable, fetchable, and movable
// back out — otherwise archiving is a one-way trip on this backend while
// the same message stays reachable over IMAP.
func TestDirMailboxArchivedMailIsReachable(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha body")
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 || folders[1] != "Archive" {
		t.Fatalf("Folders = %v, want the archive listed", folders)
	}

	archive, err := mb.InFolder(t.Context(), "Archive")
	if err != nil {
		t.Fatalf("InFolder(Archive): %v", err)
	}
	ids, err := archive.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread in archive: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Fatalf("archive contents = %v, want [a.eml]", ids)
	}
	raw, err := archive.Fetch(t.Context(), "a.eml")
	if err != nil {
		t.Fatalf("Fetch from archive: %v", err)
	}
	if !strings.Contains(string(raw), "alpha body") {
		t.Errorf("archived message = %q, want the body", raw)
	}
}

// Trashed mail is a soft delete, so it is reachable on the same terms.
func TestDirMailboxTrashedMailIsReachable(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "hi")
	if err := mb.Delete(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	trash, err := mb.InFolder(t.Context(), "Trash")
	if err != nil {
		t.Fatalf("InFolder(Trash): %v", err)
	}
	ids, err := trash.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread in trash: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Fatalf("trash contents = %v, want [a.eml]", ids)
	}
}

// CurationPlan answers with the on-disk names, so those must open the
// same maildirs — a plan a caller cannot act on is decoration.
func TestDirMailboxInFolderAcceptsCurationPlanNames(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "hi")
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	plan, err := mb.CurationPlan(t.Context())
	if err != nil {
		t.Fatalf("CurationPlan: %v", err)
	}

	archive, err := mb.InFolder(t.Context(), plan.Archive.Folder)
	if err != nil {
		t.Fatalf("InFolder(%q): %v", plan.Archive.Folder, err)
	}
	ids, err := archive.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("archive via plan name = %v, want [a.eml]", ids)
	}
}

// The curated names are reserved: a plain directory sitting under one
// must not shadow the archive or list beside it.
func TestDirMailboxCuratedNamesAreReserved(t *testing.T) {
	mb, root := newDir(t)
	if err := os.MkdirAll(filepath.Join(root, "Archive", "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	drop(t, root, "a.eml", "hi")
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 || folders[0] != "INBOX" || folders[1] != "Archive" {
		t.Errorf("Folders = %v, want [INBOX Archive] listed once", folders)
	}
	archive, err := mb.InFolder(t.Context(), "Archive")
	if err != nil {
		t.Fatalf("InFolder(Archive): %v", err)
	}
	ids, err := archive.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("Archive resolved to the plain directory: %v", ids)
	}
}

// Reaching curated mail must not have opened a way out of the root.
func TestDirMailboxCuratedFolderRejectsTraversal(t *testing.T) {
	mb, root := newDir(t)
	for _, name := range []string{
		".archive/../..", "../.archive", "Archive/../../etc", ".archive/../.ssh", "./.archive",
	} {
		if _, err := mb.InFolder(t.Context(), name); err == nil {
			t.Errorf("InFolder(%q) accepted, want error", name)
		}
	}

	drop(t, root, "a.eml", "hi")
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	archive, err := mb.InFolder(t.Context(), "Archive")
	if err != nil {
		t.Fatalf("InFolder(Archive): %v", err)
	}
	if _, err := archive.Fetch(t.Context(), "../../new/a.eml"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Fetch traversal in archive = %v, want ErrBadID", err)
	}
}

func TestDirMailboxInFolderInbox(t *testing.T) {
	mb, _ := newDir(t)

	got, err := mb.InFolder(t.Context(), "INBOX")
	if err != nil {
		t.Fatalf("InFolder(INBOX): %v", err)
	}
	if got != domain.Mailbox(mb) {
		t.Error("InFolder(INBOX) did not return the root mailbox")
	}
}

func TestDirMailboxInFolderCreatesMaildir(t *testing.T) {
	mb, root := newDir(t)

	if _, err := mb.InFolder(t.Context(), "work"); err != nil {
		t.Fatalf("InFolder: %v", err)
	}
	for _, sub := range []string{"new", "cur"} {
		st, err := os.Stat(filepath.Join(root, "work", sub))
		if err != nil || !st.IsDir() {
			t.Errorf("work/%s not created: %v", sub, err)
		}
	}
}

func TestDirMailboxInFolderRejectsBadNames(t *testing.T) {
	mb, _ := newDir(t)
	for _, name := range []string{"", "../escape", "a/b", ".hidden"} {
		if _, err := mb.InFolder(t.Context(), name); err == nil {
			t.Errorf("InFolder(%q) accepted, want error", name)
		}
	}
}

func TestDirMailboxArchive(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "hi")

	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".archive", "new", "a.eml")); err != nil {
		t.Errorf("archived message missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("message still in new/: %v", err)
	}
	ids, err := mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("unread after archive = %v, want none", ids)
	}
}

func TestDirMailboxDelete(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "a.eml", "hi")

	if err := mb.Delete(t.Context(), "a.eml"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "new", "a.eml")); err != nil {
		t.Errorf("trashed message missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("message still in new/: %v", err)
	}
}

func TestDirMailboxArchiveUnknown(t *testing.T) {
	mb, _ := newDir(t)
	if err := mb.Archive(t.Context(), "nope.eml"); err == nil {
		t.Error("Archive of unknown id accepted")
	}
}

func TestDirMailboxArchiveRejectsTraversal(t *testing.T) {
	mb, _ := newDir(t)
	err := mb.Archive(t.Context(), "../x")
	if err == nil {
		t.Fatal("path traversal accepted in Archive")
	}
	if !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Archive error = %v, want ErrBadID", err)
	}
}
