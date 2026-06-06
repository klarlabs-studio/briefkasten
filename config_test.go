package briefkasten_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/briefkasten"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigFile(t *testing.T) {
	path := writeConfig(t, `
addr: ":9999"
backend: imap
runtime_config: true
imap:
  addr: imap.example.org:993
  username: alice
  password: secret
  mailbox: Steuern
`)
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.Backend != "imap" {
		t.Errorf("Backend = %q", cfg.Backend)
	}
	if !cfg.RuntimeConfig {
		t.Error("RuntimeConfig = false, want true")
	}
	if cfg.IMAP.Addr != "imap.example.org:993" || cfg.IMAP.Username != "alice" ||
		cfg.IMAP.Password != "secret" || cfg.IMAP.Mailbox != "Steuern" {
		t.Errorf("IMAP = %+v", cfg.IMAP)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := briefkasten.LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Addr != ":8090" {
		t.Errorf("Addr = %q, want :8090", cfg.Addr)
	}
	if cfg.Maildir != "./maildir" {
		t.Errorf("Maildir = %q, want ./maildir", cfg.Maildir)
	}
	if cfg.RuntimeConfig {
		t.Error("RuntimeConfig default = true, want false")
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := briefkasten.LoadConfig("/nonexistent/briefkasten.yaml"); err == nil {
		t.Error("want error for missing explicit config file")
	}
}

func TestLoadConfigBadYAML(t *testing.T) {
	path := writeConfig(t, "addr: [unclosed")
	if _, err := briefkasten.LoadConfig(path); err == nil {
		t.Error("want error for invalid YAML")
	}
}

func TestApplyEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
addr: ":9999"
maildir: /from/file
imap:
  addr: file.example.org:993
  username: file-user
`)
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("BRIEFKASTEN_ADDR", ":7777")
	t.Setenv("BRIEFKASTEN_MAILDIR", "/from/env")
	t.Setenv("BRIEFKASTEN_IMAP_ADDR", "env.example.org:993")
	t.Setenv("BRIEFKASTEN_IMAP_USER", "env-user")
	t.Setenv("BRIEFKASTEN_IMAP_PASSWORD", "env-pass")
	t.Setenv("BRIEFKASTEN_IMAP_MAILBOX", "EnvBox")
	t.Setenv("BRIEFKASTEN_IMAP_INSECURE", "1")
	t.Setenv("BRIEFKASTEN_RUNTIME_CONFIG", "1")
	cfg.ApplyEnv()

	if cfg.Addr != ":7777" {
		t.Errorf("Addr = %q, want env :7777", cfg.Addr)
	}
	if cfg.Maildir != "/from/env" {
		t.Errorf("Maildir = %q", cfg.Maildir)
	}
	if cfg.IMAP.Addr != "env.example.org:993" || cfg.IMAP.Username != "env-user" ||
		cfg.IMAP.Password != "env-pass" || cfg.IMAP.Mailbox != "EnvBox" || !cfg.IMAP.Insecure {
		t.Errorf("IMAP = %+v", cfg.IMAP)
	}
	if !cfg.RuntimeConfig {
		t.Error("RuntimeConfig not overridden by env")
	}
}

func TestApplyEnvKeepsFileValues(t *testing.T) {
	path := writeConfig(t, `addr: ":9999"`)
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyEnv() // no env set — file values survive
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want file :9999", cfg.Addr)
	}
}

func TestBuildMailboxMaildir(t *testing.T) {
	cfg, _ := briefkasten.LoadConfig("")
	cfg.Maildir = t.TempDir()
	mb, desc, err := cfg.BuildMailbox()
	if err != nil {
		t.Fatalf("BuildMailbox: %v", err)
	}
	if _, ok := mb.(*briefkasten.DirMailbox); !ok {
		t.Errorf("backend = %T, want *DirMailbox", mb)
	}
	if desc == "" {
		t.Error("empty backend description")
	}
}

func TestBuildMailboxIMAP(t *testing.T) {
	cfg, _ := briefkasten.LoadConfig("")
	cfg.IMAP.Addr = "imap.example.org:993" // backend inferred from imap.addr
	mb, _, err := cfg.BuildMailbox()
	if err != nil {
		t.Fatalf("BuildMailbox: %v", err)
	}
	if _, ok := mb.(*briefkasten.ResilientMailbox); !ok {
		t.Errorf("backend = %T, want resilience-wrapped IMAP", mb)
	}
}

func TestBuildMailboxExplicitBackendWins(t *testing.T) {
	cfg, _ := briefkasten.LoadConfig("")
	cfg.Backend = "maildir"
	cfg.Maildir = t.TempDir()
	cfg.IMAP.Addr = "imap.example.org:993" // present but backend says maildir
	mb, _, err := cfg.BuildMailbox()
	if err != nil {
		t.Fatalf("BuildMailbox: %v", err)
	}
	if _, ok := mb.(*briefkasten.DirMailbox); !ok {
		t.Errorf("backend = %T, want *DirMailbox", mb)
	}
}

func TestBuildMailboxUnknownBackend(t *testing.T) {
	cfg, _ := briefkasten.LoadConfig("")
	cfg.Backend = "carrier-pigeon"
	if _, _, err := cfg.BuildMailbox(); err == nil {
		t.Error("want error for unknown backend")
	}
}

func TestConfigOAuth2Parsing(t *testing.T) {
	path := writeConfig(t, `
imap:
  addr: imap.gmail.com:993
  username: alice@gmail.com
  oauth2:
    client_id: cid
    client_secret: csec
    refresh_token: rtok
    token_url: https://oauth2.googleapis.com/token
    mechanism: xoauth2
`)
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.IMAP.OAuth2
	if o == nil || o.ClientID != "cid" || o.RefreshToken != "rtok" || o.Mechanism != "xoauth2" {
		t.Errorf("oauth2 = %+v", o)
	}
}
