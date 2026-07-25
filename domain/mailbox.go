// Package domain holds briefkasten's bounded context: the ports and
// invariants of a mailbox served to agents and humans. It imports no
// infrastructure — backends, transports, and presentation all depend on
// this package, never the reverse.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Mailbox is the core port: anything that can list unread messages,
// fetch raw RFC 5322 bytes, and mark a message as seen.
//
// Every method takes a context and must honour it. The contract is the
// one the caller needs to bound a request: when ctx is cancelled or its
// deadline passes, the call abandons whatever it is waiting on and
// returns promptly with an error satisfying errors.Is(err,
// context.Canceled) or errors.Is(err, context.DeadlineExceeded) — never
// a backend error dressed up as a fault. That is what distinguishes "we
// stopped waiting" from "the backend is broken", and the resilience
// decorator, the retry policy, and the circuit breaker all read the
// difference.
//
// Abandoning a call says nothing about whether the backend completed it.
// MarkSeen in particular is idempotent precisely so a cancelled call can
// be repeated safely.
type Mailbox interface {
	// ListUnread returns the ids of messages not yet marked seen.
	ListUnread(ctx context.Context) ([]string, error)
	// Fetch returns the raw message bytes for an id, read or unread.
	Fetch(ctx context.Context, id string) ([]byte, error)
	// MarkSeen marks a message as processed so it is not listed again.
	// It is idempotent: acknowledging a message that is already read
	// succeeds and changes nothing.
	MarkSeen(ctx context.Context, id string) error
}

// Scope selects which slice of a mailbox a listing or search covers.
// Unread is the agent-facing default — the ingest backlog — while Read
// and All open the already-processed mail for reference and recall.
type Scope string

const (
	// ScopeUnread covers messages not yet marked seen (the default).
	ScopeUnread Scope = "unread"
	// ScopeRead covers messages already marked seen.
	ScopeRead Scope = "read"
	// ScopeAll covers the whole mailbox, read and unread alike.
	ScopeAll Scope = "all"
)

// ParseScope resolves a scope name; the empty string means ScopeUnread,
// so callers that never heard of scopes keep the old behaviour.
func ParseScope(name string) (Scope, error) {
	switch Scope(name) {
	case "", ScopeUnread:
		return ScopeUnread, nil
	case ScopeRead:
		return ScopeRead, nil
	case ScopeAll:
		return ScopeAll, nil
	default:
		return "", fmt.Errorf("%w: %q (want unread, read, or all)", ErrBadScope, name)
	}
}

// ScopedMailbox is an optional Mailbox capability: listing beyond the
// unread backlog. Backends that cannot distinguish read from unread mail
// simply do not implement it, and scoped requests fail loudly rather
// than silently returning the unread set.
//
// Like Mailbox, every method honours its context.
type ScopedMailbox interface {
	// List returns the ids covered by scope.
	List(ctx context.Context, scope Scope) ([]string, error)
}

// Searcher is an optional Mailbox capability: full-text search over the
// unread backlog. Like Mailbox, it honours its context.
type Searcher interface {
	// Search returns the unread ids whose raw content matches the query
	// (case-insensitive).
	Search(ctx context.Context, query string) ([]string, error)
}

// ScopedSearcher is an optional Searcher capability: search restricted
// to a scope rather than always to the unread backlog. Like Mailbox, it
// honours its context.
type ScopedSearcher interface {
	// SearchScope returns the ids within scope whose raw content matches
	// the query (case-insensitive).
	SearchScope(ctx context.Context, scope Scope, query string) ([]string, error)
}

// FolderMailbox is an optional Mailbox capability: backends with multiple
// folders list them and hand out folder-scoped instances. Like Mailbox,
// it honours its context.
type FolderMailbox interface {
	// Folders returns the available folder names; the default folder is
	// included (as "INBOX" for the dir backend).
	Folders(ctx context.Context) ([]string, error)
	// InFolder returns a Mailbox scoped to the named folder. The context
	// bounds only the resolution itself; the returned Mailbox takes a
	// fresh context per call and does not inherit this one.
	InFolder(ctx context.Context, name string) (Mailbox, error)
}

