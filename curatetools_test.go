package briefkasten

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

func curateServer(t *testing.T) (*testutil.TestClient, string) {
	t.Helper()
	mb, root := newDir(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: Old\r\n\r\na")
	return testutil.NewTestClient(t, NewServer(mb)), root
}

// Without an elicitation-capable session, the destructive tools demand an
// explicit confirm flag — the agent must put a human in the loop first.
func TestDeleteRequiresConfirmationWithoutElicitation(t *testing.T) {
	client, root := curateServer(t)

	_, err := client.CallToolRaw("email.delete", map[string]any{"id": "a.eml"})
	if err == nil || !strings.Contains(err.Error(), "confirmation required") {
		t.Fatalf("unconfirmed delete: err = %v, want confirmation demand", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "new", "a.eml"))
	if len(raw) == 0 {
		t.Fatal("message gone despite refusal")
	}

	// Explicit confirmation proceeds.
	out := callMap(t, client, "email.delete", map[string]any{"id": "a.eml", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("confirmed delete = %v", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "new", "a.eml")); err != nil {
		t.Errorf("not in trash: %v", err)
	}
}

func TestArchiveRequiresConfirmationWithoutElicitation(t *testing.T) {
	client, root := curateServer(t)

	if _, err := client.CallToolRaw("email.archive", map[string]any{"id": "a.eml"}); err == nil || !strings.Contains(err.Error(), "confirmation required") {
		t.Fatalf("unconfirmed archive: err = %v, want confirmation demand", err)
	}

	out := callMap(t, client, "email.archive", map[string]any{"id": "a.eml", "confirm": true})
	if out["ok"] != true {
		t.Fatalf("confirmed archive = %v", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".archive", "new", "a.eml")); err != nil {
		t.Errorf("not archived: %v", err)
	}
}

func TestCurateToolsAnnotatedDestructive(t *testing.T) {
	mb, _ := newDir(t)
	srv := NewServer(mb)
	destructive := map[string]bool{}
	for _, tool := range srv.Tools() {
		if tool.Annotations != nil && tool.Annotations.DestructiveHint != nil {
			destructive[tool.Name] = *tool.Annotations.DestructiveHint
		}
	}
	for _, want := range []string{"email.archive", "email.delete"} {
		if !destructive[want] {
			t.Errorf("%s not annotated destructive (have %v)", want, destructive)
		}
	}
}

func TestInstructionsExplainHITL(t *testing.T) {
	if !strings.Contains(Instructions, "confirm") {
		t.Errorf("instructions silent on confirmation: %q", Instructions)
	}
}
