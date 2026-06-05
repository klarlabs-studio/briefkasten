package briefkasten

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/felixgeelhaar/mcp-go/server"
)

// Instructions is the server guidance shown to AI models connecting to
// briefkasten.
const Instructions = `Briefkasten serves a mailbox over MCP. Pull unread mail with
email.list_unread + email.fetch, then acknowledge each ingested message
with email.mark_seen — only after processing succeeded, so failures stay
unread for retry. Read state cheaply through the email://inbox and
email://outbox resources. Send mail with email.send (asynchronous: poll
email.send_status). Curate with email.archive / email.delete — both are
soft moves and require human confirmation: the host is asked via
elicitation, or you must ask the user and pass confirm=true. Nothing is
ever expunged.`

// RegisterPrompts exposes prompt templates over the mailbox: hosts get
// ready-made, content-embedded prompts instead of assembling tool calls.
func RegisterPrompts(srv *server.Server, mb Mailbox) {
	srv.Prompt("summarize_inbox").
		Description("Summarize all unread messages: senders, subjects, what needs action.").
		Handler(func(_ context.Context, _ map[string]string) (*server.PromptResult, error) {
			ids, err := mb.ListUnread()
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			b.WriteString("Summarize the following unread messages. For each: sender, subject, one-line gist, and whether it needs action.\n")
			for _, id := range ids {
				raw, err := mb.Fetch(id)
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
			raw, err := mb.Fetch(id)
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
}
