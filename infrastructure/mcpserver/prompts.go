package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/server"

	"go.klarlabs.de/briefkasten/application"
)

var errNoOutbox = errors.New("briefkasten: outbox not configured")

// Prompt-size guards: embedding the whole backlog raw would blow the
// model's context on a busy inbox.
const (
	// defaultSummarizeCount caps how many messages summarize_inbox embeds
	// unless the caller asks for more.
	defaultSummarizeCount = 20
	// maxEmbeddedMessageBytes truncates a single embedded message.
	maxEmbeddedMessageBytes = 16 << 10 // 16 KiB
)

// registerPrompts exposes prompt templates over the mailbox.
func registerPrompts(srv *mcp.Server, svc *application.Service) {
	srv.Prompt("summarize_inbox").
		Description("Summarize unread messages: senders, subjects, what needs action. Embeds up to 'count' messages (default 20), each truncated to keep the prompt bounded.").
		Argument("count", "Max messages to embed (default 20)", false).
		Handler(func(_ context.Context, args map[string]string) (*server.PromptResult, error) {
			count := defaultSummarizeCount
			if v := args["count"]; v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 {
					return nil, fmt.Errorf("summarize_inbox: 'count' must be a positive integer, got %q", v)
				}
				count = n
			}
			ids, err := svc.ListUnread("", "")
			if err != nil {
				return nil, err
			}
			total := len(ids)
			if count < total {
				ids = ids[:count]
			}
			var b strings.Builder
			b.WriteString("Summarize the following unread messages. For each: sender, subject, one-line gist, and whether it needs action.\n")
			for _, id := range ids {
				raw, err := svc.Read("", "", id)
				if err != nil {
					continue
				}
				if len(raw) > maxEmbeddedMessageBytes {
					raw = append(raw[:maxEmbeddedMessageBytes:maxEmbeddedMessageBytes], "\n[... truncated ...]"...)
				}
				fmt.Fprintf(&b, "\n--- Message %s ---\n%s\n", id, raw)
			}
			if total == 0 {
				b.WriteString("\n(The inbox has no unread messages.)")
			} else if total > len(ids) {
				fmt.Fprintf(&b, "\n(%d more unread messages not shown — re-run with a higher 'count' or list them with email.list_unread.)\n", total-len(ids))
			}
			return &server.PromptResult{
				Description: "Inbox summary request with embedded unread messages",
				Messages: []server.PromptMessage{
					{Role: "user", Content: server.TextContent{Type: "text", Text: b.String()}},
				},
			}, nil
		})

	srv.Prompt("draft_reply").
		Description("Draft a reply to a message, read or unread.").
		Argument("id", "Message id from email.list in any scope (see email://inbox)", true).
		Handler(func(_ context.Context, args map[string]string) (*server.PromptResult, error) {
			id := args["id"]
			if id == "" {
				return nil, errors.New("draft_reply: 'id' argument required")
			}
			raw, err := svc.Read("", "", id)
			if err != nil {
				return nil, err
			}
			if len(raw) > maxEmbeddedMessageBytes {
				raw = append(raw[:maxEmbeddedMessageBytes:maxEmbeddedMessageBytes], "\n[... truncated ...]"...)
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
			return completeMessageIDs(svc, arg.Value)
		})
}
