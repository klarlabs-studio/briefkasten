// Package imap is the IMAP backend (go-imap v2).
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
// Ids are message UIDs in the configured mailbox. One authenticated
// connection is kept between calls and handed to one caller at a time;
// it is probed before reuse and thrown away at the first sign of
// trouble, so the mailbox still survives server restarts and idle
// timeouts — it simply stops paying for a handshake and a LOGIN per
// operation.
//
// ListUnread issues UID SEARCH UNSEEN, List widens that to SEEN or ALL,
// Fetch reads BODY.PEEK[] (the \Seen flag is NOT set by fetching), and
// MarkSeen stores +FLAGS \Seen — seen messages simply stop being listed
// unread; nothing is ever deleted and no read flag is ever cleared.
type Mailbox struct {
	cfg Config

	// mu guards the cached connection, never a command in flight: the
	// Mailbox is shared across MCP requests, and callers that find the
	// cache empty must dial their own connection rather than queue
	// behind whoever holds it.
	mu     sync.Mutex
	idle   *imapclient.Client
	idleAt time.Time
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

const (
	// idleExpiry caps how long a cached connection may sit unused before
	// it is re-dialled rather than probed. Servers, load balancers and
	// NAT boxes all drop idle IMAP sessions without saying so; past a few
	// minutes the probe is likelier to cost a round trip than to save a
	// handshake. Well inside the ~30 minutes RFC 2177 assumes.
	idleExpiry = 3 * time.Minute

	// connectTimeout bounds the handshake and the liveness probe, whose
	// replies are single lines — anything slower is a server that is not
	// answering, not a slow one.
	connectTimeout = 30 * time.Second

	// cmdTimeout bounds a data command, which may stream a whole message
	// body; it matches the five minutes go-imap allows per literal read,
	// so it fails only what the library would have failed anyway.
	cmdTimeout = 5 * time.Minute
)

// await bounds a command whose reply carries no value.
func await(ctx context.Context, c *imapclient.Client, what string, wait func() error, limit time.Duration) error {
	_, err := awaitValue(ctx, c, what, func() (struct{}, error) { return struct{}{}, wait() }, limit)
	return err
}

// awaitValue waits for a command's reply until the caller's context ends
// or limit passes, whichever comes first.
//
// go-imap v2.0.0-beta.8 offers no hook for command deadlines:
// imapclient.Options carries a Dialer and nothing else, and the client
// arms its own 30s read deadline only once a reply has begun arriving —
// between sending a command and the first byte back it reads with no
// deadline at all. A server that accepts the socket, greets, and then
// goes quiet on LOGIN would hang the caller forever. Closing the
// connection is the only way to cancel a command in flight, which is
// fine here: an overrun connection is one we would discard anyway.
//
// That is also why the context is honoured here rather than merely
// passed down: the whole call stack below this point is a blocking read
// with no cancellation of its own, so this select is the only place a
// deadline or a hung-up caller can be turned into an actual abort. The
// waiting goroutine outlives the return by however long it takes the
// closed socket to surface as a read error; done is buffered so it can
// finish and exit rather than block forever on a receiver that left.
func awaitValue[T any](
	ctx context.Context, c *imapclient.Client, what string, wait func() (T, error), limit time.Duration,
) (T, error) {
	type reply struct {
		val T
		err error
	}
	done := make(chan reply, 1)
	go func() {
		val, err := wait()
		done <- reply{val, err}
	}()

	timer := time.NewTimer(limit)
	defer timer.Stop()
	var zero T
	select {
	case r := <-done:
		return r.val, r.err
	case <-ctx.Done():
		_ = c.Close() // unblocks the waiting goroutine
		// Wrapping ctx.Err() is what lets a caller tell "we stopped
		// waiting" from "the server said no": errors.Is reaches
		// context.Canceled or context.DeadlineExceeded through this.
		return zero, fmt.Errorf("imap: %s: %w", what, ctx.Err())
	case <-timer.C:
		_ = c.Close()
		return zero, fmt.Errorf("imap: %s: no reply within %s", what, limit)
	}
}

// withConn runs one operation on a pooled connection. The connection
// goes back to the cache when fn succeeds and is closed when it does
// not: a command that failed may have left the session mid-stream, or
// the server may have gone away entirely, and neither is worth guessing
// about on the next caller's behalf.
//
// Every operation in this file can take a pooled connection because each
// one issues its own commands, reads its own replies, and leaves the
// session exactly as it found it — authenticated and SELECTed on the
// configured mailbox, with no cross-call state to leak. IDLE is the one
// thing that would not fit, and it lives on the Watcher's own dedicated
// connection.
//
// The context bounds the whole operation, acquisition included: a probe
// or a handshake is as capable of hanging as the command it precedes.
func (m *Mailbox) withConn(ctx context.Context, fn func(*imapclient.Client) error) error {
	// A caller that has already given up gets nothing done on its behalf
	// — and, just as importantly, does not take the pooled connection
	// away from a caller that has not: acquiring it would probe it with a
	// dead context and throw it away.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("imap: %s: %w", m.cfg.Addr, err)
	}
	c, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		closeClient(ctx, c)
		return err
	}
	m.cache(ctx, c)
	return nil
}

