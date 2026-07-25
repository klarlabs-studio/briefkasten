// Package imap is the IMAP backend (go-imap v2).
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/auth"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Config configures an Mailbox.
type Config struct {
	// Addr is the IMAP server address (host:port). Required.
	Addr string
	// Username and Password authenticate via LOGIN.
	Username string
	Password string
	// Mailbox is the mailbox to read. Defaults to "INBOX".
	Mailbox string
	// ArchiveFolder and TrashFolder pin where curation files messages.
	// Empty means discover: the server's SPECIAL-USE declaration first,
	// then the personal namespace's conventional path. Set these only
	// when a server's layout defies both.
	ArchiveFolder string
	TrashFolder   string
	// Insecure dials without TLS. For tests and local servers only.
	Insecure bool
	// TLSConfig optionally overrides the TLS client configuration.
	TLSConfig *tls.Config
	// OAuth2 switches authentication from LOGIN to XOAUTH2/OAUTHBEARER.
	OAuth2 *auth.OAuth2Settings
}

// Mailbox is a Mailbox backed by an IMAP server (go-imap v2).
//
// Ids are message UIDs in the configured mailbox. Each call dials a fresh
// connection and logs out afterwards — no connection state is kept, so the
// mailbox survives server restarts and idle timeouts.
//
// ListUnread issues UID SEARCH UNSEEN, List widens that to SEEN or ALL,
// Fetch reads BODY.PEEK[] (the \Seen flag is NOT set by fetching), and
// MarkSeen stores +FLAGS \Seen — seen messages simply stop being listed
// unread; nothing is ever deleted and no read flag is ever cleared.
type Mailbox struct {
	cfg Config
}

// New validates cfg and returns an Mailbox.
func New(cfg Config) (*Mailbox, error) {
	if cfg.Addr == "" {
		return nil, errors.New("imap: Addr is required")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	return &Mailbox{cfg: cfg}, nil
}

// dial connects, logs in, and selects the configured mailbox.
func (m *Mailbox) dial() (*imapclient.Client, error) {
	var (
		c   *imapclient.Client
		err error
	)
	if m.cfg.Insecure {
		c, err = imapclient.DialInsecure(m.cfg.Addr, nil)
	} else {
		c, err = imapclient.DialTLS(m.cfg.Addr, &imapclient.Options{TLSConfig: m.cfg.TLSConfig})
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", m.cfg.Addr, err)
	}
	if m.cfg.OAuth2 != nil {
		host, port := auth.SplitHostPort(m.cfg.Addr, 993)
		saslAuth, err := m.cfg.OAuth2.SASLClient(context.Background(), m.cfg.Username, host, port)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		if err := c.Authenticate(saslAuth); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imap: authenticate: %w", err)
		}
	} else if err := c.Login(m.cfg.Username, m.cfg.Password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: login: %w", err)
	}
	if _, err := c.Select(m.cfg.Mailbox, nil).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: select %s: %w", m.cfg.Mailbox, err)
	}
	return c, nil
}

func closeClient(c *imapclient.Client) {
	_ = c.Logout().Wait()
	_ = c.Close()
}

// ListUnread returns the UIDs of unseen messages.
func (m *Mailbox) ListUnread() ([]string, error) { return m.List(domain.ScopeUnread) }

// List returns the UIDs covered by scope: UID SEARCH UNSEEN, SEEN, or
// ALL. Listing never touches flags, so a read message stays read.
func (m *Mailbox) List(scope domain.Scope) ([]string, error) {
	criteria, err := scopeCriteria(scope)
	if err != nil {
		return nil, err
	}
	return m.uidSearch(criteria, "search "+string(scope))
}

var _ domain.ScopedMailbox = (*Mailbox)(nil)

// scopeCriteria translates a scope into IMAP search criteria. ScopeAll
// searches with no flag restriction, which is IMAP's ALL.
func scopeCriteria(scope domain.Scope) (*imap.SearchCriteria, error) {
	switch scope {
	case domain.ScopeUnread:
		return &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil
	case domain.ScopeRead:
		return &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, nil
	case domain.ScopeAll:
		return &imap.SearchCriteria{}, nil
	default:
		return nil, fmt.Errorf("%w: %q", domain.ErrBadScope, string(scope))
	}
}

