// Package briefkasten exposes a mailbox as a Model Context Protocol server.
//
// Briefkasten (German: letterbox) turns any message store into three MCP
// tools — email.list_unread, email.fetch, email.mark_seen — so agent
// runtimes and ingestion pipelines can pull mail through a stable,
// language-agnostic contract instead of binding to IMAP libraries.
//
// Backends implement the Mailbox interface. The built-in DirMailbox serves a
// maildir-style directory (drop .eml files into new/); IMAP, Gmail, or any
// other source slot in behind the same three methods.
package briefkasten

import "errors"

// Mailbox is the backend port: anything that can list unread messages,
// fetch raw RFC 5322 bytes, and mark a message as seen.
type Mailbox interface {
	// ListUnread returns the ids of messages not yet marked seen.
	ListUnread() ([]string, error)
	// Fetch returns the raw message bytes for an unread id.
	Fetch(id string) ([]byte, error)
	// MarkSeen marks a message as processed so it is not listed again.
	MarkSeen(id string) error
}

// Searcher is an optional Mailbox capability: full-text search over the
// unread backlog. Backends without it get a server-side fallback
// (list + fetch + substring match).
type Searcher interface {
	// Search returns the unread ids whose raw content matches the query
	// (case-insensitive).
	Search(query string) ([]string, error)
}

// FolderMailbox is an optional Mailbox capability: backends with multiple
// folders list them and hand out folder-scoped instances. The base Mailbox
// always serves the default folder (INBOX).
type FolderMailbox interface {
	// Folders returns the available folder names; the default folder is
	// included (as "INBOX" for the dir backend).
	Folders() ([]string, error)
	// InFolder returns a Mailbox scoped to the named folder.
	InFolder(name string) (Mailbox, error)
}

// scoped resolves an optional folder argument: empty keeps the default
// mailbox, otherwise the backend must support folders.
func scoped(mb Mailbox, folder string) (Mailbox, error) {
	if folder == "" {
		return mb, nil
	}
	fm, ok := mb.(FolderMailbox)
	if !ok {
		return nil, errors.New("briefkasten: backend has no folder support")
	}
	return fm.InFolder(folder)
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
