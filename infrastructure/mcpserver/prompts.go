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
	"go.klarlabs.de/briefkasten/domain"
)

var errNoOutbox = errors.New("briefkasten: outbox not configured")

// Prompt-size guards: embedding the whole backlog raw would blow the
// model's context on a busy inbox. The hard ceiling is
// domain.MaxSummaryMessages; these two only shape what happens below it —
// what a caller gets without asking, and how much of one message is
// quoted. Multiplied out they bound the worst case: at most
// domain.MaxSummaryMessages × maxEmbeddedMessageBytes of embedded mail.
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
		Description(fmt.Sprintf("Summarize unread messages: senders, subjects, what needs action. Embeds up to 'count' messages (default %d, capped at %d), each truncated to keep the prompt bounded.",
			defaultSummarizeCount, domain.MaxSummaryMessages)).
		Argument("count", fmt.Sprintf("Max messages to embed (default %d, capped at %d)", defaultSummarizeCount, domain.MaxSummaryMessages), false).
		Handler(func(ctx context.Context, args map[string]string) (*server.PromptResult, error) {
			count := defaultSummarizeCount
			if v := args["count"]; v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 {
					return nil, fmt.Errorf("summarize_inbox: 'count' must be a positive integer, got %q", v)
				}
				count = n
			}
			// Over the cap is clamped, never refused: this prompt only
			// reads, so the caller gets the bounded version of what it
			// asked for and is told so below.
			asked := count
			if count > domain.MaxSummaryMessages {
				count = domain.MaxSummaryMessages
			}
			ids, err := svc.ListUnread(ctx, "", "")
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
				raw, err := svc.Read(ctx, "", "", id)
				if err != nil {
					// One unreadable message is skipped; a request nobody is
					// waiting for stops embedding the rest.
					if ctx.Err() != nil {
						return nil, err
					}
					continue
				}
				if len(raw) > maxEmbeddedMessageBytes {
					raw = append(raw[:maxEmbeddedMessageBytes:maxEmbeddedMessageBytes], "\n[... truncated ...]"...)
				}
				fmt.Fprintf(&b, "\n--- Message %s ---\n%s\n", id, raw)
			}
			if total == 0 {
				b.WriteString("\n(The inbox has no unread messages.)")
			} else {
				if asked > count {
					fmt.Fprintf(&b, "\n('count' asked for %d messages and was clamped to %d — a prompt quotes message bodies verbatim, so the cap bounds both its size and the untrusted content in it.)\n",
						asked, count)
				}
				if total > len(ids) {
					if count >= domain.MaxSummaryMessages {
						fmt.Fprintf(&b, "\n(%d more unread messages not shown — %d is this prompt's cap, so a higher 'count' will not show more; list them with email.list_unread, or summarize the rest in further batches.)\n",
							total-len(ids), domain.MaxSummaryMessages)
					} else {
						fmt.Fprintf(&b, "\n(%d more unread messages not shown — re-run with a higher 'count' or list them with email.list_unread.)\n", total-len(ids))
					}
				}
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
		Handler(func(ctx context.Context, args map[string]string) (*server.PromptResult, error) {
			id := args["id"]
			if id == "" {
				return nil, errors.New("draft_reply: 'id' argument required")
			}
			raw, err := svc.Read(ctx, "", "", id)
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
		Handler(func(ctx context.Context, _ server.CompletionRef, arg server.CompletionArgument) (*server.CompletionResult, error) {
			return completeMessageIDs(ctx, svc, arg.Value)
		})
}
