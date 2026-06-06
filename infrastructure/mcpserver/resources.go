package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/server"

	"go.klarlabs.de/briefkasten/application"
)

// registerResources exposes mailbox and outbox state as MCP resources —
// all reads route through the application service.
func registerResources(srv *mcp.Server, svc *application.Service, ob *application.Outbox) {
	srv.Resource("email://inbox").
		Name("Inbox").
		Description("Unread message ids in the mailbox.").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			ids, err := svc.ListUnread("", "")
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, map[string]any{"unread": ids})
		})

	srv.Resource("email://inbox/{id}").
		Name("Inbox message").
		Description("Raw RFC 5322 message by unread id.").
		MimeType("message/rfc822").
		Handler(func(_ context.Context, uri string, params map[string]string) (*server.ResourceContent, error) {
			raw, err := svc.Read("", "", params["id"])
			if err != nil {
				return nil, err
			}
			return &server.ResourceContent{URI: uri, MimeType: "message/rfc822", Text: string(raw)}, nil
		})

	srv.ResourceCompletion("email://inbox/{id}").
		Handler(func(_ context.Context, _ server.CompletionRef, arg server.CompletionArgument) (*server.CompletionResult, error) {
			ids, err := svc.ListUnread("", "")
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
			folders, err := svc.Folders("")
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, map[string]any{"folders": folders})
		})

	srv.Resource("email://accounts").
		Name("Accounts").
		Description("Configured mailbox accounts; \"default\" is the primary.").
		MimeType("application/json").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			return jsonResource(uri, map[string]any{"accounts": svc.Accounts()})
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
				return nil, errNoOutbox
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