// uidSearch runs a UID SEARCH and renders the UIDs as ids.
func (m *Mailbox) uidSearch(criteria *imap.SearchCriteria, what string) ([]string, error) {
	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer closeClient(c)

	data, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: %s: %w", what, err)
	}

	uids := data.AllUIDs()
	ids := make([]string, len(uids))
	for i, uid := range uids {
		ids[i] = strconv.FormatUint(uint64(uid), 10)
	}
	return ids, nil
}

// Fetch returns the raw RFC 5322 bytes of the message with the given UID.
// It peeks — fetching does not mark the message seen.
func (m *Mailbox) Fetch(id string) ([]byte, error) {
	uid, err := parseUID(id)
	if err != nil {
		return nil, err
	}

	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer closeClient(c)

	section := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch %s: %w", id, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}
	raw := msgs[0].FindBodySection(section)
	if raw == nil {
		return nil, fmt.Errorf("imap: fetch %s: no body section in response", id)
	}
	return raw, nil
}

// Search returns unseen UIDs matching the query (UID SEARCH UNSEEN TEXT).
func (m *Mailbox) Search(query string) ([]string, error) {
	return m.SearchScope(domain.ScopeUnread, query)
}

// SearchScope returns the scope's UIDs matching the query (UID SEARCH
// <scope> TEXT).
func (m *Mailbox) SearchScope(scope domain.Scope, query string) ([]string, error) {
	criteria, err := scopeCriteria(scope)
	if err != nil {
		return nil, err
	}
	criteria.Text = []string{query}
	return m.uidSearch(criteria, "search")
}

var (
	_ domain.Searcher       = (*Mailbox)(nil)
	_ domain.ScopedSearcher = (*Mailbox)(nil)
)

// Folders lists the server's mailboxes (LIST "" "*").
func (m *Mailbox) Folders() ([]string, error) {
	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer closeClient(c)

	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: list folders: %w", err)
	}
	out := make([]string, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, b.Mailbox)
	}
	return out, nil
}

// InFolder returns an Mailbox scoped to the named mailbox.
func (m *Mailbox) InFolder(name string) (domain.Mailbox, error) {
	if name == "" {
		return nil, errors.New("imap: folder name required")
	}
	cfg := m.cfg
	cfg.Mailbox = name
	return &Mailbox{cfg: cfg}, nil
}

var _ domain.FolderMailbox = (*Mailbox)(nil)

// MarkSeen sets the \Seen flag on the message with the given UID.
func (m *Mailbox) MarkSeen(id string) error {
	uid, err := parseUID(id)
	if err != nil {
		return err
	}

	c, err := m.dial()
	if err != nil {
		return err
	}
	defer closeClient(c)

	if err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}, nil).Close(); err != nil {
		return fmt.Errorf("imap: mark seen %s: %w", id, err)
	}
	return nil
}

// curationTarget names one destination for a soft move, in the three
// ways a server might describe it: what the operator pinned, what the
// server declares via RFC 6154 SPECIAL-USE, and the conventional leaf
// name to place inside the personal namespace.
type curationTarget struct {
	override string
	attr     imap.MailboxAttr
	leaf     string
	// aliases are folder names other clients leave behind — localized
	// or legacy. They are consulted last, and only to avoid creating a
	// duplicate next to a folder that already does the job.
	aliases []string
}