// acquire hands out a connection ready for use: the cached one when it
// is still alive, a freshly dialled one otherwise.
func (m *Mailbox) acquire(ctx context.Context) (*imapclient.Client, error) {
	if c := m.takeIdle(ctx); c != nil {
		return c, nil
	}
	return m.dial(ctx)
}

// takeIdle claims the cached connection and returns it only if it still
// works. Claiming it under the lock is what keeps concurrent callers
// apart: a connection is either in the cache or owned by exactly one
// caller, never both.
//
// The NOOP matters because a dropped session looks perfectly healthy
// from this side until a command fails on it — and failing a caller's
// operation to discover that is precisely the failure reuse is meant to
// remove. It doubles as the poll that makes the server flush whatever
// it queued while the session sat idle, so a reused connection sees
// mail that arrived meanwhile.
func (m *Mailbox) takeIdle(ctx context.Context) *imapclient.Client {
	m.mu.Lock()
	c, since := m.idle, m.idleAt
	m.idle, m.idleAt = nil, time.Time{}
	m.mu.Unlock()

	if c == nil {
		return nil
	}
	if time.Since(since) > idleExpiry {
		closeClient(ctx, c)
		return nil
	}
	// A probe that the caller's context cuts short leaves a connection of
	// unknown health, which is exactly the kind this must not hand on.
	// Closing it and returning nil is right either way: the caller is
	// about to fail on its own context anyway, and the cache is left
	// without a socket nobody proved is alive.
	if err := await(ctx, c, "noop", c.Noop().Wait, connectTimeout); err != nil {
		closeClient(ctx, c)
		return nil
	}
	return c
}

// cache offers a healthy connection back for the next operation. One is
// enough — a second concurrent caller dialled its own and can hang up
// rather than leave the mailbox holding sockets it will not use.
func (m *Mailbox) cache(ctx context.Context, c *imapclient.Client) {
	m.mu.Lock()
	if m.idle != nil {
		m.mu.Unlock()
		closeClient(ctx, c)
		return
	}
	m.idle, m.idleAt = c, time.Now()
	m.mu.Unlock()
}

// dialerFor bounds the TCP connect — and, over TLS, the handshake — by
// the caller's deadline. It is the one hang above this file that closing
// a connection cannot cure, because there is no connection yet: a
// black-holed address would otherwise sit in connect() for the library's
// default 30 seconds no matter how little time the caller allowed.
//
// Only the deadline carries; go-imap dials through a net.Dialer rather
// than DialContext, so a plain cancellation cannot reach the connect.
// Everything after it — greeting, LOGIN, SELECT — goes through
// awaitValue, which does honour cancellation.
func dialerFor(ctx context.Context) *net.Dialer {
	d := &net.Dialer{Timeout: connectTimeout}
	if deadline, ok := ctx.Deadline(); ok {
		d.Deadline = deadline
	}
	return d
}

