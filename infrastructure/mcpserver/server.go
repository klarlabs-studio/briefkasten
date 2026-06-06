// Package mcpserver is briefkasten's MCP presentation adapter: every
// tool, resource, and prompt is a thin translation onto the shared
// application use cases — the same methods the CLI calls.
package mcpserver

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"

	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/server"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// InboxUIResourceURI is the MCP Apps resource the inbox UI is served from.
const InboxUIResourceURI = "ui://briefkasten/inbox"

//go:embed ui/inbox.html
var inboxHTML string

// Instructions is the server guidance shown to AI models.
const Instructions = `Briefkasten serves a mailbox over MCP. Pull unread mail with
email.list_unread + email.fetch, then acknowledge each ingested message
with email.mark_seen — only after processing succeeded, so failures stay
unread for retry. Read state cheaply through the email://inbox and
email://outbox resources. Send mail with email.send (asynchronous: poll
email.send_status). Curate with email.archive / email.delete — both are
soft moves and require human confirmation: the host is asked via
elicitation, or you must ask the user and pass confirm=true. Nothing is
ever expunged.`

// Option configures the server surface.
type Option func(*options)

type options struct {
	outbox *application.Outbox
}

// WithOutbox enables the sending tools and outbox resources.
func WithOutbox(ob *application.Outbox) Option {
	return func(o *options) { o.outbox = ob }
}

// New builds the MCP server over the application service. Tools,
// resources, prompts, and the MCP Apps UI all route through svc — one
// code path, shared with the CLI.
func New(svc *application.Service, serverOpts ...Option) *mcp.Server {
	opts := &options{}
	for _, opt := range serverOpts {
		opt(opts)
	}

	srv := mcp.NewServer(mcp.ServerInfo{Name: "briefkasten", Version: "0.8.0"},
		mcp.WithInstructions(Instructions))

	registerTools(srv, svc)
	registerCurateTools(srv, svc)
	if opts.outbox != nil {
		registerSendTools(srv, opts.outbox)
	}
	registerResources(srv, svc, opts.outbox)
	registerPrompts(srv, svc)
	registerUI(srv)
	return srv
}

func registerTools(srv *mcp.Server, svc *application.Service) {
	srv.Tool("email.list_unread").
		Description("List ids of unread messages. Optional: folder (see email://folders), account (see email://accounts).").
		ReadOnly().
		UIResource(InboxUIResourceURI).
		OutputSchema(map[string]any{"ids": []string{"m1.eml"}}).
		Handler(func(_ context.Context, in struct {
			Folder  string `json:"folder,omitempty"`
			Account string `json:"account,omitempty"`
		},
		) (map[string]any, error) {
			ids, err := svc.ListUnread(in.Account, in.Folder)
			if err != nil {
				return nil, err
			}
			return map[string]any{"ids": ids}, nil
		})

	srv.Tool("email.fetch").
		Description("Fetch the raw RFC 5322 message for an unread id, base64-encoded.").
		ReadOnly().
		OutputSchema(map[string]any{"raw": "<base64>"}).
		Handler(func(_ context.Context, in struct {
			ID      string `json:"id"`
			Folder  string `json:"folder,omitempty"`
			Account string `json:"account,omitempty"`
		},
		) (map[string]any, error) {
			raw, err := svc.Read(in.Account, in.Folder, in.ID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"raw": base64.StdEncoding.EncodeToString(raw)}, nil
		})

	srv.Tool("email.mark_seen").
		Description("Mark a message as seen so it is not ingested again.").
		Idempotent().
		OutputSchema(map[string]any{"ok": true}).
		Handler(func(_ context.Context, in struct {
			ID      string `json:"id"`
			Folder  string `json:"folder,omitempty"`
			Account string `json:"account,omitempty"`
		},
		) (map[string]any, error) {
			if err := svc.MarkSeen(in.Account, in.Folder, in.ID); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})

	srv.Tool("email.search").
		Description("Search unread messages for a text query (case-insensitive). Returns matching ids.").
		ReadOnly().
		OutputSchema(map[string]any{"ids": []string{"m1.eml"}}).
		Handler(func(_ context.Context, in struct {
			Query   string `json:"query"`
			Folder  string `json:"folder,omitempty"`
			Account string `json:"account,omitempty"`
		},
		) (map[string]any, error) {
			ids, err := svc.Search(in.Account, in.Folder, in.Query)
			if err != nil {
				return nil, err
			}
			return map[string]any{"ids": ids}, nil
		})
}

