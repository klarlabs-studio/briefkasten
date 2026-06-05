package briefkasten

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

func TestSwitchableSwap(t *testing.T) {
	mbA, rootA := newDir(t)
	drop(t, rootA, "a.eml", "From: a@a\r\n\r\na")
	mbB, rootB := newDir(t)
	drop(t, rootB, "b.eml", "From: b@b\r\n\r\nb")

	sw := NewSwitchable(mbA)
	ids, err := sw.ListUnread()
	if err != nil || len(ids) != 1 || ids[0] != "a.eml" {
		t.Fatalf("before swap: ids=%v err=%v", ids, err)
	}

	sw.Swap(mbB)
	ids, err = sw.ListUnread()
	if err != nil || len(ids) != 1 || ids[0] != "b.eml" {
		t.Fatalf("after swap: ids=%v err=%v", ids, err)
	}
}

func newConfigServer(t *testing.T, cfg *Config) *testutil.TestClient {
	t.Helper()
	srv, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatalf("NewConfigServer: %v", err)
	}
	return testutil.NewTestClient(t, srv)
}

func TestConfigToolsGatedOffByDefault(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Maildir = t.TempDir()
	client := newConfigServer(t, cfg)

	tools, err := client.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		name := tool["name"].(string)
		if strings.HasPrefix(name, "config.") {
			t.Errorf("config tool %q registered without runtime_config", name)
		}
	}
}

func TestConfigGetRedactsPassword(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Backend = "imap"
	cfg.IMAP = IMAPSettings{Addr: "imap.example.org:993", Username: "alice", Password: "geheim"}
	client := newConfigServer(t, cfg)

	got := callMap(t, client, "config.get", map[string]any{})
	if got["backend"] != "imap" {
		t.Errorf("backend = %v", got["backend"])
	}
	imap := got["imap"].(map[string]any)
	if imap["username"] != "alice" {
		t.Errorf("username = %v", imap["username"])
	}
	if pw, ok := imap["password"]; ok && strings.Contains(pw.(string), "geheim") {
		t.Errorf("password leaked: %v", pw)
	}
}

func TestConfigSetSwapsBackendAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	oldDir := t.TempDir()
	if err := os.WriteFile(path, []byte("maildir: "+oldDir+"\nruntime_config: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	client := newConfigServer(t, cfg)

	newDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(newDir, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	drop(t, newDir, "new.eml", "From: n@n\r\n\r\nn")

	got := callMap(t, client, "config.set", map[string]any{
		"backend": "maildir",
		"maildir": newDir,
	})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	if got["persisted"] != true {
		t.Errorf("persisted = %v, want true", got["persisted"])
	}

	// Mailbox now serves the new directory.
	listed := callMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "new.eml" {
		t.Errorf("ids after set = %v", ids)
	}

	// Change survived to disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), newDir) {
		t.Errorf("config file not persisted:\n%s", raw)
	}

	// Password never written redacted — file holds real values only.
	if strings.Contains(string(raw), "REDACTED") {
		t.Errorf("redaction marker leaked into config file:\n%s", raw)
	}
}

func TestConfigSetInvalidKeepsOldBackend(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	dir := t.TempDir()
	cfg.Maildir = dir
	client := newConfigServer(t, cfg)
	drop(t, dir, "keep.eml", "From: k@k\r\n\r\nk")

	resp, err := client.CallToolRaw("config.set", map[string]any{"backend": "carrier-pigeon"})
	if err == nil && resp.Error == nil {
		result := resp.Result.(map[string]any)
		if isErr, _ := result["isError"].(bool); !isErr {
			t.Fatalf("config.set with bad backend succeeded: %v", result)
		}
	}

	// Old backend still in service.
	listed := callMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "keep.eml" {
		t.Errorf("ids after failed set = %v", ids)
	}
}

func TestConfigSetMergesPartialIMAPUpdate(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Backend = "imap"
	cfg.IMAP = IMAPSettings{Addr: "imap.example.org:993", Username: "alice", Password: "geheim"}
	client := newConfigServer(t, cfg)

	// Update mailbox only — credentials must survive.
	got := callMap(t, client, "config.set", map[string]any{
		"imap": map[string]any{"mailbox": "Steuern"},
	})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	if cfg.IMAP.Username != "alice" || cfg.IMAP.Password != "geheim" {
		t.Errorf("credentials lost on partial update: %+v", cfg.IMAP)
	}
	if cfg.IMAP.Mailbox != "Steuern" {
		t.Errorf("mailbox = %q, want Steuern", cfg.IMAP.Mailbox)
	}
}
