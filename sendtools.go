package briefkasten

import (
	"context"

	mcp "github.com/felixgeelhaar/mcp-go"
)

// registerSendTools exposes the outbox over MCP. email.send only enqueues —
// delivery happens in the outbox worker, so a slow SMTP server never blocks
// the tool call, and the caller can watch progress via email.send_status.
func registerSendTools(srv *mcp.Server, ob *Outbox) {
	srv.Tool("email.send").
		Description("Queue an outbound email. Returns the outbox id; delivery is asynchronous — poll email.send_status.").
		OutputSchema(map[string]any{"id": "abc123", "state": "queued"}).
		Handler(func(_ context.Context, in struct {
			To      []string `json:"to"`
			Subject string   `json:"subject"`
			Body    string   `json:"body"`
		}) (map[string]any, error) {
			id, err := ob.Enqueue(OutboundMessage{To: in.To, Subject: in.Subject, Body: in.Body})
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": id, "state": "queued"}, nil
		})

	srv.Tool("email.send_status").
		Description("Report the lifecycle state of a queued email: queued, sending, sent, or failed (with error).").
		ReadOnly().
		OutputSchema(map[string]any{"id": "abc123", "state": "sent", "attempts": 1}).
		Handler(func(_ context.Context, in struct {
			ID string `json:"id"`
		}) (map[string]any, error) {
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
