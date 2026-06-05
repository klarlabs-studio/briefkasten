package briefkasten

import (
	"strings"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

func TestAccountParamRoutesTools(t *testing.T) {
	defaultBox, defaultRoot := newDir(t)
	drop(t, defaultRoot, "d.eml", "From: a@b\r\nSubject: Default\r\n\r\nx")
	business, businessRoot := newDir(t)
	drop(t, businessRoot, "b.eml", "From: c@d\r\nSubject: Business\r\n\r\nGeschäft")

	srv := NewServer(defaultBox, WithAccounts(map[string]Mailbox{"business": business}))
	client := testutil.NewTestClient(t, srv)

	out := callMap(t, client, "email.list_unread", map[string]any{})
	if ids := out["ids"].([]string); len(ids) != 1 || ids[0] != "d.eml" {
		t.Errorf("default ids = %v", ids)
	}

	out = callMap(t, client, "email.list_unread", map[string]any{"account": "business"})
	if ids := out["ids"].([]string); len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("business ids = %v", ids)
	}

	found := callMap(t, client, "email.search", map[string]any{"query": "Geschäft", "account": "business"})
	if n := len(found["ids"].([]string)); n != 1 {
		t.Errorf("business search = %d", n)
	}

	// Unknown account errors.
	resp, err := client.CallToolRaw("email.list_unread", map[string]any{"account": "nope"})
	if err == nil && resp.Error == nil {
		result := resp.Result.(map[string]any)
		if isErr, _ := result["isError"].(bool); !isErr {
			t.Errorf("unknown account accepted: %v", result)
		}
	}

	callMap(t, client, "email.mark_seen", map[string]any{"id": "b.eml", "account": "business"})
	out = callMap(t, client, "email.list_unread", map[string]any{"account": "business"})
	if n := len(out["ids"].([]string)); n != 0 {
		t.Errorf("business unread after seen = %d", n)
	}
}

func TestAccountsResource(t *testing.T) {
	defaultBox, _ := newDir(t)
	business, _ := newDir(t)
	srv := NewServer(defaultBox, WithAccounts(map[string]Mailbox{"business": business}))
	RegisterResources(srv, defaultBox, nil, WithAccounts(map[string]Mailbox{"business": business}))
	client := testutil.NewTestClient(t, srv)

	text, err := client.ReadResource("email://accounts")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	for _, want := range []string{"default", "business"} {
		if !strings.Contains(text, want) {
			t.Errorf("accounts resource missing %q: %s", want, text)
		}
	}
}

func TestConfigAccountsBuild(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	path := writeTestConfig(t, "maildir: "+dirA+"\naccounts:\n  business:\n    maildir: "+dirB+"\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := cfg.BuildAccounts()
	if err != nil {
		t.Fatalf("BuildAccounts: %v", err)
	}
	if _, ok := accounts["business"]; !ok {
		t.Errorf("accounts = %v", accounts)
	}
}
