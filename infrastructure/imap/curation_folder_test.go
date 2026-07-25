package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func box(name string, attrs ...imap.MailboxAttr) *imap.ListData {
	return &imap.ListData{Mailbox: name, Attrs: attrs, Delim: '.'}
}

var (
	archiveTarget = curationTarget{attr: imap.MailboxAttrArchive, leaf: "Archive"}
	trashTarget   = curationTarget{attr: imap.MailboxAttrTrash, leaf: "Trash"}
)

func TestChooseCurationFolder(t *testing.T) {
	// The layout that motivated this: a personal namespace rooted at
	// "INBOX.", a server that declares \Trash but stays silent about
	// \Archive, and an INBOX.Archive folder sitting there unadvertised.
	// Copying to a bare "Archive" fails on such a server, and creating
	// one strands mail outside the namespace the user's client reads.
	hetznerish := []*imap.ListData{
		box("INBOX"),
		box("INBOX.Archive"),
		box("INBOX.Drafts", imap.MailboxAttrDrafts),
		box("INBOX.Sent", imap.MailboxAttrSent),
		box("INBOX.Trash", imap.MailboxAttrTrash),
		box("INBOX.spambucket", imap.MailboxAttrJunk),
	}

	for _, tc := range []struct {
		name       string
		target     curationTarget
		boxes      []*imap.ListData
		prefix     string
		wantFolder string
		wantCreate bool
	}{
		{
			"declared special-use wins",
			trashTarget, hetznerish, "INBOX.",
			"INBOX.Trash", false,
		},
		{
			"undeclared archive falls back to the namespace path",
			archiveTarget, hetznerish, "INBOX.",
			"INBOX.Archive", false,
		},
		{
			"flat server, declared",
			trashTarget,
			[]*imap.ListData{box("INBOX"), box("Trash", imap.MailboxAttrTrash)},
			"", "Trash", false,
		},
		{
			"flat server, undeclared but present",
			archiveTarget,
			[]*imap.ListData{box("INBOX"), box("Archive")},
			"", "Archive", false,
		},
		{
			// A prefixed server with neither declaration nor folder must
			// create inside the namespace, never at the root.
			"nothing to file into, prefixed namespace",
			archiveTarget,
			[]*imap.ListData{box("INBOX")},
			"INBOX.", "INBOX.Archive", true,
		},
		{
			"nothing to file into, flat namespace",
			trashTarget,
			[]*imap.ListData{box("INBOX")},
			"", "Trash", true,
		},
		{
			// A declaration outranks a same-named folder elsewhere.
			"special-use outranks a conventional path",
			trashTarget,
			[]*imap.ListData{box("INBOX.Trash"), box("Papierkorb", imap.MailboxAttrTrash)},
			"INBOX.", "Papierkorb", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder, create := chooseCurationFolder(tc.target, tc.boxes, tc.prefix)
			if folder != tc.wantFolder || create != tc.wantCreate {
				t.Errorf("chooseCurationFolder = (%q, create=%v), want (%q, create=%v)",
					folder, create, tc.wantFolder, tc.wantCreate)
			}
		})
	}
}

// An operator override answers before any server round trip, so a layout
// that defies discovery entirely still has an escape hatch.
func TestChooseCurationFolderOverrideShortCircuits(t *testing.T) {
	m := &Mailbox{cfg: Config{ArchiveFolder: "Archiv/2026", TrashFolder: "Müll"}}

	// resolveCurationFolder returns the override before touching the
	// client, which is why a nil client cannot panic here.
	for _, tc := range []struct {
		target curationTarget
		want   string
	}{
		{curationTarget{override: m.cfg.ArchiveFolder, attr: imap.MailboxAttrArchive, leaf: "Archive"}, "Archiv/2026"},
		{curationTarget{override: m.cfg.TrashFolder, attr: imap.MailboxAttrTrash, leaf: "Trash"}, "Müll"},
	} {
		got, err := m.resolveCurationFolder(nil, tc.target)
		if err != nil {
			t.Fatalf("override resolve: %v", err)
		}
		if got != tc.want {
			t.Errorf("override = %q, want %q", got, tc.want)
		}
	}
}
