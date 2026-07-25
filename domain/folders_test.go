package domain_test

import (
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

func TestCheckFolderName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		ok    bool
	}{
		{"a plain name", "Work", true},
		{"a hierarchy path is the backend's business, not this check's", "INBOX.Work", true},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"a newline, which is a name written to be interpreted elsewhere", "Work\r\nLOGOUT", false},
		{"a NUL", "Work\x00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CheckFolderName(tc.input)
			if tc.ok {
				if err != nil {
					t.Fatalf("CheckFolderName(%q) = %v, want nil", tc.input, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrBadFolder) {
				t.Fatalf("CheckFolderName(%q) = %v, want ErrBadFolder", tc.input, err)
			}
		})
	}
}

// The count is the point of the refusal: it is the size of what would
// have been destroyed, and the caller needs it to decide what to do next.
func TestCheckFolderEmptyStatesTheCount(t *testing.T) {
	if err := domain.CheckFolderEmpty("Work", 0); err != nil {
		t.Fatalf("empty folder refused: %v", err)
	}

	err := domain.CheckFolderEmpty("Work", 3)
	if !errors.Is(err, domain.ErrFolderNotEmpty) {
		t.Fatalf("err = %v, want ErrFolderNotEmpty", err)
	}
	for _, want := range []string{`"Work"`, "3 messages", "archive or delete them first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err, want)
		}
	}

	// A count and its noun that disagree undermine the number.
	if one := domain.CheckFolderEmpty("Work", 1); !strings.Contains(one.Error(), "1 message ") {
		t.Errorf("refusal %q, want a singular message count", one)
	}
}

func TestCheckFolderChildless(t *testing.T) {
	if err := domain.CheckFolderChildless("Work", nil); err != nil {
		t.Fatalf("childless folder refused: %v", err)
	}
	err := domain.CheckFolderChildless("Work", []string{"Work/A", "Work/B"})
	if !errors.Is(err, domain.ErrFolderNotEmpty) {
		t.Fatalf("err = %v, want ErrFolderNotEmpty", err)
	}
	for _, want := range []string{"2 subfolders", "Work/A, Work/B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err, want)
		}
	}
}

// The inbox and both curation destinations are refused, and the refusal
// says what deleting them would break — a caller told only "no" would
// reasonably try again with a different spelling.
func TestCheckFolderDeletableProtectsInboxAndCuration(t *testing.T) {
	plan := domain.CurationPlan{
		Archive: domain.CurationDestination{Folder: "INBOX.Archive", Route: domain.RouteConvention},
		Trash:   domain.CurationDestination{Folder: "INBOX.Trash", Route: domain.RouteDeclared},
	}

	if err := domain.CheckFolderDeletable("INBOX.Work", "INBOX.Work", plan); err != nil {
		t.Fatalf("an ordinary folder was refused: %v", err)
	}

	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{"inbox", "INBOX", []string{`"INBOX"`, "mailbox itself"}},
		{"inbox, lowercase — IMAP treats it case-insensitively", "inbox", []string{"mailbox itself"}},
		{"archive destination", "INBOX.Archive", []string{
			"email.archive", "email.archive and email.delete", "convention", "archive_folder",
		}},
		{"trash destination", "INBOX.Trash", []string{
			"email.delete", "email.archive and email.delete", "declared", "trash_folder",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CheckFolderDeletable(tc.input, tc.input, plan)
			if !errors.Is(err, domain.ErrFolderProtected) {
				t.Fatalf("err = %v, want ErrFolderProtected", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err, want)
				}
			}
		})
	}
}

// A backend that exposes a folder under a different name than it stores
// it under must not let the protection be sidestepped by spelling it the
// other way — which is exactly the maildir backend's "Trash"/".trash".
func TestCheckFolderDeletableComparesTheResolvedName(t *testing.T) {
	plan := domain.CurationPlan{
		Archive: domain.CurationDestination{Folder: ".archive", Route: domain.RouteFixed},
		Trash:   domain.CurationDestination{Folder: ".trash", Route: domain.RouteFixed},
	}

	err := domain.CheckFolderDeletable("Trash", ".trash", plan)
	if !errors.Is(err, domain.ErrFolderProtected) {
		t.Fatalf("err = %v, want ErrFolderProtected", err)
	}
	if !strings.Contains(err.Error(), `"Trash"`) {
		t.Errorf("refusal %q, want it to quote the name the caller used", err)
	}
	// A fixed layout has nothing to repoint, so it must not tell the
	// caller to edit a config key that would change nothing.
	if strings.Contains(err.Error(), "trash_folder") {
		t.Errorf("refusal %q offers a config override the dir backend does not have", err)
	}
}
