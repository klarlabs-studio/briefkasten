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

// ErrBadID rejects message ids that try to escape the mailbox.
var ErrBadID = errors.New("briefkasten: invalid message id")