// FolderManager is an optional FolderMailbox capability: making and
// removing the folders FolderMailbox lists. A backend that can only
// enumerate what already exists simply does not implement it.
//
// The two operations are deliberately asymmetric, because their risks
// are. Creating a folder cannot lose anything, so it is idempotent and
// cheap. Deleting one could destroy every message in it — which is the
// single thing briefkasten never does — so a delete is refused rather
// than allowed to cascade: see DeleteFolder.
//
// Like Mailbox, both methods honour their context.
type FolderManager interface {
	// CreateFolder creates a folder, and succeeds if it is already
	// there.
	//
	// Idempotence is the same choice MarkSeen makes, for the same
	// reason: the caller asked for a state, not for an event. A folder
	// that already exists is that state, and failing the call would only
	// teach agents to swallow the error — after which a real failure
	// would be swallowed with it. It also makes a cancelled call safe to
	// repeat.
	//
	// The name is resolved by the backend, not taken literally: on a
	// server whose personal namespace is rooted at "INBOX.", asking for
	// "Work" creates "INBOX.Work", beside the user's other folders
	// rather than somewhere their mail client never looks. A name the
	// backend cannot use — one that escapes its root, or one it reserves
	// for curation — is refused with ErrBadFolder or ErrFolderProtected.
	CreateFolder(ctx context.Context, name string) error

	// DeleteFolder removes an empty folder.
	//
	// Only an empty one. Briefkasten's central invariant is that it
	// never destroys mail: email.delete is a soft move to trash, and the
	// IMAP backend copies rather than MOVEs precisely because MOVE
	// expunges. Deleting a folder full of messages would destroy all of
	// them in one command, so a folder holding mail is refused with
	// ErrFolderNotEmpty and the count — the caller moves or deletes the
	// messages first, each of which is itself a soft move, and then the
	// empty folder can go. There is deliberately no force flag: a flag
	// that turns the invariant off is the invariant not holding.
	//
	// Two folders are refused outright, with ErrFolderProtected: the
	// inbox, and whichever folders curation resolves to (see
	// CurationPlan). Removing the destination archive and delete file
	// into would break both.
	//
	// The emptiness check races, and cannot not race. It asks the
	// backend what the folder holds and then deletes it, and mail can
	// arrive in the window between the two — a delivery on IMAP, a file
	// dropped into a maildir. The window is made as small as the
	// backends allow (the count is taken immediately before the delete,
	// never cached from an earlier resolution step) and the maildir
	// backend closes it entirely, because removing a directory that has
	// gained a file fails at the kernel rather than deleting it. On IMAP
	// it stays open: a message that arrives in that window is deleted
	// with the folder, and no amount of checking first can change that.
	DeleteFolder(ctx context.Context, name string) error
}

// ErrBadFolder rejects a folder name the backend cannot use: empty, one
// that escapes the mailbox root, or one naming a folder that is not
// there.
var ErrBadFolder = errors.New("briefkasten: invalid folder")

// ErrFolderNotEmpty refuses to delete a folder that still holds
// messages or subfolders.
var ErrFolderNotEmpty = errors.New("briefkasten: folder is not empty")

// ErrFolderProtected refuses to create or delete a folder briefkasten
// depends on: the inbox itself, or a curation destination.
var ErrFolderProtected = errors.New("briefkasten: folder is protected")

// CheckFolderName rejects the names no backend can accept.
//
// It is the shared floor, not the whole check: what counts as a legal
// name below this is the backend's own business — a maildir folder is
// one directory inside the root, an IMAP folder may be a hierarchy path
// — and each adds its rules on top. Control characters are refused here
// because a name carrying CR or LF is a name written to be interpreted
// somewhere it should not be, on the wire or in a path, and no backend
// has a use for one.
func CheckFolderName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: a folder name is required", ErrBadFolder)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character", ErrBadFolder, name)
		}
	}
	return nil
}

