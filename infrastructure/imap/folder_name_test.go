package imap

import (
	"errors"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"

	"go.klarlabs.de/briefkasten/domain"
)

// The layout this exists for — a personal namespace rooted under the
// inbox — is exactly what an in-memory test server does not reproduce,
// so the resolution is a pure function and tested as one, the same way
// chooseCurationFolder is.
func TestNamespaceFolder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		input  string
		want   string
	}{
		{"a flat server leaves the name alone", "", "Work", "Work"},
		{"an INBOX-rooted server places the folder inside the namespace", "INBOX.", "Work", "INBOX.Work"},
		{"a name that already carries the prefix is not prefixed twice", "INBOX.", "INBOX.Work", "INBOX.Work"},
		{"a nested path is placed whole", "INBOX.", "Work.2026", "INBOX.Work.2026"},
		{"INBOX is reserved and never a child of the prefix", "INBOX.", "INBOX", "INBOX"},
		{"INBOX is case-insensitive", "INBOX.", "inbox", "inbox"},
		{"a slash-delimited namespace works the same way", "Mail/", "Work", "Mail/Work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespaceFolder(tc.prefix, tc.input); got != tc.want {
				t.Errorf("namespaceFolder(%q, %q) = %q, want %q", tc.prefix, tc.input, got, tc.want)
			}
		})
	}
}

// A wildcard names a pattern, not a folder: an existence check that can
// match a mailbox the caller never named is one that can approve the
// wrong deletion.
func TestCheckFolderNameRefusesWildcards(t *testing.T) {
	for _, name := range []string{"Work*", "%", "Work%2026"} {
		if err := checkFolderName(name); !errors.Is(err, domain.ErrBadFolder) {
			t.Errorf("checkFolderName(%q) = %v, want ErrBadFolder", name, err)
		}
	}
	if err := checkFolderName("INBOX.Work"); err != nil {
		t.Errorf("checkFolderName(INBOX.Work) = %v, want nil", err)
	}
	if err := checkFolderName("Work\r\nLOGOUT"); !errors.Is(err, domain.ErrBadFolder) {
		t.Errorf("a name carrying CRLF was accepted: %v", err)
	}
}

// The delimiter comes from the server: matching on a guessed one would
// either miss a subtree or mistake "Workshop" for a child of "Work".
func TestChildrenOfAndFindFolder(t *testing.T) {
	// box() builds a listing entry with '.' as the delimiter.
	boxes := []*imap.ListData{
		box("INBOX"), box("Work"), box("Work.2026"), box("Work.2026.Q1"), box("Workshop"),
	}

	delim, held := findFolder(boxes, "Work")
	if !held || delim != '.' {
		t.Fatalf("findFolder(Work) = %q, %v; want the delimiter and true", delim, held)
	}
	if _, held := findFolder(boxes, "Ghost"); held {
		t.Error("findFolder(Ghost) reported a folder the server did not list")
	}

	got := childrenOf(boxes, "Work", delim)
	want := []string{"Work.2026", "Work.2026.Q1"}
	if !slices.Equal(got, want) {
		t.Errorf("childrenOf = %v, want %v (Workshop is not a child of Work)", got, want)
	}

	// A server that reports no delimiter has a flat namespace: nothing
	// can be a child, and guessing one would refuse deletions wrongly.
	if got := childrenOf(boxes, "Work", 0); got != nil {
		t.Errorf("childrenOf without a delimiter = %v, want none", got)
	}
}
