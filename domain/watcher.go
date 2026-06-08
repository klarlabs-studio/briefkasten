package domain

import "context"

// MailboxWatcher signals when a mailbox may have received new mail. Watch
// blocks until ctx is cancelled, invoking onChange whenever a change is
// detected — letting the MCP server push notifications/resources/updated to
// subscribers instead of relying on the consumer to poll.
//
// Not every backend supports watching; backends that cannot are simply never
// constructed (the consumer keeps polling).
type MailboxWatcher interface {
	Watch(ctx context.Context, onChange func()) error
}
