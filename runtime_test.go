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

// TestApplyOAuth2Patch builds a fresh settings struct, overlaying the patch on
// current values.
func TestApplyOAuth2Patch(t *testing.T) {
	cur := &OAuth2Settings{ClientID: "old", RefreshToken: "rtok"}
	got := applyOAuth2Patch(cur, &oauth2Patch{CredentialsFile: "/g.json", ClientID: "new"})
	if got == cur {
		t.Error("patch must return a fresh struct (dropping the cached token source)")
	}
	if got.ClientID != "new" || got.RefreshToken != "rtok" || got.CredentialsFile != "/g.json" {
		t.Errorf("merged = %+v", got)
	}
}

// TestConfigSetReconfiguresOAuth2 patches the IMAP OAuth2 credentials file at
// runtime; the new settings are read and applied without a restart.
func TestConfigSetReconfiguresOAuth2(t *testing.T) {
	// A downloaded OAuth client secret (low-entropy, no real secret).
	credPath := filepath.Join(t.TempDir(), "google.json")
	clientJSON := `{"web":{"client_id":"cid.apps.googleusercontent.com","client_secret":"x","token_uri":"https://oauth2.googleapis.com/token","auth_uri":"https://accounts.google.com/o/oauth2/auth","redirect_uris":["http://127.0.0.1/cb"]}}`
	if err := os.WriteFile(credPath, []byte(clientJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	client := newConfigClient(t, cfg)

	got := rootCallMap(t, client, "config.set", map[string]any{
		"backend": "imap",
		"imap": map[string]any{
			"addr":     "imap.gmail.com:993",
			"username": "you@gmail.com",
			"oauth2":   map[string]any{"credentials_file": credPath, "refresh_token": "rtok"},
		},
	})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	// The live config carries the new credentials file, hydrated from disk.
	if cfg.IMAP.OAuth2 == nil || cfg.IMAP.OAuth2.CredentialsFile != credPath {
		t.Fatalf("oauth2 not reconfigured: %+v", cfg.IMAP.OAuth2)
	}
	if cfg.IMAP.OAuth2.ClientID != "cid.apps.googleusercontent.com" {
		t.Errorf("client_id not hydrated from the credentials file: %q", cfg.IMAP.OAuth2.ClientID)
	}
}

// TestConfigSetSwapsSender reconfigures the outbound SMTP sender at runtime and
// reports the new sender in the result.
func TestConfigSetSwapsSender(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	cfg.Outbox = OutboxSettings{
		Dir:  filepath.Join(t.TempDir(), "outbox"),
		From: "ops@example.org",
		SMTP: SMTPSettings{Addr: "smtp.example.org:587"},
	}
	client := newConfigClient(t, cfg)

	got := rootCallMap(t, client, "config.set", map[string]any{
		"outbox": map[string]any{"smtp": map[string]any{"addr": "smtp2.example.org:587"}},
	})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	if s, _ := got["sender"].(string); !strings.Contains(s, "smtp2.example.org") {
		t.Errorf("sender not swapped: %v", got["sender"])
	}
	if cfg.Outbox.SMTP.Addr != "smtp2.example.org:587" {
		t.Errorf("live smtp addr = %q", cfg.Outbox.SMTP.Addr)
	}
}

// TestConfigSetReconfiguresToDifferentCredentials guards the stale-credentials
// regression: pointing config.set at a NEW credentials file must adopt the new
// client identity, not keep the previous file's client_id.
func TestConfigSetReconfiguresToDifferentCredentials(t *testing.T) {
	dir := t.TempDir()
	writeClient := func(name, clientID string) string {
		p := filepath.Join(dir, name)
		j := `{"web":{"client_id":"` + clientID + `","client_secret":"x","token_uri":"https://oauth2.googleapis.com/token","auth_uri":"https://accounts.google.com/o/oauth2/auth","redirect_uris":["http://127.0.0.1/cb"]}}`
		if err := os.WriteFile(p, []byte(j), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := writeClient("a.json", "client-a.apps.googleusercontent.com")
	second := writeClient("b.json", "client-b.apps.googleusercontent.com")

	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	client := newConfigClient(t, cfg)

	base := map[string]any{"addr": "imap.gmail.com:993", "username": "you@gmail.com"}
	set := func(file string) {
		imap := map[string]any{}
		for k, v := range base {
			imap[k] = v
		}
		imap["oauth2"] = map[string]any{"credentials_file": file, "refresh_token": "rtok"}
		if got := rootCallMap(t, client, "config.set", map[string]any{"backend": "imap", "imap": imap}); got["ok"] != true {
			t.Fatalf("config.set(%s) = %v", file, got)
		}
	}

	set(first)
	if cfg.IMAP.OAuth2.ClientID != "client-a.apps.googleusercontent.com" {
		t.Fatalf("first client_id = %q", cfg.IMAP.OAuth2.ClientID)
	}
	set(second)
	if cfg.IMAP.OAuth2.ClientID != "client-b.apps.googleusercontent.com" {
		t.Errorf("after reconfig, client_id = %q, want client-b (stale-credentials regression)", cfg.IMAP.OAuth2.ClientID)
	}
}