// CheckFolderEmpty refuses to delete a folder that still holds mail.
//
// The refusal states the count and what to do instead, because both are
// what the caller needs: the count is the size of what would have been
// destroyed, and the remedy — move or delete the messages first — is
// itself a soft move, so following it destroys nothing either. A folder
// emptied that way can then be deleted, which is the whole path from
// "delete this folder" to the folder being gone without a single message
// ceasing to exist.
func CheckFolderEmpty(name string, messages int) error {
	if messages > 0 {
		return fmt.Errorf(
			"%w: %q holds %d %s — archive or delete them first (both are soft moves, so nothing is destroyed), then delete the folder",
			ErrFolderNotEmpty, name, messages, plural(messages, "message"))
	}
	return nil
}

// CheckFolderChildless refuses to delete a folder that has subfolders.
//
// A folder with children is not empty even when it holds no messages of
// its own: the children do, or may. Deleting a parent takes its subtree
// with it on backends that recurse, and leaves an unselectable stub on
// the ones that do not — so the caller deletes the leaves first, each of
// which is checked the same way.
func CheckFolderChildless(name string, children []string) error {
	if len(children) > 0 {
		return fmt.Errorf("%w: %q holds %d %s (%s) — delete them first, then delete the folder",
			ErrFolderNotEmpty, name, len(children), plural(len(children), "subfolder"),
			strings.Join(children, ", "))
	}
	return nil
}

// CheckFolderDeletable refuses the deletions that would break the
// mailbox rather than merely empty it: the inbox, and either curation
// destination.
//
// resolved is the backend's own name for the folder, name is what the
// caller asked for and what the error quotes. The two differ where a
// backend exposes a folder under a different name than it stores it
// under — the maildir backend answers to "Trash" for a directory it
// calls ".trash" — and comparing the resolved form is what stops the
// protection being sidestepped by spelling the folder the other way.
//
// The curation check reads the plan rather than a name list because the
// destinations are discovered, not fixed: on one server trash is
// "INBOX.Trash", on the next it is "Deleted Items". Whatever archive and
// delete would file into is what must survive, and asking the same
// question the curation path asks is the only way to be sure the answer
// matches.
func CheckFolderDeletable(name, resolved string, plan CurationPlan) error {
	if strings.EqualFold(resolved, "INBOX") {
		return fmt.Errorf("%w: %q is the mailbox itself and cannot be deleted", ErrFolderProtected, name)
	}
	for _, dest := range []struct {
		CurationDestination
		tool     string
		override string
	}{
		{plan.Archive, "email.archive", "archive_folder"},
		{plan.Trash, "email.delete", "trash_folder"},
	} {
		if dest.Folder == "" || dest.Folder != resolved {
			continue
		}
		remedy := fmt.Sprintf("point %s at another folder in the config first", dest.override)
		if dest.Route == RouteFixed {
			remedy = "this backend's curation layout is fixed, so there is nothing to point elsewhere"
		}
		return fmt.Errorf(
			"%w: %q is where %s files mail (route: %s) — deleting it would break email.archive and email.delete; %s",
			ErrFolderProtected, name, dest.tool, dest.Route, remedy)
	}
	return nil
}

// plural is the one-or-many suffix these refusals need; a count in an
// error message that reads "1 messages" undermines the number it is
// there to convey.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// Curator is an optional Mailbox capability: human curation of the
// mailbox. Both operations are soft moves — Archive files the message
// away, Delete moves it to trash. Nothing is ever expunged; data is
// never destroyed.
//
// Curation is not restricted to the unread backlog: an id is an id,
// whatever its read state. Tidying mail that was processed weeks ago is
// exactly what a human reaches for these operations to do, so a backend
// that resolves ids only within the unread set does not satisfy this
// port.
//
// Like Mailbox, every method honours its context. Because curation is a
// soft move, an abandoned call is safe to repeat: the worst outcome of
// giving up on one is not knowing yet whether the move landed.
type Curator interface {
	// Archive moves a message — read or unread — to the archive.
	Archive(ctx context.Context, id string) error
	// Delete moves a message — read or unread — to the trash.
	Delete(ctx context.Context, id string) error
}

