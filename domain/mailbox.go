// Package domain holds briefkasten's bounded context: the ports and
// invariants of a mailbox served to agents and humans. It imports no
// infrastructure — backends, transports, and presentation all depend on
// this package, never the reverse.
package domain

import (
	"errors"
	"fmt"
)

// Mailbox is the core port: anything that can list unread messages,
// fetch raw RFC 5322 bytes, and mark a message as seen.
type Mailbox interface {
	// ListUnread returns the ids of messages not yet marked seen.
	ListUnread() ([]string, error)
	// Fetch returns the raw message bytes for an id.
	Fetch(id string) ([]byte, error)
	// MarkSeen marks a message as processed so it is not listed again.
	MarkSeen(id string) error
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
type ScopedMailbox interface {
	// List returns the ids covered by scope.
	List(scope Scope) ([]string, error)
}

// Searcher is an optional Mailbox capability: full-text search over the
// unread backlog.
type Searcher interface {
	// Search returns the unread ids whose raw content matches the query
	// (case-insensitive).
	Search(query string) ([]string, error)
}

// ScopedSearcher is an optional Searcher capability: search restricted
// to a scope rather than always to the unread backlog.
type ScopedSearcher interface {
	// SearchScope returns the ids within scope whose raw content matches
	// the query (case-insensitive).
	SearchScope(scope Scope, query string) ([]string, error)
}

// FolderMailbox is an optional Mailbox capability: backends with multiple
// folders list them and hand out folder-scoped instances.
type FolderMailbox interface {
	// Folders returns the available folder names; the default folder is
	// included (as "INBOX" for the dir backend).
	Folders() ([]string, error)
	// InFolder returns a Mailbox scoped to the named folder.
	InFolder(name string) (Mailbox, error)
}

// Curator is an optional Mailbox capability: human curation of the
// unread backlog. Both operations are soft moves — Archive files the
// message away, Delete moves it to trash. Nothing is ever expunged;
// data is never destroyed.
type Curator interface {
	// Archive moves an unread message to the archive.
	Archive(id string) error
	// Delete moves an unread message to the trash.
	Delete(id string) error
}

// ErrBadID rejects message ids that try to escape the mailbox.
var ErrBadID = errors.New("briefkasten: invalid message id")

// ErrBadScope rejects listing scopes outside unread/read/all.
var ErrBadScope = errors.New("briefkasten: invalid scope")