// Folder names mail clients create for the same purpose in different
// languages and eras. Order is priority: an earlier entry wins, so the
// choice does not depend on the order a server happens to LIST in.
//
// These rank below the server's own declaration and below the
// conventional path on purpose. A mailbox touched by several clients
// over the years can hold three plausible trash folders at once, and a
// name table cannot tell which one the human still opens — only the
// server (SPECIAL-USE) or the operator can settle that. The list earns
// its place only in the case where the alternative is creating a fresh
// folder beside the one already in use.
var (
	trashAliases = []string{
		"Deleted Items",    // Outlook / Exchange
		"Deleted Messages", // Apple Mail
		"Papierkorb",       // de
		"Corbeille",        // fr
		"Papelera",         // es
		"Cestino",          // it
		"Lixeira",          // pt
		"Prullenbak",       // nl
		"Kosz",             // pl
		"Skräp",            // sv
		"Slettede elementer",
	}
	archiveAliases = []string{
		"Archives", // plural is common on Dovecot/Thunderbird
		"Archiv",   // de
		"Archivio", // it
		"Archivo",  // es
		"Arquivo",  // pt
		"Arkiv",    // sv/da/no
		"Archiwum", // pl
	}
)

// resolveCurationFolder finds where a soft move should land, in order of
// authority: the operator's override, the server's own SPECIAL-USE
// declaration, then the personal namespace's conventional path.
//
// The namespace step is not a fallback for exotic servers — it is the
// common case. A mailbox rooted at "INBOX." holds its Trash at
// "INBOX.Trash", and plenty of such servers advertise \Trash while
// staying silent about \Archive. Copying to a bare "Archive" there
// fails, and creating one puts a stray folder outside the namespace the
// user's mail client reads.
func (m *Mailbox) resolveCurationFolder(c *imapclient.Client, t curationTarget) (string, error) {
	dest, err := m.planCurationFolder(c, t)
	if err != nil {
		return "", err
	}
	if dest.Route != domain.RouteCreate {
		return dest.Folder, nil
	}
	// Nothing to file into yet. Create it where the namespace says it
	// belongs, so it lands beside the user's other folders rather than
	// somewhere their mail client will never look.
	if err := c.Create(dest.Folder, nil).Wait(); err != nil {
		return "", fmt.Errorf(
			"imap: no %s folder found (no mailbox declares %s, and neither %q nor any known alias exists) and creating it failed: %w",
			t.leaf, t.attr, dest.Folder, err)
	}
	return dest.Folder, nil
}

// planCurationFolder resolves a destination without creating anything,
// so the same decision can be reported to a human in advance.
func (m *Mailbox) planCurationFolder(c *imapclient.Client, t curationTarget) (domain.CurationDestination, error) {
	if t.override != "" {
		return domain.CurationDestination{Folder: t.override, Route: domain.RouteOverride}, nil
	}
	boxes, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return domain.CurationDestination{}, fmt.Errorf("imap: cannot resolve the %s folder: %w", t.leaf, err)
	}
	folder, route := chooseCurationFolder(t, boxes, m.personalPrefix(c))
	return domain.CurationDestination{Folder: folder, Route: route}, nil
}

// CurationPlan reports where Archive and Delete would file, and how each
// destination was decided, without moving or creating anything.
func (m *Mailbox) CurationPlan() (domain.CurationPlan, error) {
	c, err := m.dial()
	if err != nil {
		return domain.CurationPlan{}, err
	}
	defer closeClient(c)

	archive, err := m.planCurationFolder(c, m.archiveTarget())
	if err != nil {
		return domain.CurationPlan{}, err
	}
	trash, err := m.planCurationFolder(c, m.trashTarget())
	if err != nil {
		return domain.CurationPlan{}, err
	}
	return domain.CurationPlan{Archive: archive, Trash: trash}, nil
}

var _ domain.CurationInspector = (*Mailbox)(nil)

// chooseCurationFolder decides where a soft move lands from what the
// server listed and where its personal namespace is rooted. Pure, so
// every server shape is testable without one: the layout that matters
// most here — a namespace rooted at "INBOX." whose server declares
// \Trash but not \Archive — is precisely the one an in-memory test
// server will not reproduce.
//
// Order of authority: the server's own SPECIAL-USE declaration, then an
// existing folder at the namespace's conventional path, then a known
// localized or legacy name, and only then a path to create.
func chooseCurationFolder(t curationTarget, boxes []*imap.ListData, prefix string) (string, domain.CurationRoute) {
	existing := make(map[string]string, len(boxes)) // lowercased leaf -> full name
	for _, b := range boxes {
		if slices.Contains(b.Attrs, t.attr) {
			return b.Mailbox, domain.RouteDeclared
		}
		existing[strings.ToLower(leafOf(b.Mailbox, prefix))] = b.Mailbox
	}

	if full, ok := existing[strings.ToLower(t.leaf)]; ok {
		return full, domain.RouteConvention
	}
	// Nothing canonical. Before creating a folder, check whether another
	// client already made one for this purpose under a different name —
	// filing beside it would split the user's mail across two folders.
	for _, alias := range t.aliases {
		if full, ok := existing[strings.ToLower(alias)]; ok {
			return full, domain.RouteAlias
		}
	}
	return prefix + t.leaf, domain.RouteCreate
}

