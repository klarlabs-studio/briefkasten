package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

func newFolderMailbox(t *testing.T) (*Mailbox, string) {
	t.Helper()
	root := t.TempDir()
	mb, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return mb, root
}

// A created folder is a whole maildir — new/, cur/ and tmp/ — so a
// foreign delivery agent can write into it, and it shows up in the
// folder listing the MCP resource serves.
func TestCreateFolderMakesAMaildir(t *testing.T) {
	mb, root := newFolderMailbox(t)

	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	for _, sub := range []string{"new", "cur", "tmp"} {
		info, err := os.Stat(filepath.Join(root, "Work", sub))
		if err != nil || !info.IsDir() {
			t.Errorf("Work/%s: %v", sub, err)
		}
	}

	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if !slices.Contains(folders, "Work") {
		t.Errorf("folders = %v, want it to list Work", folders)
	}

	// Idempotent, like MarkSeen: the caller asked for a folder to exist.
	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("second CreateFolder: %v", err)
	}
	if err := mb.CreateFolder(t.Context(), "INBOX"); err != nil {
		t.Fatalf("CreateFolder(INBOX): %v", err)
	}
}

// A folder name is the only part of the call the caller controls, so it
// gets the same confinement a message id gets.
func TestCreateFolderRejectsNamesThatEscapeTheRoot(t *testing.T) {
	mb, root := newFolderMailbox(t)

	for _, name := range []string{"../escape", "a/b", ".hidden", "", "   ", "a/../../b"} {
		err := mb.CreateFolder(t.Context(), name)
		if !errors.Is(err, domain.ErrBadFolder) {
			t.Errorf("CreateFolder(%q) = %v, want ErrBadFolder", name, err)
		}
	}
	// Nothing was written outside the mailbox on the way to those errors.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder was created outside the root: %v", err)
	}
}

// The curated names are briefkasten's; a folder created under one of
// them would collide with the maildir curation already files into.
func TestCreateFolderRefusesTheCuratedNames(t *testing.T) {
	mb, _ := newFolderMailbox(t)

	for _, name := range []string{"Archive", "Trash"} {
		err := mb.CreateFolder(t.Context(), name)
		if !errors.Is(err, domain.ErrFolderProtected) {
			t.Errorf("CreateFolder(%q) = %v, want ErrFolderProtected", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "email.archive and email.delete") {
			t.Errorf("refusal %q does not say what it would break", err)
		}
	}
	// The on-disk spellings are refused too, by the same rule.
	for _, name := range []string{".archive", ".trash"} {
		if err := mb.CreateFolder(t.Context(), name); !errors.Is(err, domain.ErrFolderProtected) {
			t.Errorf("CreateFolder(%q) = %v, want ErrFolderProtected", name, err)
		}
	}
}

func TestDeleteFolderRemovesAnEmptyOne(t *testing.T) {
	mb, root := newFolderMailbox(t)
	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatal(err)
	}

	if err := mb.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Work")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Work still on disk: %v", err)
	}
	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(folders, "Work") {
		t.Errorf("folders = %v, want Work gone", folders)
	}
}

// The whole maildir counts as content, not just the unread backlog: a
// message in cur/ was read, not disposed of.
func TestDeleteFolderRefusesAFolderHoldingMail(t *testing.T) {
	for _, sub := range []string{"new", "cur", "tmp"} {
		t.Run(sub, func(t *testing.T) {
			mb, root := newFolderMailbox(t)
			if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.eml", "b.eml"} {
				if err := os.WriteFile(filepath.Join(root, "Work", sub, name), []byte("From: a\r\n\r\nx"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := mb.DeleteFolder(t.Context(), "Work")
			if !errors.Is(err, domain.ErrFolderNotEmpty) {
				t.Fatalf("DeleteFolder = %v, want ErrFolderNotEmpty", err)
			}
			if !strings.Contains(err.Error(), "2 messages") {
				t.Errorf("refusal %q does not state the count", err)
			}
			// The mail is still there: a refusal that deleted half the
			// folder would be the invariant not holding.
			if _, err := os.Stat(filepath.Join(root, "Work", sub, "a.eml")); err != nil {
				t.Errorf("message gone after a refused delete: %v", err)
			}
		})
	}
}

// A folder whose only content is a child folder is not empty either —
// the child holds mail, or may.
func TestDeleteFolderRefusesAFolderWithSubfolders(t *testing.T) {
	mb, root := newFolderMailbox(t)
	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Work", "Sub", "new"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := mb.DeleteFolder(t.Context(), "Work")
	if !errors.Is(err, domain.ErrFolderNotEmpty) {
		t.Fatalf("DeleteFolder = %v, want ErrFolderNotEmpty", err)
	}
	if !strings.Contains(err.Error(), "1 subfolder") {
		t.Errorf("refusal %q does not name the subfolder", err)
	}
}

func TestDeleteFolderRefusesTheInboxAndTheCurationDestinations(t *testing.T) {
	mb, _ := newFolderMailbox(t)

	for _, name := range []string{"INBOX", "Archive", "Trash", ".archive", ".trash"} {
		err := mb.DeleteFolder(t.Context(), name)
		if !errors.Is(err, domain.ErrFolderProtected) {
			t.Errorf("DeleteFolder(%q) = %v, want ErrFolderProtected", name, err)
		}
	}

	// Deleting the archive would break both curation operations, and the
	// refusal has to say so — the caller is choosing what to do next.
	err := mb.DeleteFolder(t.Context(), "Archive")
	if err == nil || !strings.Contains(err.Error(), "email.archive and email.delete") {
		t.Errorf("refusal %q does not say what it would break", err)
	}
}

func TestDeleteFolderRejectsUnknownAndEscapingNames(t *testing.T) {
	mb, _ := newFolderMailbox(t)

	for _, name := range []string{"../escape", "a/b", ".hidden", "", "Ghost"} {
		if err := mb.DeleteFolder(t.Context(), name); !errors.Is(err, domain.ErrBadFolder) {
			t.Errorf("DeleteFolder(%q) = %v, want ErrBadFolder", name, err)
		}
	}
}

// Curated mail stays reachable through the exposed folder names, so a
// created folder must not shadow them and a deleted one must not take
// them with it.
func TestFolderManagementLeavesCurationReachable(t *testing.T) {
	mb, root := newFolderMailbox(t)
	if err := os.WriteFile(filepath.Join(root, "new", "a.eml"), []byte("From: a\r\n\r\nx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mb.Archive(t.Context(), "a.eml"); err != nil {
		t.Fatal(err)
	}
	if err := mb.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatal(err)
	}
	if err := mb.DeleteFolder(t.Context(), "Work"); err != nil {
		t.Fatal(err)
	}

	box, err := mb.InFolder(t.Context(), "Archive")
	if err != nil {
		t.Fatalf("InFolder(Archive): %v", err)
	}
	ids, err := box.ListUnread(t.Context())
	if err != nil || len(ids) != 1 {
		t.Fatalf("archived mail = %v err %v, want one message", ids, err)
	}
}

var _ domain.FolderManager = (*Mailbox)(nil)
