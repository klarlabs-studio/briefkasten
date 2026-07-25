// Package briefkasten exposes a mailbox as a Model Context Protocol
// server — and the same use cases to a human CLI.
//
// The package is layered hexagonally; this root package is the
// compatibility facade and composition surface:
//
//	domain/         ports and invariants (Mailbox, Curator, outbox lifecycle, …)
//	application/    shared use cases — the MCP tools and the CLI call the
//	                same Service methods
//	infrastructure/ backends (maildir, imap), transports (smtp), auth,
//	                resilience, and the MCP presentation adapter
package briefkasten

import (
	"errors"

	mcp "go.klarlabs.de/mcp"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/auth"
	"go.klarlabs.de/briefkasten/infrastructure/imap"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
	"go.klarlabs.de/briefkasten/infrastructure/mcpserver"
	"go.klarlabs.de/briefkasten/infrastructure/resilience"
	"go.klarlabs.de/briefkasten/infrastructure/smtp"
)

// Mailbox and friends are the domain ports, re-exported for consumers.
//
//nolint:revive // grouped aliases share one doc comment by design
type (
	Mailbox         = domain.Mailbox
	ScopedMailbox   = domain.ScopedMailbox
	Searcher        = domain.Searcher
	ScopedSearcher  = domain.ScopedSearcher
	FolderMailbox   = domain.FolderMailbox
	Curator         = domain.Curator
	BulkMailbox     = domain.BulkMailbox
	BulkCurator     = domain.BulkCurator
	BulkFetcher     = domain.BulkFetcher
	MessageSizer    = domain.MessageSizer
	BulkResult      = domain.BulkResult
	BulkFailure     = domain.BulkFailure
	BulkMessage     = domain.BulkMessage
	FetchResult     = domain.FetchResult
	Sender          = domain.Sender
	OutboundMessage = domain.OutboundMessage
	Attachment      = domain.Attachment
	// Scope selects which slice of a mailbox a listing covers.
	Scope = domain.Scope
)

// Listing scopes, re-exported for consumers.
const (
	ScopeUnread = domain.ScopeUnread
	ScopeRead   = domain.ScopeRead
	ScopeAll    = domain.ScopeAll
)

// ErrBadID rejects message ids that try to escape the mailbox.
var ErrBadID = domain.ErrBadID

// ErrBadScope rejects listing scopes outside unread/read/all.
var ErrBadScope = domain.ErrBadScope

// ErrBulkSize rejects a batch that is empty, over the cap, or repeats an id.
var ErrBulkSize = domain.ErrBulkSize

// ErrFetchTooLarge refuses a batch measured over the fetch budget.
var ErrFetchTooLarge = domain.ErrFetchTooLarge

// MaxBulkIDs caps how many messages one bulk call may act on.
const MaxBulkIDs = domain.MaxBulkIDs

// MaxFetchBytes caps the raw bytes one batched fetch may return in total.
const MaxFetchBytes = domain.MaxFetchBytes

// MaxSummaryMessages caps how many messages one summary prompt embeds.
const MaxSummaryMessages = domain.MaxSummaryMessages

// Application types.
type (
	// Service is the shared use-case layer; MCP and CLI both call it.
	Service = application.Service
	// Outbox drives outbound messages through their lifecycle.
	Outbox = application.Outbox
	// Switchable is a runtime-swappable mailbox decorator.
	Switchable = application.Switchable
	// SwitchableSender is a runtime-swappable outbound-sender decorator.
	SwitchableSender = application.SwitchableSender
)

// NewService wires the shared use cases over a default mailbox and named
// accounts.
func NewService(mb Mailbox, accounts map[string]Mailbox) *Service {
	return application.NewService(mb, accounts)
}

// NewSwitchable wraps an initial backend for runtime swapping.
func NewSwitchable(mb Mailbox) *Switchable { return application.NewSwitchable(mb) }

// NewSwitchableSender wraps an initial sender for runtime swapping.
func NewSwitchableSender(s Sender) *SwitchableSender { return application.NewSwitchableSender(s) }

// DirMailbox and IMAPMailbox are the built-in backends.
//
//nolint:revive // grouped aliases share one doc comment by design
type (
	DirMailbox  = maildir.Mailbox
	IMAPMailbox = imap.Mailbox
	IMAPConfig  = imap.Config
)

// Watcher reports new mail so the MCP server can push resource updates to
// subscribers instead of relying on polling. See Config.BuildWatcher.
type Watcher = domain.MailboxWatcher

// InboxResourceURI is the resource subscribers watch for new-mail updates.
const InboxResourceURI = "email://inbox"

// NewDirMailbox prepares a maildir-style directory backend.
func NewDirMailbox(root string) (*DirMailbox, error) { return maildir.New(root) }

// NewIMAPMailbox validates cfg and returns an IMAP backend.
func NewIMAPMailbox(cfg IMAPConfig) (*IMAPMailbox, error) { return imap.New(cfg) }

// NewDirWatcher watches a maildir new/ directory for arriving mail.
func NewDirWatcher(root string) Watcher { return maildir.NewWatcher(root) }

