// Package domain holds briefkasten's bounded context: the ports and
// invariants of a mailbox served to agents and humans. It imports no
// infrastructure — backends, transports, and presentation all depend on
// this package, never the reverse.
package domain

import (
	"context"
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
