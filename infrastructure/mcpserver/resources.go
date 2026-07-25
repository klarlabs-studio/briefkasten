package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/mail"
	"strings"

	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/server"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// registerResources exposes mailbox and outbox state as MCP resources —
// all reads route through the application service.
func registerResources(srv *mcp.Server, svc *application.Service, ob *application.Outbox) {
	srv.Resource("email://inbox").
		Name("Inbox").
		Description("Unread message ids in the mailbox.").
		MimeType("application/json").
		Handler(func(ctx context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			ids, err := svc.ListUnread(ctx, "", "")
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, map[string]any{"unread": ids})
		})

	srv.Resource("email://inbox/{id}").
		Name("Inbox message").
		Description("Raw RFC 5322 message by id — read or unread; reading never marks a message seen.").
		MimeType("message/rfc822").
		Handler(func(ctx context.Context, uri string, params map[string]string) (*server.ResourceContent, error) {
			raw, err := svc.Read(ctx, "", "", params["id"])
			if err != nil {
				return nil, err
			}
			return &server.ResourceContent{URI: uri, MimeType: "message/rfc822", Text: string(raw)}, nil
		})

	srv.Resource("email://inbox/{id}/headers").
		Name("Inbox message headers").
		Description("Parsed headers (from, to, subject, date, message_id) by id, read or unread — triage without fetching the full message.").
		MimeType("application/json").
		Handler(func(ctx context.Context, uri string, params map[string]string) (*server.ResourceContent, error) {
			raw, err := svc.Read(ctx, "", "", params["id"])
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, parseHeaders(raw))
		})

	srv.ResourceCompletion("email://inbox/{id}").
		Handler(func(ctx context.Context, _ server.CompletionRef, arg server.CompletionArgument) (*server.CompletionResult, error) {
			return completeMessageIDs(ctx, svc, arg.Value)
		})

	srv.Resource("email://folders").
		Name("Folders").
		Description("Available mailbox folders, plus where email.archive and email.delete would file (and how each destination was decided).").
		MimeType("application/json").
		Handler(func(ctx context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			folders, err := svc.Folders(ctx, "")
			if err != nil {
				return nil, err
			}
			payload := map[string]any{"folders": folders}
			// Curation destinations ride along so the answer to "where
			// would this go?" is available before asking a human to
			// approve a move. Best-effort: a backend that cannot report
			// them must not break folder listing.
			if plan, err := svc.CurationPlan(ctx, "", ""); err == nil {
				payload["curation"] = plan
			}
			return jsonResource(uri, payload)
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

// completeMessageIDs suggests message ids beginning with prefix, drawn
// from the whole mailbox: the surfaces that complete an id — the inbox
// resource, the draft_reply prompt — serve read and unread mail alike.
//
// A backend that cannot scope falls back to the unread set. That is safe
// here in a way it never is for email.list: these are typing hints, not
// an answer about what is unread, so the fallback offers fewer
// suggestions rather than passing read mail off as unread.
func completeMessageIDs(ctx context.Context, svc *application.Service, prefix string) (*server.CompletionResult, error) {
	ids, err := svc.List(ctx, "", "", domain.ScopeAll)
	if err != nil {
		if ids, err = svc.ListUnread(ctx, "", ""); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	return &server.CompletionResult{Values: out, Total: len(out)}, nil
}

// parseHeaders extracts the triage-relevant headers from a raw RFC 5322
// message. Best-effort: an unparsable message yields empty fields rather
// than an error — the raw resource stays available either way.
func parseHeaders(raw []byte) map[string]any {
	out := map[string]any{"from": "", "to": "", "subject": "", "date": "", "message_id": ""}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return out
	}
	dec := mime.WordDecoder{}
	decode := func(v string) string {
		if d, err := dec.DecodeHeader(v); err == nil {
			return d
		}
		return v
	}
	out["from"] = decode(msg.Header.Get("From"))
	out["to"] = decode(msg.Header.Get("To"))
	out["subject"] = decode(msg.Header.Get("Subject"))
	out["date"] = msg.Header.Get("Date")
	out["message_id"] = msg.Header.Get("Message-Id")
	return out
}

func jsonResource(uri string, payload any) (*server.ResourceContent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &server.ResourceContent{URI: uri, MimeType: "application/json", Text: string(raw)}, nil
}
