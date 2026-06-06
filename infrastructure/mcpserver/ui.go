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
		Description("Interactive inbox: list unread, read, mark seen, compose.").
		MimeType("text/html").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			return &server.ResourceContent{URI: uri, MimeType: "text/html", Text: inboxHTML}, nil
		})
}
