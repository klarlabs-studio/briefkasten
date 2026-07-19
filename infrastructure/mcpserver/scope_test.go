package mcpserver

import (
	"strings"
	"testing"
)

// The MCP surface must expose read mail without changing what an
// unscoped call does.
func TestListToolScope(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")
	drop(t, root, "b.eml", "From: a@b.c\r\nSubject: Beta\r\n\r\nbeta")
	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})

	unread := callMap(t, client, "email.list", map[string]any{})
	if ids := unread["ids"].([]string); len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("default scope ids = %v, want [b.eml]", ids)
	}
	if got := unread["scope"]; got != "unread" {
		t.Errorf("scope = %v, want unread", got)
	}

	read := callMap(t, client, "email.list", map[string]any{"scope": "read"})
	if ids := read["ids"].([]string); len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("read ids = %v, want [a.eml]", ids)
	}

	all := callMap(t, client, "email.list", map[string]any{"scope": "all"})
	if ids := all["ids"].([]string); len(ids) != 2 {
		t.Errorf("all ids = %v, want 2", ids)
	}

	// The legacy tool keeps its unread-only contract.
	legacy := callMap(t, client, "email.list_unread", map[string]any{})
	if ids := legacy["ids"].([]string); len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("email.list_unread ids = %v, want [b.eml]", ids)
	}
}

func TestListToolRejectsBadScope(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")

	_, err := client.CallTool("email.list", map[string]any{"scope": "nonsense"})
	if err == nil {
		t.Fatal("email.list with scope=nonsense succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error = %v, want it to mention the scope", err)
	}
}

func TestSearchToolScope(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Quittung\r\n\r\nbeleg")
	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})

	unread := callMap(t, client, "email.search", map[string]any{"query": "quittung"})
	if ids := unread["ids"].([]string); len(ids) != 0 {
		t.Errorf("unread search = %v, want none (it is read)", ids)
	}

	read := callMap(t, client, "email.search", map[string]any{"query": "quittung", "scope": "read"})
	if ids := read["ids"].([]string); len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("read search = %v, want [a.eml]", ids)
	}
}

// Fetching read mail must work and must not resurrect it as unread.
func TestFetchReadMessageOverMCP(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: a@b.c\r\nSubject: Alpha\r\n\r\nalpha")
	callMap(t, client, "email.mark_seen", map[string]any{"id": "a.eml"})

	if out := callMap(t, client, "email.fetch", map[string]any{"id": "a.eml"}); out["raw"] == "" {
		t.Fatal("fetch of read message returned empty raw")
	}
	unread := callMap(t, client, "email.list", map[string]any{})
	if ids := unread["ids"].([]string); len(ids) != 0 {
		t.Errorf("unread after fetching read mail = %v, want none", ids)
	}
}