// dial connects, logs in, and selects the configured mailbox.
func (m *Mailbox) dial(ctx context.Context) (*imapclient.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", m.cfg.Addr, err)
	}
	var (
		c    *imapclient.Client
		err  error
		opts = &imapclient.Options{TLSConfig: m.cfg.TLSConfig, Dialer: dialerFor(ctx)}
	)
	if m.cfg.Insecure {
		c, err = imapclient.DialInsecure(m.cfg.Addr, opts)
	} else {
		c, err = imapclient.DialTLS(m.cfg.Addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", m.cfg.Addr, err)
	}
	if m.cfg.OAuth2 != nil {
		host, port := auth.SplitHostPort(m.cfg.Addr, 993)
		saslAuth, err := m.cfg.OAuth2.SASLClient(ctx, m.cfg.Username, host, port)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		authenticate := func() error { return c.Authenticate(saslAuth) }
		if err := await(ctx, c, "authenticate", authenticate, connectTimeout); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imap: authenticate: %w", err)
		}
	} else if err := await(ctx, c, "login", c.Login(m.cfg.Username, m.cfg.Password).Wait, connectTimeout); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: login: %w", err)
	}
	if _, err := awaitValue(ctx, c, "select "+m.cfg.Mailbox, c.Select(m.cfg.Mailbox, nil).Wait, connectTimeout); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: select %s: %w", m.cfg.Mailbox, err)
	}
	return c, nil
}

func closeClient(ctx context.Context, c *imapclient.Client) {
	// LOGOUT is a courtesy to the server, not something the caller waits
	// on: bound it so a server that has stopped answering cannot pin a
	// caller on the way out. A context that is already done shortens the
	// courtesy to nothing, which is the right trade — the caller has left.
	_ = await(ctx, c, "logout", c.Logout().Wait, connectTimeout)
	_ = c.Close()
}

// ListUnread returns the UIDs of unseen messages.
func (m *Mailbox) ListUnread(ctx context.Context) ([]string, error) {
	return m.List(ctx, domain.ScopeUnread)
}

