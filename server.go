package briefkasten

import (
	"context"
	"encoding/base64"

	mcp "github.com/felixgeelhaar/mcp-go"
)

// NewServer exposes a Mailbox as an MCP server. The three email.* tools are
// the connector contract: any client that speaks them can consume any
// backend.
func NewServer(mb Mailbox) *mcp.Server {
	srv := mcp.NewServer(mcp.ServerInfo{Name: "briefkasten", Version: "0.4.0"})

	srv.Tool("email.list_unread").
		Description("List ids of unread messages in the mailbox.").
		Handler(func(_ context.Context, _ struct{}) (map[string]any, error) {
			ids, err := mb.ListUnread()
			if err != nil {
				return nil, err
			}
			if ids == nil {
				ids = []string{}
			}
			return map[string]any{"ids": ids}, nil
		})

	srv.Tool("email.fetch").
		Description("Fetch the raw RFC 5322 message for an unread id, base64-encoded.").
		Handler(func(_ context.Context, in struct {
			ID string `json:"id"`
		}) (map[string]any, error) {
			raw, err := mb.Fetch(in.ID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"raw": base64.StdEncoding.EncodeToString(raw)}, nil
		})

	srv.Tool("email.mark_seen").
		Description("Mark a message as seen so it is not ingested again.").
		Handler(func(_ context.Context, in struct {
			ID string `json:"id"`
		}) (map[string]any, error) {
			if err := mb.MarkSeen(in.ID); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})

	return srv
}