// MaxBulkIDs caps how many messages one bulk call may act on.
//
// A batch is authorised by a single human gesture, so the cap is what
// bounds the blast radius of that one "yes": a confirmation covering a
// hundred messages is still something a person can weigh, one covering
// ten thousand is not. It also keeps a batch small enough that the
// per-id report stays readable and one IMAP command set stays within
// what servers accept.
const MaxBulkIDs = 100

// MaxFetchBytes caps the raw bytes one batched fetch may return in total.
//
// The id cap alone does not bound a fetch. Curation moves references —
// a hundred of them cost a hundred UIDs on the wire — but a fetch
// returns whole messages, and a hundred messages carrying attachments is
// comfortably hundreds of megabytes: enough to exhaust the MCP client's
// context or its memory before it can decide it did not want them.
//
// 25 MiB is deliberately the same ceiling domain.MaxMessageBytes puts on
// one outbound message, so the two directions agree on what briefkasten
// considers a reasonable amount of mail to move in one call. A batch
// measured over it is refused, never trimmed: see ErrFetchTooLarge.
const MaxFetchBytes = 25 << 20 // 25 MiB

// MaxSummaryMessages caps how many messages one summary embeds.
//
// The other two ceilings bound what a call moves; this one bounds what a
// call quotes. A summary embeds message bodies verbatim, so without a cap
// its size is the mailbox's size: on a 50,000-message backlog, a caller
// asking to summarize the lot builds the whole backlog in memory as one
// string before anything downstream gets the chance to decline it. And
// because the embedded bodies are untrusted, the widest possible embed is
// also the widest possible injection payload — the memory cost is only
// half of what the cap is for.
//
// 100 is deliberately the same number MaxBulkIDs allows one bulk call, so
// a caller can summarize exactly the set it could then curate in one
// call, and no more. The byte side needs
// no budget of its own. Every embedded message is already truncated
// individually (16 KiB in the MCP adapter), so 100 of them is at most
// ~1.6 MiB — two orders of magnitude under MaxFetchBytes, and small
// enough that a second knob would guard nothing the first does not. The
// message count was the only unbounded dimension, so it is the only one
// capped.
//
// Unlike MaxBulkIDs, asking for more than this is not an error. A summary
// is a read-only convenience, not a destructive batch: a caller over the
// cap gets the cap, and is told in the rendered text that it was clamped
// and that asking again for more will not help.
const MaxSummaryMessages = 100

// ErrBulkSize rejects a batch that is empty, over MaxBulkIDs, or repeats
// an id.
var ErrBulkSize = errors.New("briefkasten: invalid batch")

// ErrFetchTooLarge refuses a batch whose messages would exceed
// MaxFetchBytes in total.
var ErrFetchTooLarge = errors.New("briefkasten: batch too large to fetch")

