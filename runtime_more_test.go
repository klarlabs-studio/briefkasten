package briefkasten

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewConfigServerErrors(t *testing.T) {
	// An unknown backend fails the build.
	cfg, _ := LoadConfig("")
	cfg.Backend = "carrier-pigeon"
	if _, _, err := NewConfigServer(cfg); err == nil {
		t.Error("unknown backend accepted")
	}

	// A broken named account fails the build.
	cfg, _ = LoadConfig("")
	cfg.Maildir = newRootDir(t)
	cfg.Accounts = map[string]AccountSettings{"broken": {Backend: "carrier-pigeon"}}
	if _, _, err := NewConfigServer(cfg); err == nil {
		t.Error("broken account accepted")
	}

	// A sender that cannot be built (no From) fails the build.
	cfg, _ = LoadConfig("")
	cfg.Maildir = newRootDir(t)
	cfg.Outbox = OutboxSettings{
		Dir:        filepath.Join(t.TempDir(), "outbox"),
		DeliverDir: filepath.Join(t.TempDir(), "delivered"),
	}
	if _, _, err := NewConfigServer(cfg); err == nil {
		t.Error("outbox without From accepted")
	}

	// An outbox store that cannot be prepared fails the build.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadConfig("")
	cfg.Maildir = newRootDir(t)
	cfg.Outbox = OutboxSettings{
		Dir:        filepath.Join(blocker, "outbox"),
		From:       "me@x.y",
		DeliverDir: filepath.Join(t.TempDir(), "delivered"),
	}
	if _, _, err := NewConfigServer(cfg); err == nil {
		t.Error("outbox dir under a file accepted")
	}
}

// TestApplyOAuth2PatchFieldOverlay covers every patchable field plus the
// nil-current and same-file cases.
func TestApplyOAuth2PatchFieldOverlay(t *testing.T) {
	// No current settings: the patch becomes the settings.
	got := applyOAuth2Patch(nil, &oauth2Patch{
		ClientSecret: "csec",
		RefreshToken: "rtok",
		TokenURL:     "https://t.example/token",
		AccessToken:  "atok",
		Mechanism:    "oauthbearer",
	})
	if got.ClientSecret != "csec" || got.RefreshToken != "rtok" ||
		got.TokenURL != "https://t.example/token" || got.AccessToken != "atok" ||
		got.Mechanism != "oauthbearer" {
		t.Errorf("patch onto nil = %+v", got)
	}

	// Re-stating the SAME credentials file keeps the carried-over client
	// identity (only a NEW file clears it).
	cur := &OAuth2Settings{CredentialsFile: "/g.json", ClientID: "keep", TokenURL: "https://t"}
	got = applyOAuth2Patch(cur, &oauth2Patch{CredentialsFile: "/g.json"})
	if got.ClientID != "keep" || got.TokenURL != "https://t" || got.CredentialsFile != "/g.json" {
		t.Errorf("same-file patch = %+v, want client identity kept", got)
	}

	// A different file clears the old client identity.
	got = applyOAuth2Patch(cur, &oauth2Patch{CredentialsFile: "/other.json"})
	if got.ClientID != "" || got.TokenURL != "" || got.CredentialsFile != "/other.json" {
		t.Errorf("new-file patch = %+v, want cleared client identity", got)
	}
}

func TestConfigGetIncludesConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path,
		[]byte("maildir: "+newRootDir(t)+"\nruntime_config: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	client := newConfigClient(t, cfg)
	got := rootCallMap(t, client, "config.get", map[string]any{})
	if got["config_file"] != path {
		t.Errorf("config_file = %v, want %q", got["config_file"], path)
	}
}

