package briefkasten

import (
	"encoding/base64"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

// callMap invokes a tool via the test client and returns the handler's map
// result. testutil's harness passes handler return values through
// unserialized; the real transport JSON-marshals them into a text block.
func callMap(t *testing.T, client *testutil.TestClient, name string, args map[string]any) map[string]any {
	t.Helper()
	resp, err := client.CallToolRaw(name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if resp.Error != nil {
		t.Fatalf("%s: %v", name, resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	out, ok := content[0]["text"].(map[string]any)
	if !ok {
		t.Fatalf("%s: unexpected payload %T", name, content[0]["text"])
	}
	return out
}

func TestServerTools(t *testing.T) {
	mb, root := newDir(t)
	drop(t, root, "msg1.eml", "From: a@b.c\r\nSubject: Quittung\r\n\r\nhi")
	client := testutil.NewTestClient(t, NewServer(mb))

	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"email.list_unread", "email.fetch", "email.mark_seen"} {
		if !names[want] {
			t.Errorf("tool %q missing (have %v)", want, names)
		}
	}

	listed := callMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "msg1.eml" {
		t.Fatalf("ids = %v", ids)
	}

	fetched := callMap(t, client, "email.fetch", map[string]any{"id": "msg1.eml"})
	raw, err := base64.StdEncoding.DecodeString(fetched["raw"].(string))
	if err != nil {
		t.Fatalf("raw not base64: %v", err)
	}
	if string(raw[:5]) != "From:" {
		t.Errorf("raw = %q", raw[:10])
	}

	callMap(t, client, "email.mark_seen", map[string]any{"id": "msg1.eml"})
	listed = callMap(t, client, "email.list_unread", map[string]any{})
	if n := len(listed["ids"].([]string)); n != 0 {
		t.Errorf("unread after seen = %d", n)
	}
}