// NewIMAPWatcher watches an IMAP mailbox for new mail using IDLE.
func NewIMAPWatcher(cfg IMAPConfig) Watcher { return imap.NewWatcher(cfg) }

// NewDirSender delivers messages as .eml files into a maildir new/.
func NewDirSender(root, from string) (*maildir.Sender, error) { return maildir.NewSender(root, from) }

// SMTPSender is the outbound SMTP transport.
//
//nolint:revive // grouped aliases share one doc comment by design
type (
	SMTPSender = smtp.Sender
	SMTPConfig = smtp.Config
)

// NewSMTPSender validates the config and builds the SMTP transport.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) { return smtp.NewSender(cfg) }

// OAuth2Settings configures token-based IMAP/SMTP authentication.
type OAuth2Settings = auth.OAuth2Settings

// ResilientMailbox decorates remote backends with fortify patterns.
//
//nolint:revive // grouped aliases share one doc comment by design
type (
	ResilientMailbox = resilience.Mailbox
	ResilienceConfig = resilience.Config
)

// Resilient wraps mb with timeout, retry, and circuit breaker.
func Resilient(mb Mailbox, cfg ResilienceConfig) *ResilientMailbox {
	return resilience.Wrap(mb, cfg)
}

// ErrOutboxBusy reports that another process owns the outbox directory.
var ErrOutboxBusy = application.ErrOutboxBusy

// NewOutbox binds a maildir-backed outbox store to the sender — the
// pre-restructure convenience constructor, and the one the server uses.
// The store is recovered from any unclean shutdown before use: duplicate
// state files are repaired and messages stranded mid-send are moved to
// failed for explicit retry.
//
// Recovery is skipped when another process already holds the outbox: what
// looks stranded from outside is that process's in-flight delivery, and
// the owner repairs its own directory when it starts. An unreadable record
// does fail construction — the alternative is starting up beside somebody's
// half-written mail and never mentioning it.
func NewOutbox(root string, sender Sender) (*Outbox, error) {
	ob, err := NewClientOutbox(root, sender)
	if err != nil {
		return nil, err
	}
	if err := ob.Recover(); err != nil && !errors.Is(err, ErrOutboxBusy) {
		return nil, err
	}
	return ob, nil
}

// NewClientOutbox binds the same store and sender without recovering.
// A one-shot command is a client of the outbox, not its owner: repair is
// the owner's startup job, and a client that needs it (see the CLI's
// retry) asks for it explicitly rather than as a side effect of connecting.
// Its own writes stay serialised against the owner by the outbox lock.
func NewClientOutbox(root string, sender Sender) (*Outbox, error) {
	store, err := maildir.NewOutboxStore(root)
	if err != nil {
		return nil, err
	}
	return application.NewOutbox(store, sender), nil
}

// BuildClientOutbox is BuildOutbox for a one-shot command: identical
// wiring, no recovery. See NewClientOutbox.
func (c *Config) BuildClientOutbox() (*Outbox, string, error) {
	if c.Outbox.Dir == "" {
		return nil, "", nil
	}
	sender, desc, err := c.buildSender()
	if err != nil {
		return nil, "", err
	}
	ob, err := NewClientOutbox(c.Outbox.Dir, sender)
	if err != nil {
		return nil, "", err
	}
	return ob, desc, nil
}

// MCP presentation.
const (
	// Instructions is the server guidance shown to AI models.
	Instructions = mcpserver.Instructions
	// InboxUIResourceURI is the MCP Apps inbox resource.
	InboxUIResourceURI = mcpserver.InboxUIResourceURI
)

// ForwardAuthorizationHeader is the HTTP transport option that makes the
// Authorization header visible to the auth middleware from
// Config.BuildAuthMiddleware. Pass both to mcp.ServeHTTPWithMiddleware.
var ForwardAuthorizationHeader = mcpserver.ForwardAuthorizationHeader

// ServerOption configures the MCP server surface.
type ServerOption func(*serverConfig)

type serverConfig struct {
	accounts map[string]Mailbox
	outbox   *Outbox
}

// WithAccounts registers named mailboxes alongside the default one.
func WithAccounts(accounts map[string]Mailbox) ServerOption {
	return func(c *serverConfig) { c.accounts = accounts }
}

// WithOutbox enables the sending tools and outbox resources.
func WithOutbox(ob *Outbox) ServerOption {
	return func(c *serverConfig) { c.outbox = ob }
}

// NewServer exposes a Mailbox as an MCP server: the email.* tool contract
// plus resources, prompts, and the MCP Apps UI, all over the shared
// application service.
func NewServer(mb Mailbox, opts ...ServerOption) *mcp.Server {
	cfg := &serverConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	svc := application.NewService(mb, cfg.accounts)
	var mcpOpts []mcpserver.Option
	if cfg.outbox != nil {
		mcpOpts = append(mcpOpts, mcpserver.WithOutbox(cfg.outbox))
	}
	return mcpserver.New(svc, mcpOpts...)
}
