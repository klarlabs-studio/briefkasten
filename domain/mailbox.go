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

// ErrBadID rejects message ids that try to escape the mailbox.
var ErrBadID = errors.New("briefkasten: invalid message id")

// ErrBadScope rejects listing scopes outside unread/read/all.
var ErrBadScope = errors.New("briefkasten: invalid scope")