// CheckBulkIDs validates a batch before any of it runs. It is deliberate
// that the whole call is refused rather than trimmed: a caller that
// exceeded the cap enumerated more mail than it was allowed to curate at
// once, and silently acting on the first hundred would be a different
// operation from the one it asked for.
//
// Duplicates are refused for the same reason the result is keyed by id —
// an id appearing twice would make its outcome ambiguous.
func CheckBulkIDs(ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("%w: no ids given (want 1 to %d)", ErrBulkSize, MaxBulkIDs)
	}
	if len(ids) > MaxBulkIDs {
		return fmt.Errorf("%w: %d ids exceeds the %d-id cap for one call — split the batch",
			ErrBulkSize, len(ids), MaxBulkIDs)
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: id %q appears more than once", ErrBulkSize, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// CheckFetchBudget refuses a batch measured over MaxFetchBytes.
//
// It is called with a total the backend measured before reading a single
// body — an IMAP RFC822.SIZE fetch, a maildir stat — because the only
// useful moment to refuse is before the bytes exist. Discovering the size
// by reading it has already spent the memory the budget exists to
// protect.
//
// The refusal is whole, and it is not a truncation. A truncated RFC 5322
// message is not a smaller message: it is corrupt data that a MIME parser
// will happily accept and misread, and handing that to a consumer is
// worse than handing it an error. So the answer is the budget, what the
// batch actually measures, and how many ids that covers — everything the
// caller needs to split the batch and try again.
func CheckFetchBudget(total int64, ids int) error {
	if total > MaxFetchBytes {
		return fmt.Errorf(
			"%w: %d ids measure %d bytes, over the %d-byte (%d MiB) budget for one call — fetch fewer ids per call",
			ErrFetchTooLarge, ids, total, MaxFetchBytes, MaxFetchBytes>>20)
	}
	return nil
}

// BulkFailure is one id a bulk operation could not act on, with the
// reason it failed. The error is kept whole rather than flattened to a
// string so callers can still test it — errors.Is(f.Err, ErrBadID)
// distinguishes an id the mailbox does not hold from a backend fault.
type BulkFailure struct {
	ID  string
	Err error
}

// MarshalJSON renders a failure as {"id":…,"error":…}: an error value has
// no useful JSON form of its own, and the message is what a human or an
// agent reads.
func (f BulkFailure) MarshalJSON() ([]byte, error) {
	msg := ""
	if f.Err != nil {
		msg = f.Err.Error()
	}
	return json.Marshal(struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}{ID: f.ID, Error: msg})
}

// BulkResult reports a bulk operation id by id.
//
// There is no transaction behind a batch: nothing rolls back, and one
// unusable id must not sink the rest. So the outcome is never a single
// ok — every id lands in exactly one of the two lists, and a caller can
// tell precisely what moved and what did not. Reporting blanket success
// for a batch that partly failed would be the same lie as reporting a
// soft move a server never performed.
type BulkResult struct {
	Succeeded []string      `json:"succeeded"`
	Failed    []BulkFailure `json:"failed"`
}

// NewBulkResult prepares a result sized for a batch. Both lists start
// non-nil so an empty one renders as [] rather than null — "nothing
// failed" must not be indistinguishable from "no report".
func NewBulkResult(size int) BulkResult {
	return BulkResult{Succeeded: make([]string, 0, size), Failed: make([]BulkFailure, 0)}
}

// Succeed records an id the operation acted on.
func (r *BulkResult) Succeed(id string) { r.Succeeded = append(r.Succeeded, id) }

// Fail records an id the operation could not act on, and why.
func (r *BulkResult) Fail(id string, err error) {
	r.Failed = append(r.Failed, BulkFailure{ID: id, Err: err})
}

// BulkMessage is one message a batched fetch returned: the id it was
// asked for, and the raw RFC 5322 bytes. Raw is []byte rather than a
// string so encoding/json renders it base64 — the identical wire form
// the single-message fetch produces, so a client can decode both the
// same way.
type BulkMessage struct {
	ID  string `json:"id"`
	Raw []byte `json:"raw"`
}

// FetchResult reports a batched fetch id by id.
//
// It is BulkResult's shape for an operation whose successes carry a
// payload: the ids that were read come back with their bytes, the ids
// that could not appear individually with a reason, and there is no
// blanket ok. One id the backend does not hold is that id's own failure,
// not a failed call — the caller keeps the mail it asked for and can see
// precisely what is missing.
type FetchResult struct {
	Fetched []BulkMessage `json:"fetched"`
	Failed  []BulkFailure `json:"failed"`
}

// NewFetchResult prepares a result sized for a batch. Both lists start
// non-nil so an empty one renders as [] rather than null.
func NewFetchResult(size int) FetchResult {
	return FetchResult{Fetched: make([]BulkMessage, 0, size), Failed: make([]BulkFailure, 0)}
}

// Add records a message the fetch read.
func (r *FetchResult) Add(id string, raw []byte) {
	r.Fetched = append(r.Fetched, BulkMessage{ID: id, Raw: raw})
}

// Fail records an id the fetch could not read, and why.
func (r *FetchResult) Fail(id string, err error) {
	r.Failed = append(r.Failed, BulkFailure{ID: id, Err: err})
}