// TestConfigSetPatchesEveryField exercises the full IMAP + outbox patch
// surface in one runtime reconfiguration.
func TestConfigSetPatchesEveryField(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	// Start insecure so the patch can exercise the flag in the only
	// direction runtime changes allow: towards TLS, never away from it.
	cfg.IMAP.Insecure = true
	cfg.Outbox = OutboxSettings{
		Dir:        filepath.Join(t.TempDir(), "outbox"),
		From:       "old@x.y",
		DeliverDir: filepath.Join(t.TempDir(), "old-delivered"),
		SMTP:       SMTPSettings{Insecure: true},
	}
	client := newConfigClient(t, cfg)

	newDeliver := filepath.Join(t.TempDir(), "new-delivered")
	got := rootCallMap(t, client, "config.set", map[string]any{
		"confirm": true,
		"backend": "imap",
		"imap": map[string]any{
			"addr":     "imap.example.org:993",
			"username": "alice",
			"password": "geheim",
			"mailbox":  "Steuern",
			"insecure": false,
		},
		"outbox": map[string]any{
			"from":        "new@x.y",
			"deliver_dir": newDeliver,
			"smtp": map[string]any{
				"addr":         "smtp.example.org:587",
				"username":     "smtp-user",
				"password":     "smtp-pass",
				"implicit_tls": true,
				"insecure":     false,
			},
		},
	})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	if cfg.IMAP.Addr != "imap.example.org:993" || cfg.IMAP.Username != "alice" ||
		cfg.IMAP.Password != "geheim" || cfg.IMAP.Mailbox != "Steuern" || cfg.IMAP.Insecure {
		t.Errorf("imap after set = %+v", cfg.IMAP)
	}
	if cfg.Outbox.From != "new@x.y" || cfg.Outbox.DeliverDir != newDeliver {
		t.Errorf("outbox after set = %+v", cfg.Outbox)
	}
	smtp := cfg.Outbox.SMTP
	if smtp.Addr != "smtp.example.org:587" || smtp.Username != "smtp-user" ||
		smtp.Password != "smtp-pass" || !smtp.ImplicitTLS || smtp.Insecure {
		t.Errorf("smtp after set = %+v", smtp)
	}
	if s, _ := got["sender"].(string); !strings.Contains(s, "smtp.example.org") {
		t.Errorf("sender = %v", got["sender"])
	}
}

// TestConfigSetSenderBuildErrorKeepsConfig rejects a patch whose sender
// cannot be built and leaves the running configuration untouched.
func TestConfigSetSenderBuildErrorKeepsConfig(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	cfg.Outbox = OutboxSettings{
		Dir:        filepath.Join(t.TempDir(), "outbox"),
		From:       "me@x.y",
		DeliverDir: filepath.Join(t.TempDir(), "delivered"),
	}
	client := newConfigClient(t, cfg)

	_, err := client.CallToolRaw("config.set", map[string]any{
		"outbox": map[string]any{"smtp": map[string]any{
			"addr":   "smtp.example.org:587",
			"oauth2": map[string]any{"credentials_file": "/no/such/creds.json"},
		}},
	})
	if err == nil {
		t.Fatal("unbuildable sender accepted")
	}
	if cfg.Outbox.SMTP.Addr != "" || cfg.Outbox.SMTP.OAuth2 != nil {
		t.Errorf("failed patch leaked into the live config: %+v", cfg.Outbox.SMTP)
	}
}

// TestConfigSetReportsPersistFailure applies the change in memory but
// reports that persisting it back to the config file failed.
func TestConfigSetReportsPersistFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path,
		[]byte("maildir: "+newRootDir(t)+"\nruntime_config: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	client := newConfigClient(t, cfg)

	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})

	next := subMaildir(t, cfg.Maildir, "nested")
	got := rootCallMap(t, client, "config.set", map[string]any{"maildir": next, "confirm": true})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	if got["persisted"] != false || got["persist_error"] == nil {
		t.Errorf("persist result = %v, want persisted=false with an error", got)
	}
	if cfg.Maildir != next {
		t.Errorf("live maildir = %q, want the in-memory change applied", cfg.Maildir)
	}
}
