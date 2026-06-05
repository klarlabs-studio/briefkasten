package briefkasten

import (
	"context"
	_ "embed"

	"github.com/felixgeelhaar/mcp-go/server"
)

// InboxUIResourceURI is the MCP Apps resource the inbox UI is served from.
const InboxUIResourceURI = "ui://briefkasten/inbox"

//go:embed ui/inbox.html
var inboxHTML string

// RegisterUI serves the interactive inbox as an MCP Apps resource: hosts
// that support the extension render it as an embedded app whose buttons
// call the email.* tools through the postMessage JSON-RPC bridge. Tools
// link to it via their _meta ui resourceUri (see UIResource in NewServer).
func RegisterUI(srv *server.Server) {
	srv.Resource(InboxUIResourceURI).
		Name("Briefkasten inbox").
		Description("Interactive inbox: list unread, read, mark seen, compose.").
		MimeType("text/html").
		Handler(func(_ context.Context, uri string, _ map[string]string) (*server.ResourceContent, error) {
			return &server.ResourceContent{URI: uri, MimeType: "text/html", Text: inboxHTML}, nil
		})
}