// MessageSizer is an optional Mailbox capability: reporting how large
// messages are without reading them. It exists so a batched fetch can be
// refused before it allocates anything — the pre-flight CheckFetchBudget
// is fed from.
//
// Both built-in backends answer cheaply: IMAP fetches RFC822.SIZE for the
// whole set in one round trip, and maildir stats the files. A backend
// that cannot answer cannot have its fetches bounded, so it is told to
// fetch one id at a time rather than quietly allowed an unbounded batch.
//
// Like Mailbox, it honours its context. Sizing never changes a message's
// state — it is as read-only as Fetch, and does not set \Seen.
type MessageSizer interface {
	// Sizes reports the raw byte size of each id the backend holds. Ids
	// it does not hold are simply absent from the map rather than an
	// error: a stale id is that id's own failure once the fetch runs, and
	// answering the measurable part is what lets the rest proceed.
	Sizes(ctx context.Context, ids []string) (map[string]int64, error)
}

// BulkFetcher is an optional Mailbox capability: reading many messages in
// one call. Backends that gain nothing from batching do not implement it
// and are looped over instead, after the same size pre-flight.
//
// Implementations must measure before they read. The whole point of the
// capability is that a batch of fifty is one round trip rather than
// fifty; the whole point of the pre-flight is that a batch of fifty large
// messages is refused rather than delivered.
//
// Like Mailbox, it honours its context, and fetching never marks anything
// seen.
type BulkFetcher interface {
	// FetchMany returns the raw bytes for each id and reports the outcome
	// per id. The error is reserved for a failure of the batch as a whole
	// — an invalid batch, one measured over MaxFetchBytes, or a backend
	// that could not be reached at all.
	FetchMany(ctx context.Context, ids []string) (FetchResult, error)
}

// BulkMailbox is an optional Mailbox capability: acknowledging many
// messages in one call. Backends that gain nothing from batching simply
// do not implement it and are looped over instead — the saving is round
// trips to a remote server, which local file operations do not pay.
//
// Like Mailbox, it honours its context, and bulk mark-seen keeps
// MarkSeen's idempotence: ids already seen succeed and change nothing.
type BulkMailbox interface {
	// MarkSeenMany marks each id seen and reports the outcome per id.
	// The error is reserved for a failure of the batch as a whole — an
	// invalid batch, or a backend that could not be reached at all.
	MarkSeenMany(ctx context.Context, ids []string) (BulkResult, error)
}

// BulkCurator is an optional Curator capability: curating many messages
// in one call, for backends where a batch costs less than the messages
// in it. On IMAP a single-message soft move costs a folder resolution, an
// existence check, a COPY and a STORE; fifty of them is enough to hit a
// provider's rate limit, while one batch is one of each.
//
// Both operations take explicit ids and only explicit ids. There is
// deliberately no predicate or query form: message content reaches every
// tool, so a bulk-by-query delete would let one injected sentence in an
// email body destroy an unbounded amount of mail. The caller passes a
// list it already enumerated and can be held to.
//
// Like Curator, both are soft moves, both honour their context, and
// neither rolls anything back — a partial batch is reported precisely,
// not undone.
type BulkCurator interface {
	// ArchiveMany archives each id and reports the outcome per id.
	ArchiveMany(ctx context.Context, ids []string) (BulkResult, error)
	// DeleteMany trashes each id and reports the outcome per id.
	DeleteMany(ctx context.Context, ids []string) (BulkResult, error)
}

// CurationRoute names how a curation destination was decided. It exists
// so the choice can be shown to a human before mail moves, rather than
// inferred afterwards from where mail landed.
type CurationRoute string

const (
	// RouteOverride means the operator named the folder in configuration.
	RouteOverride CurationRoute = "override"
	// RouteDeclared means the server declared it (RFC 6154 SPECIAL-USE).
	RouteDeclared CurationRoute = "declared"
	// RouteConvention means the conventional path inside the personal
	// namespace, which already exists.
	RouteConvention CurationRoute = "convention"
	// RouteAlias means an existing folder under a known localized or
	// legacy name, matched only to avoid creating a duplicate beside it.
	RouteAlias CurationRoute = "alias"
	// RouteCreate means nothing suitable exists and the folder would be
	// created.
	RouteCreate CurationRoute = "create"
	// RouteFixed means the backend's layout is not negotiable — the
	// maildir backend always files into .archive and .trash.
	RouteFixed CurationRoute = "fixed"
)

