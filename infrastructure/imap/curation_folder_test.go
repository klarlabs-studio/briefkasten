package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"

	"go.klarlabs.de/briefkasten/domain"
)

func box(name string, attrs ...imap.MailboxAttr) *imap.ListData {
	return &imap.ListData{Mailbox: name, Attrs: attrs, Delim: '.'}
}

var (
	archiveTarget = curationTarget{attr: imap.MailboxAttrArchive, leaf: "Archive", aliases: archiveAliases}
	trashTarget   = curationTarget{attr: imap.MailboxAttrTrash, leaf: "Trash", aliases: trashAliases}
)

func TestChooseCurationFolder(t *testing.T) {
	// The layout that motivated this: a personal namespace rooted at
	// "INBOX.", a server that declares \Trash but stays silent about
	// \Archive, and an INBOX.Archive folder sitting there unadvertised.
	// Note the three trash-like folders — the residue of several mail
	// clients over the years, and the reason a name table must never
	// outrank the server's own declaration.
	multiClient := []*imap.ListData{
		box("INBOX"),
		box("INBOX.Archive"),
		box("INBOX.Drafts", imap.MailboxAttrDrafts),
		box("INBOX.Sent", imap.MailboxAttrSent),
		box("INBOX.Trash", imap.MailboxAttrTrash),
		box("INBOX.Deleted Messages"),
		box("INBOX.Papierkorb"),
		box("INBOX.spambucket", imap.MailboxAttrJunk),
	}

	for _, tc := range []struct {
		name       string
		target     curationTarget
		boxes      []*imap.ListData
		prefix     string
		wantFolder string
		wantRoute  domain.CurationRoute
	}{
		{
			"declared special-use wins over every alias present",
			trashTarget, multiClient, "INBOX.",
			"INBOX.Trash", domain.RouteDeclared,
		},
		{
			"undeclared archive falls back to the namespace path",
			archiveTarget, multiClient, "INBOX.",
			"INBOX.Archive", domain.RouteConvention,
		},
		{
			"flat server, declared",
			trashTarget,
			[]*imap.ListData{box("INBOX"), box("Trash", imap.MailboxAttrTrash)},
			"", "Trash", domain.RouteDeclared,
		},
		{
			"flat server, undeclared but present",
			archiveTarget,
			[]*imap.ListData{box("INBOX"), box("Archive")},
			"", "Archive", domain.RouteConvention,
		},
		{
			// The point of the alias step: a German mailbox with no
			// declaration and no "Trash" must file into the Papierkorb
			// the human actually opens, not a fresh Trash beside it.
			"localized name used instead of creating a duplicate",
			trashTarget,
			[]*imap.ListData{box("INBOX"), box("INBOX.Papierkorb")},
			"INBOX.", "INBOX.Papierkorb", domain.RouteAlias,
		},
		{
			"legacy Outlook name, flat server",
			trashTarget,
			[]*imap.ListData{box("INBOX"), box("Deleted Items")},
			"", "Deleted Items", domain.RouteAlias,
		},
		{
			// Alias order decides, not the server's LIST order, so the
			// choice is stable across servers.
			"earlier alias wins when several exist",
			trashTarget,
			[]*imap.ListData{box("INBOX"), box("Papierkorb"), box("Deleted Items")},
			"", "Deleted Items", domain.RouteAlias,
		},
		{
			"alias matching ignores case",
			archiveTarget,
			[]*imap.ListData{box("INBOX"), box("INBOX.archiv")},
			"INBOX.", "INBOX.archiv", domain.RouteAlias,
		},
		{
			// A conventional folder outranks any alias.
			"convention beats alias",
			trashTarget,
			[]*imap.ListData{box("INBOX"), box("INBOX.Trash"), box("INBOX.Papierkorb")},
			"INBOX.", "INBOX.Trash", domain.RouteConvention,
		},
		{
			"nothing to file into, prefixed namespace",
			archiveTarget,
			[]*imap.ListData{box("INBOX")},
			"INBOX.", "INBOX.Archive", domain.RouteCreate,
		},
		{
			"nothing to file into, flat namespace",
			trashTarget,
			[]*imap.ListData{box("INBOX")},
			"", "Trash", domain.RouteCreate,
		},
		{
			"special-use outranks a conventional path",
			trashTarget,
			[]*imap.ListData{box("INBOX.Trash"), box("Papierkorb", imap.MailboxAttrTrash)},
			"INBOX.", "Papierkorb", domain.RouteDeclared,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder, route := chooseCurationFolder(tc.target, tc.boxes, tc.prefix)
			if folder != tc.wantFolder || route != tc.wantRoute {
				t.Errorf("chooseCurationFolder = (%q, %s), want (%q, %s)",
					folder, route, tc.wantFolder, tc.wantRoute)
			}
		})
	}
}

// An operator override answers before any server round trip, so a layout
// that defies discovery entirely still has an escape hatch.
func TestCurationOverrideShortCircuits(t *testing.T) {
	m := &Mailbox{cfg: Config{ArchiveFolder: "Archiv/2026", TrashFolder: "Müll"}}

	// planCurationFolder returns the override before touching the client,
	// which is why a nil client cannot panic here.
	for _, tc := range []struct {
		target curationTarget
		want   string
	}{
		{m.archiveTarget(), "Archiv/2026"},
		{m.trashTarget(), "Müll"},
	} {
		got, err := m.planCurationFolder(t.Context(), nil, tc.target)
		if err != nil {
			t.Fatalf("override resolve: %v", err)
		}
		if got.Folder != tc.want || got.Route != domain.RouteOverride {
			t.Errorf("override = %+v, want %q via %s", got, tc.want, domain.RouteOverride)
		}
	}
}