// List returns the UIDs covered by scope: UID SEARCH UNSEEN, SEEN, or
// ALL. Listing never touches flags, so a read message stays read.
func (m *Mailbox) List(ctx context.Context, scope domain.Scope) ([]string, error) {
	criteria, err := scopeCriteria(scope)
	if err != nil {
		return nil, err
	}
	return m.uidSearch(ctx, criteria, "search "+string(scope))
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
func (m *Mailbox) uidSearch(ctx context.Context, criteria *imap.SearchCriteria, what string) ([]string, error) {
	var ids []string
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		data, err := awaitValue(ctx, c, what, c.UIDSearch(criteria, nil).Wait, cmdTimeout)
		if err != nil {
			return fmt.Errorf("imap: %s: %w", what, err)
		}

		uids := data.AllUIDs()
		ids = make([]string, len(uids))
		for i, uid := range uids {
			ids[i] = strconv.FormatUint(uint64(uid), 10)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// Fetch returns the raw RFC 5322 bytes of the message with the given UID.
// It peeks — fetching does not mark the message seen.
func (m *Mailbox) Fetch(ctx context.Context, id string) ([]byte, error) {
	uid, err := parseUID(id)
	if err != nil {
		return nil, err
	}

	var raw []byte
	err = m.withConn(ctx, func(c *imapclient.Client) error {
		section := &imap.FetchItemBodySection{Peek: true}
		msgs, err := awaitValue(ctx, c, "fetch "+id, c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			UID:         true,
			BodySection: []*imap.FetchItemBodySection{section},
		}).Collect, cmdTimeout)
		if err != nil {
			return fmt.Errorf("imap: fetch %s: %w", id, err)
		}
		if len(msgs) == 0 {
			return fmt.Errorf("%w: %s", domain.ErrBadID, id)
		}
		if raw = msgs[0].FindBodySection(section); raw == nil {
			return fmt.Errorf("imap: fetch %s: no body section in response", id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// Search returns unseen UIDs matching the query (UID SEARCH UNSEEN TEXT).
func (m *Mailbox) Search(ctx context.Context, query string) ([]string, error) {
	return m.SearchScope(ctx, domain.ScopeUnread, query)
}

// SearchScope returns the scope's UIDs matching the query (UID SEARCH
// <scope> TEXT).
func (m *Mailbox) SearchScope(ctx context.Context, scope domain.Scope, query string) ([]string, error) {
	criteria, err := scopeCriteria(scope)
	if err != nil {
		return nil, err
	}
	criteria.Text = []string{query}
	return m.uidSearch(ctx, criteria, "search")
}

var (
	_ domain.Searcher       = (*Mailbox)(nil)
	_ domain.ScopedSearcher = (*Mailbox)(nil)
)

// Folders lists the server's mailboxes (LIST "" "*").
func (m *Mailbox) Folders(ctx context.Context) ([]string, error) {
	var out []string
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		boxes, err := awaitValue(ctx, c, "list folders", c.List("", "*", nil).Collect, cmdTimeout)
		if err != nil {
			return fmt.Errorf("imap: list folders: %w", err)
		}
		out = make([]string, 0, len(boxes))
		for _, b := range boxes {
			out = append(out, b.Mailbox)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InFolder returns an Mailbox scoped to the named mailbox. It gets its
// own connection: a connection is SELECTed on one mailbox, and folder
// listings are rare next to inbox traffic, so sharing one would cost a
// RESELECT per call to save a handshake that is not on the hot path.
// Resolution is local — no command is sent — so the context is checked
// rather than plumbed anywhere.
func (m *Mailbox) InFolder(ctx context.Context, name string) (domain.Mailbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("imap: folder %q: %w", name, err)
	}
	if name == "" {
		return nil, errors.New("imap: folder name required")
	}
	cfg := m.cfg
	cfg.Mailbox = name
	return &Mailbox{cfg: cfg}, nil
}

var _ domain.FolderMailbox = (*Mailbox)(nil)

// MarkSeen sets the \Seen flag on the message with the given UID. It is
// idempotent: a message that is already seen succeeds unchanged, because
// +FLAGS adds a flag it already carries.
func (m *Mailbox) MarkSeen(ctx context.Context, id string) error {
	uid, err := parseUID(id)
	if err != nil {
		return err
	}

	return m.withConn(ctx, func(c *imapclient.Client) error {
		// Like COPY in fileTo, a STORE against a UID the mailbox does not
		// hold is a no-op most servers answer OK to. Agents are told to
		// mark seen only once processing succeeded, so an OK here retires
		// a message from every future unread listing — claiming that for a
		// message never touched destroys the retry safety net. A stale or
		// wrong id must fail loudly instead.
		found, err := awaitValue(ctx, c, "locate "+id,
			c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true}).Collect, cmdTimeout)
		if err != nil {
			return fmt.Errorf("imap: locate %s: %w", id, err)
		}
		if len(found) == 0 {
			return fmt.Errorf("%w: %s", domain.ErrBadID, id)
		}

		if err := await(ctx, c, "mark seen "+id, c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagSeen},
		}, nil).Close, cmdTimeout); err != nil {
			return fmt.Errorf("imap: mark seen %s: %w", id, err)
		}
		return nil
	})
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
func (m *Mailbox) resolveCurationFolder(
	ctx context.Context, c *imapclient.Client, t curationTarget,
) (string, error) {
	dest, err := m.planCurationFolder(ctx, c, t)
	if err != nil {
		return "", err
	}
	if dest.Route != domain.RouteCreate {
		return dest.Folder, nil
	}
	// Nothing to file into yet. Create it where the namespace says it
	// belongs, so it lands beside the user's other folders rather than
	// somewhere their mail client will never look.
	if err := await(ctx, c, "create "+dest.Folder, c.Create(dest.Folder, nil).Wait, cmdTimeout); err != nil {
		return "", fmt.Errorf(
			"imap: no %s folder found (no mailbox declares %s, and neither %q nor any known alias exists) and creating it failed: %w",
			t.leaf, t.attr, dest.Folder, err)
	}
	return dest.Folder, nil
}

// planCurationFolder resolves a destination without creating anything,
// so the same decision can be reported to a human in advance.
func (m *Mailbox) planCurationFolder(
	ctx context.Context, c *imapclient.Client, t curationTarget,
) (domain.CurationDestination, error) {
	if t.override != "" {
		return domain.CurationDestination{Folder: t.override, Route: domain.RouteOverride}, nil
	}
	boxes, err := awaitValue(ctx, c, "list folders",
		c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect, cmdTimeout)
	if err != nil {
		return domain.CurationDestination{}, fmt.Errorf("imap: cannot resolve the %s folder: %w", t.leaf, err)
	}
	folder, route := chooseCurationFolder(t, boxes, m.personalPrefix(ctx, c))
	return domain.CurationDestination{Folder: folder, Route: route}, nil
}

// CurationPlan reports where Archive and Delete would file, and how each
// destination was decided, without moving or creating anything.
func (m *Mailbox) CurationPlan(ctx context.Context) (domain.CurationPlan, error) {
	var plan domain.CurationPlan
	err := m.withConn(ctx, func(c *imapclient.Client) error {
		archive, err := m.planCurationFolder(ctx, c, m.archiveTarget())
		if err != nil {
			return err
		}
		trash, err := m.planCurationFolder(ctx, c, m.trashTarget())
		if err != nil {
			return err
		}
		plan = domain.CurationPlan{Archive: archive, Trash: trash}
		return nil
	})
	if err != nil {
		return domain.CurationPlan{}, err
	}
	return plan, nil
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
func (m *Mailbox) personalPrefix(ctx context.Context, c *imapclient.Client) string {
	ns, err := awaitValue(ctx, c, "namespace", c.Namespace().Wait, connectTimeout)
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
func (m *Mailbox) fileTo(ctx context.Context, t curationTarget, id string) error {
	uid, err := parseUID(id)
	if err != nil {
		return err
	}
	return m.withConn(ctx, func(c *imapclient.Client) error {
		folder, err := m.resolveCurationFolder(ctx, c, t)
		if err != nil {
			return err
		}

		// COPY of a UID that is not in the mailbox is a no-op most servers
		// answer OK to, which would report a soft move that never happened.
		// Curation reaches read mail, where an id can come from a listing
		// taken long before the call, so a stale id must fail loudly.
		found, err := awaitValue(ctx, c, "locate "+id,
			c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true}).Collect, cmdTimeout)
		if err != nil {
			return fmt.Errorf("imap: locate %s: %w", id, err)
		}
		if len(found) == 0 {
			return fmt.Errorf("%w: %s", domain.ErrBadID, id)
		}

		// The destination is resolved, not guessed, so a copy failure here is
		// a real fault worth surfacing rather than something to paper over by
		// creating a folder the server did not ask for.
		if _, err := awaitValue(ctx, c, "copy "+id, c.Copy(imap.UIDSetNum(uid), folder).Wait, cmdTimeout); err != nil {
			return fmt.Errorf("imap: copy %s to %s: %w", id, folder, err)
		}
		if err := await(ctx, c, "mark seen "+id, c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
			Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen},
		}, nil).Close, cmdTimeout); err != nil {
			return fmt.Errorf("imap: mark seen %s: %w", id, err)
		}
		return nil
	})
}

// Archive files the message into the server's archive folder; the
// original is marked seen, never expunged. The destination is whatever
// the server calls its archive — many advertise \Trash but not
// \Archive, so this commonly resolves through the namespace path.
func (m *Mailbox) Archive(ctx context.Context, id string) error {
	return m.fileTo(ctx, m.archiveTarget(), id)
}

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
func (m *Mailbox) Delete(ctx context.Context, id string) error {
	return m.fileTo(ctx, m.trashTarget(), id)
}

var _ domain.Curator = (*Mailbox)(nil)

func parseUID(id string) (imap.UID, error) {
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%w: %s", domain.ErrBadID, id)
	}
	return imap.UID(n), nil
}

var _ domain.Mailbox = (*Mailbox)(nil)
