package briefkasten

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/felixgeelhaar/mcp-go/server"
)

// RegisterResources exposes the mailbox and outbox as MCP resources:
//
//	email://inbox          unread summary (JSON)
//	email://inbox/{id}     raw RFC 5322 message
//	email://outbox         per-state id summary (JSON)
//	email://outbox/{id}    outbound message status (JSON)
//
// Hosts read state directly instead of spending tool calls; {id} arguments
// complete from live data. ob may be nil when sending is not configured —
// the outbox resources then report an empty outbox.
func RegisterResources(srv *server.Server, mb Mailbox, ob *Outbox, serverOpts ...ServerOption) {
	opts := &serverOptions{}
	for _, opt := range serverOpts {
		opt(opts)
	}

	srv.Resource("email://accounts").
		Name("Accounts").
		Description("Configured mailbox accounts; \"default\" is the primary.").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			names := []string{"default"}
			for name := range opts.accounts {
				names = append(names, name)
			}
			sort.Strings(names[1:])
			return jsonResource(uri, map[string]any{"accounts": names})
		})

	srv.Resource("email://inbox").
		Name("Inbox").
		Description("Unread message ids in the mailbox.").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			ids, err := mb.ListUnread()
			if err != nil {
				return nil, err
			}
			if ids == nil {
				ids = []string{}
			}
			return jsonResource(uri, map[string]any{"unread": ids})
		})

	srv.Resource("email://inbox/{id}").
		Name("Inbox message").
		Description("Raw RFC 5322 message by unread id.").
		MimeType("message/rfc822").
		Handler(func(_ context.Context, uri string, params map[string]string) (*server.ResourceContent, error) {
			raw, err := mb.Fetch(params["id"])
			if err != nil {
				return nil, err
			}
			return &server.ResourceContent{URI: uri, MimeType: "message/rfc822", Text: string(raw)}, nil
		})

	srv.ResourceCompletion("email://inbox/{id}").
		Handler(func(_ context.Context, _ server.CompletionRef, arg server.CompletionArgument) (*server.CompletionResult, error) {
			ids, err := mb.ListUnread()
			if err != nil {
				return nil, err
			}
			var out []string
			for _, id := range ids {
				if strings.HasPrefix(id, arg.Value) {
					out = append(out, id)
				}
			}
			return &server.CompletionResult{Values: out, Total: len(out)}, nil
		})

	srv.Resource("email://folders").
		Name("Folders").
		Description("Available mailbox folders.").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			folders := []string{"INBOX"}
			if fm, ok := mb.(FolderMailbox); ok {
				var err error
				folders, err = fm.Folders()
				if err != nil {
					return nil, err
				}
			}
			return jsonResource(uri, map[string]any{"folders": folders})
		})

	srv.Resource("email://outbox").
		Name("Outbox").
		Description("Outbound message ids grouped by lifecycle state (queued, sending, sent, failed).").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			if ob == nil {
				return jsonResource(uri, map[string][]string{})
			}
			summary, err := ob.Summary()
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, summary)
		})

	srv.Resource("email://outbox/{id}").
		Name("Outbox message").
		Description("Outbound message status by outbox id.").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, params map[string]string) (*server.ResourceContent, error) {
			if ob == nil {
				return nil, fmt.Errorf("%w: outbox not configured", ErrBadID)
			}
			msg, err := ob.Status(params["id"])
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, msg)
		})
}

func jsonResource(uri string, payload any) (*server.ResourceContent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &server.ResourceContent{URI: uri, MimeType: "application/json", Text: string(raw)}, nil
}
