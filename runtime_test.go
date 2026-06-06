package briefkasten

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/testutil"
)

func newRootDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func dropRoot(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "new", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rootCallMap(t *testing.T, client *testutil.TestClient, name string, args map[string]any) map[string]any {
	t.Helper()
	resp, err := client.CallToolRaw(name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	out, ok := content[0]["text"].(map[string]any)
	if !ok {
		t.Fatalf("%s: unexpected payload %T", name, content[0]["text"])
	}
	return out
}

func newConfigClient(t *testing.T, cfg *Config) *testutil.TestClient {
	t.Helper()
	srv, _, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatalf("NewConfigServer: %v", err)
	}
	return testutil.NewTestClient(t, srv)
}

func TestConfigToolsGatedOffByDefault(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Maildir = newRootDir(t)
	client := newConfigClient(t, cfg)
	tools, err := client.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool["name"].(string), "config.") {
			t.Errorf("config tool registered without runtime_config: %v", tool["name"])
		}
	}
}

func TestConfigGetRedactsPassword(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Backend = "imap"
	cfg.IMAP = IMAPSettings{Addr: "imap.example.org:993", Username: "alice", Password: "geheim"}
	client := newConfigClient(t, cfg)

	got := rootCallMap(t, client, "config.get", map[string]any{})
	if got["backend"] != "imap" {
		t.Errorf("backend = %v", got["backend"])
	}
	imapInfo := got["imap"].(map[string]any)
	if pw, ok := imapInfo["password"]; ok && strings.Contains(pw.(string), "geheim") {
		t.Errorf("password leaked: %v", pw)
	}
}

func TestConfigSetSwapsBackendAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	oldDir := newRootDir(t)
	if err := os.WriteFile(path, []byte("maildir: "+oldDir+"\nruntime_config: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	client := newConfigClient(t, cfg)

	next := newRootDir(t)
	dropRoot(t, next, "new.eml", "From: n@n\r\n\r\nn")

	got := rootCallMap(t, client, "config.set", map[string]any{"backend": "maildir", "maildir": next})
	if got["ok"] != true || got["persisted"] != true {
		t.Fatalf("config.set = %v", got)
	}
	listed := rootCallMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "new.eml" {
		t.Errorf("ids after set = %v", ids)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), next) {
		t.Errorf("config file not persisted:\n%s", raw)
	}
}

func TestConfigSetInvalidKeepsOldBackend(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	dir := newRootDir(t)
	cfg.Maildir = dir
	client := newConfigClient(t, cfg)
	dropRoot(t, dir, "keep.eml", "From: k@k\r\n\r\nk")

	if _, err := client.CallToolRaw("config.set", map[string]any{"backend": "carrier-pigeon"}); err == nil {
		t.Fatal("bad backend accepted")
	}
	listed := rootCallMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "keep.eml" {
		t.Errorf("ids after failed set = %v", ids)
	}
	_ = context.Background()
}