// leafOf strips the personal namespace prefix so "INBOX.Papierkorb" and
// a flat "Papierkorb" compare equal.
func leafOf(mailbox, prefix string) string {
	if prefix != "" {
		return strings.TrimPrefix(mailbox, prefix)
	}
	return mailbox
}

// personalPrefix reports where the user's own folders are rooted (e.g.
// "INBOX." on servers that nest everything under the inbox), or "" when
// the server does not answer or roots them at the top.
func (m *Mailbox) personalPrefix(c *imapclient.Client) string {
	ns, err := c.Namespace().Wait()
	if err != nil || ns == nil || len(ns.Personal) == 0 {
		return ""
	}
	return ns.Personal[0].Prefix
}

// fileTo copies a message into the resolved folder and marks the
// original seen. Deliberately not MOVE: MOVE expunges the source, and
// briefkasten never expunges — the original survives, seen. The UID is
// all that identifies the message, so already-read mail curates exactly
// like unread mail.
func (m *Mailbox) fileTo(t curationTarget, id string) error {
	uid, err := parseUID(id)
	if err != nil {
		return err
	}
	c, err := m.dial()
	if err != nil {
		return err
	}
	defer closeClient(c)

	folder, err := m.resolveCurationFolder(c, t)
	if err != nil {
		return err
	}

	// COPY of a UID that is not in the mailbox is a no-op most servers
	// answer OK to, which would report a soft move that never happened.
	// Curation reaches read mail, where an id can come from a listing
	// taken long before the call, so a stale id must fail loudly.
	found, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		return fmt.Errorf("imap: locate %s: %w", id, err)
	}
	if len(found) == 0 {
		return fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}

	// The destination is resolved, not guessed, so a copy failure here is
	// a real fault worth surfacing rather than something to paper over by
	// creating a folder the server did not ask for.
	if _, err := c.Copy(imap.UIDSetNum(uid), folder).Wait(); err != nil {
		return fmt.Errorf("imap: copy %s to %s: %w", id, folder, err)
	}
	if err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen},
	}, nil).Close(); err != nil {
		return fmt.Errorf("imap: mark seen %s: %w", id, err)
	}
	return nil
}

// Archive files the message into the server's archive folder; the
// original is marked seen, never expunged. The destination is whatever
// the server calls its archive — many advertise \Trash but not
// \Archive, so this commonly resolves through the namespace path.
func (m *Mailbox) Archive(id string) error { return m.fileTo(m.archiveTarget(), id) }

func (m *Mailbox) archiveTarget() curationTarget {
	return curationTarget{
		override: m.cfg.ArchiveFolder,
		attr:     imap.MailboxAttrArchive,
		leaf:     "Archive",
		aliases:  archiveAliases,
	}
}

func (m *Mailbox) trashTarget() curationTarget {
	return curationTarget{
		override: m.cfg.TrashFolder,
		attr:     imap.MailboxAttrTrash,
		leaf:     "Trash",
		aliases:  trashAliases,
	}
}

// Delete files the message into the server's trash folder — a soft
// delete; real removal stays with the mail provider's retention,
// briefkasten never expunges.
func (m *Mailbox) Delete(id string) error { return m.fileTo(m.trashTarget(), id) }

var _ domain.Curator = (*Mailbox)(nil)

func parseUID(id string) (imap.UID, error) {
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}
	return imap.UID(n), nil
}

var _ domain.Mailbox = (*Mailbox)(nil)