// confirmCuration puts a human in the loop before a destructive
// operation: MCP elicitation when the client supports it, an explicit
// confirm flag otherwise.
func confirmCuration(ctx context.Context, confirmed bool, action, id string) error {
	if confirmed {
		return nil
	}
	session := server.SessionFromContext(ctx)
	if session != nil {
		result, err := server.NewElicitor(session).Elicit(ctx, &server.ElicitRequest{
			Message: fmt.Sprintf("Confirm %s of message %q? The message is moved, never destroyed.", action, id),
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		})
		if err == nil {
			if result.Action == "accept" {
				return nil
			}
			return fmt.Errorf("briefkasten: %s of %q declined by user", action, id)
		}
	}
	return errors.New("briefkasten: confirmation required — ask the user, then retry with confirm=true")
}

func registerCurateTools(srv *mcp.Server, svc *application.Service) {
	type curateInput struct {
		ID      string `json:"id"`
		Folder  string `json:"folder,omitempty"`
		Account string `json:"account,omitempty"`
		Confirm bool   `json:"confirm,omitempty"`
	}

	srv.Tool("email.archive").
		Description("Archive an unread message (soft: filed away, never destroyed). Requires human confirmation — the host is asked via elicitation, or pass confirm=true after asking the user yourself.").
		Destructive().
		OutputSchema(map[string]any{"ok": true}).
		Handler(func(ctx context.Context, in curateInput) (map[string]any, error) {
			if err := confirmCuration(ctx, in.Confirm, "archive", in.ID); err != nil {
				return nil, err
			}
			if err := svc.Archive(in.Account, in.Folder, in.ID); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})

	srv.Tool("email.delete").
		Description("Move an unread message to trash (soft delete: never expunged). Requires human confirmation — the host is asked via elicitation, or pass confirm=true after asking the user yourself.").
		Destructive().
		OutputSchema(map[string]any{"ok": true}).
		Handler(func(ctx context.Context, in curateInput) (map[string]any, error) {
			if err := confirmCuration(ctx, in.Confirm, "delete", in.ID); err != nil {
				return nil, err
			}
			if err := svc.Delete(in.Account, in.Folder, in.ID); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})
}

func registerSendTools(srv *mcp.Server, ob *application.Outbox) {
	srv.Tool("email.send").
		Description("Queue an outbound email. Returns the outbox id; delivery is asynchronous — poll email.send_status.").
		OutputSchema(map[string]any{"id": "abc123", "state": "queued"}).
		Handler(func(_ context.Context, in struct {
			To      []string `json:"to"`
			Subject string   `json:"subject"`
			Body    string   `json:"body"`
		},
		) (map[string]any, error) {
			id, err := ob.Enqueue(domain.OutboundMessage{To: in.To, Subject: in.Subject, Body: in.Body})
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": id, "state": "queued"}, nil
		})

	srv.Tool("email.send_status").
		Description("Report the lifecycle state of a queued email: queued, sending, sent, or failed (with error).").
		ReadOnly().
		UIResource(InboxUIResourceURI).
		OutputSchema(map[string]any{"id": "abc123", "state": "sent", "attempts": 1}).
		Handler(func(_ context.Context, in struct {
			ID string `json:"id"`
		},
		) (map[string]any, error) {
			msg, err := ob.Status(in.ID)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"id": msg.ID, "state": msg.State, "attempts": msg.Attempts}
			if msg.Error != "" {
				out["error"] = msg.Error
			}
			return out, nil
		})
}
