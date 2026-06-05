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
	mcp "github.com/felixgeelhaar/mcp-go"

	"github.com/felixgeelhaar/briefkasten/application"
	"github.com/felixgeelhaar/briefkasten/domain"
	"github.com/felixgeelhaar/briefkasten/infrastructure/auth"
	"github.com/felixgeelhaar/briefkasten/infrastructure/imap"
	"github.com/felixgeelhaar/briefkasten/infrastructure/maildir"
	"github.com/felixgeelhaar/briefkasten/infrastructure/mcpserver"
	"github.com/felixgeelhaar/briefkasten/infrastructure/resilience"
	"github.com/felixgeelhaar/briefkasten/infrastructure/smtp"
)

// Mailbox and friends are the domain ports, re-exported for consumers.
//
//nolint:revive // grouped aliases share one doc comment by design
type (
	Mailbox         = domain.Mailbox
	Searcher        = domain.Searcher
	FolderMailbox   = domain.FolderMailbox
	Curator         = domain.Curator
	Sender          = domain.Sender
	OutboundMessage = domain.OutboundMessage
)

// ErrBadID rejects message ids that try to escape the mailbox.
var ErrBadID = domain.ErrBadID

// Application types.
type (
	// Service is the shared use-case layer; MCP and CLI both call it.
	Service = application.Service
	// Outbox drives outbound messages through their lifecycle.
	Outbox = application.Outbox
	// Switchable is a runtime-swappable mailbox decorator.
	Switchable = application.Switchable
)

// NewService wires the shared use cases over a default mailbox and named
// accounts.
func NewService(mb Mailbox, accounts map[string]Mailbox) *Service {
	return application.NewService(mb, accounts)
}

// NewSwitchable wraps an initial backend for runtime swapping.
func NewSwitchable(mb Mailbox) *Switchable { return application.NewSwitchable(mb) }

// DirMailbox and IMAPMailbox are the built-in backends.
//
//nolint:revive // grouped aliases share one doc comment by design
type (
	DirMailbox  = maildir.Mailbox
	IMAPMailbox = imap.Mailbox
	IMAPConfig  = imap.Config
)

// NewDirMailbox prepares a maildir-style directory backend.
func NewDirMailbox(root string) (*DirMailbox, error) { return maildir.New(root) }

// NewIMAPMailbox validates cfg and returns an IMAP backend.
func NewIMAPMailbox(cfg IMAPConfig) (*IMAPMailbox, error) { return imap.New(cfg) }

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

// NewOutbox binds a maildir-backed outbox store to the sender — the
// pre-restructure convenience constructor.
func NewOutbox(root string, sender Sender) (*Outbox, error) {
	store, err := maildir.NewOutboxStore(root)
	if err != nil {
		return nil, err
	}
	return application.NewOutbox(store, sender), nil
}

// MCP presentation.
const (
	// Instructions is the server guidance shown to AI models.
	Instructions = mcpserver.Instructions
	// InboxUIResourceURI is the MCP Apps inbox resource.
	InboxUIResourceURI = mcpserver.InboxUIResourceURI
)

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
