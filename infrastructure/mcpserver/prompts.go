package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/server"

	"go.klarlabs.de/briefkasten/application"
)

var errNoOutbox = errors.New("briefkasten: outbox not configured")

// registerPrompts exposes prompt templates over the mailbox.
func registerPrompts(srv *mcp.Server, svc *application.Service) {
	srv.Prompt("summarize_inbox").
		Description("Summarize all unread messages: senders, subjects, what needs action.").
		Handler(func(_ context.Context, _ map[string]string) (*server.PromptResult, error) {
			ids, err := svc.ListUnread("", "")
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			b.WriteString("Summarize the following unread messages. For each: sender, subject, one-line gist, and whether it needs action.\n")
			for _, id := range ids {
				raw, err := svc.Read("", "", id)
				if err != nil {
					continue
				}
				fmt.Fprintf(&b, "\n--- Message %s ---\n%s\n", id, raw)
			}
			if len(ids) == 0 {
				b.WriteString("\n(The inbox has no unread messages.)")
			}
			return &server.PromptResult{
				Description: "Inbox summary request with embedded unread messages",
				Messages: []server.PromptMessage{
					{Role: "user", Content: server.TextContent{Type: "text", Text: b.String()}},
				},
			}, nil
		})

	srv.Prompt("draft_reply").
		Description("Draft a reply to an unread message.").
		Argument("id", "Unread message id (see email://inbox)", true).
		Handler(func(_ context.Context, args map[string]string) (*server.PromptResult, error) {
			id := args["id"]
			if id == "" {
				return nil, errors.New("draft_reply: 'id' argument required")
			}
			raw, err := svc.Read("", "", id)
			if err != nil {
				return nil, err
			}
			text := fmt.Sprintf(
				"Draft a courteous reply to the following message. Keep the original language. Return only the reply body.\n\n--- Original (%s) ---\n%s",
				id, raw)
			return &server.PromptResult{
				Description: "Reply draft request with embedded original message",
				Messages: []server.PromptMessage{
					{Role: "user", Content: server.TextContent{Type: "text", Text: text}},
				},
			}, nil
		})

	srv.PromptCompletion("draft_reply").
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
}