// CurationDestination is where one curation operation would file a
// message, and how that was decided.
type CurationDestination struct {
	Folder string        `json:"folder"`
	Route  CurationRoute `json:"route"`
}

// CurationPlan reports both curation destinations without moving
// anything — the answer to "where would this go?" asked in advance.
type CurationPlan struct {
	Archive CurationDestination `json:"archive"`
	Trash   CurationDestination `json:"trash"`
}

// CurationInspector is an optional Curator capability: reporting where
// curation would file, before it files anything. Resolution can consult
// the server, so this is a query, not a pure accessor — and, like every
// other query here, it honours its context.
type CurationInspector interface {
	CurationPlan(ctx context.Context) (CurationPlan, error)
}

// MaxScanMessages caps how many messages the search fallback may read in
// one call.
//
// The fallback is the last resort: it only runs for a backend that
// implements neither ScopedSearcher nor Searcher. Both shipped backends
// search natively — maildir scans locally, IMAP searches server-side —
// so this bounds a third-party backend implementing the bare Mailbox
// port, and changes nothing for the mailboxes briefkasten ships with.
//
// What it bounds is the number of messages examined, not the bytes read.
// The scan holds one body at a time and discards it after matching, so
// peak memory is already one message; the quantity that grows without
// limit is the number of Fetch calls, each of them a round trip on a
// backend that may well be remote. A byte budget on top of that could
// only be enforced by counting bytes as they arrive — a limit discovered
// by spending the very resource it protects. The pre-flight in
// CheckFetchBudget can refuse before reading because a batch fetch is
// measurable up front; a backend that cannot even search is not one to
// rely on to measure, so the honest bound here is the count, which is
// known from the listing before a single message is read.
//
// 5000 is fifty times MaxBulkIDs. A read-only scan nobody had to
// confirm may reasonably range wider than a batch a human approved, but
// not without limit: it sits well above any backlog a person searches
// through by hand, and low enough that the worst case is thousands of
// reads rather than millions.
const MaxScanMessages = 5000

// ErrScanTooLarge refuses a fallback scan over MaxScanMessages.
var ErrScanTooLarge = errors.New("briefkasten: mailbox too large to scan")

// CheckScanBudget refuses a scan wider than MaxScanMessages before any of
// it runs.
//
// The refusal is whole, and it is deliberately not a truncation. A scan
// that stopped at the cap would return the matches among the messages it
// happened to reach and no way to tell that apart from the matches in the
// mailbox — a wrong answer wearing the shape of a right one, which is
// worse than no answer. This package already prefers the loud failure
// twice over: listMailbox refuses a scope the backend cannot speak for
// rather than quietly answering with unread mail, and CheckFetchBudget
// refuses an oversized batch rather than trimming it. So the caller is
// told the count, the cap, and what to do about it: narrow the scope, or
// give the backend a native search.
// CheckScanBudget is exported so every layer that can run the scan
// enforces the same bound: the resilience decorator carries its own copy
// of the fallback, and a bound only one of them applies is not a bound.
func CheckScanBudget(ids int, scope Scope) error {
	if ids > MaxScanMessages {
		return fmt.Errorf(
			"%w: %d messages in scope %q exceeds the %d-message scan budget, and this backend has no native search"+
				" — narrow the search to a folder or a smaller scope, or give the backend a Searcher",
			ErrScanTooLarge, ids, string(scope), MaxScanMessages)
	}
	return nil
}

// ErrBadID rejects message ids that try to escape the mailbox.
var ErrBadID = errors.New("briefkasten: invalid message id")

// ErrBadScope rejects listing scopes outside unread/read/all.
var ErrBadScope = errors.New("briefkasten: invalid scope")
