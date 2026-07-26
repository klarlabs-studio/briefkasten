package mcpserver

import (
	"context"

	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/server"
)

// registerUI serves the interactive inbox as an MCP Apps resource.
func registerUI(srv *mcp.Server) {
	srv.Resource(InboxUIResourceURI).
		Name("Briefkasten inbox").
		Description("Interactive inbox: browse folders, list unread/read/all mail, search it, read a message, mark seen, archive, delete, reply (or reply-all), forward, and compose with cc/bcc. Sends are never self-confirmed — the host still elicits.").
		MimeType("text/html").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			return &server.ResourceContent{URI: uri, MimeType: "text/html", Text: inboxHTML}, nil
		})
}
