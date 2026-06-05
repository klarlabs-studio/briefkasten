package briefkasten

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felixgeelhaar/mcp-go/testutil"
)

func TestConfigOutboxParsingAndEnv(t *testing.T) {
	path := writeTestConfig(t, `
outbox:
  dir: /var/outbox
  from: nexa@local.example
  deliver_dir: /var/delivery
  smtp:
    addr: smtp.example.org:587
    username: alice
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Outbox.Dir != "/var/outbox" || cfg.Outbox.From != "nexa@local.example" ||
		cfg.Outbox.DeliverDir != "/var/delivery" || cfg.Outbox.SMTP.Addr != "smtp.example.org:587" {
		t.Errorf("outbox = %+v", cfg.Outbox)
	}

	t.Setenv("BRIEFKASTEN_OUTBOX_DIR", "/env/outbox")
	t.Setenv("BRIEFKASTEN_SMTP_ADDR", "env.example.org:587")
	t.Setenv("BRIEFKASTEN_SMTP_PASSWORD", "env-secret")
	cfg.ApplyEnv()
	if cfg.Outbox.Dir != "/env/outbox" || cfg.Outbox.SMTP.Addr != "env.example.org:587" ||
		cfg.Outbox.SMTP.Password != "env-secret" {
		t.Errorf("env overlay = %+v", cfg.Outbox)
	}
}

func TestBuildOutboxDirSender(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Outbox.Dir = filepath.Join(t.TempDir(), "out")
	cfg.Outbox.From = "nexa@local.example"
	cfg.Outbox.DeliverDir = filepath.Join(t.TempDir(), "delivery")

	ob, desc, err := cfg.BuildOutbox()
	if err != nil {
		t.Fatalf("BuildOutbox: %v", err)
	}
	if ob == nil || !strings.Contains(desc, "dir") {
		t.Errorf("ob=%v desc=%q", ob, desc)
	}
}

func TestBuildOutboxNotConfigured(t *testing.T) {
	cfg, _ := LoadConfig("")
	ob, _, err := cfg.BuildOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if ob != nil {
		t.Errorf("ob = %v, want nil when outbox.dir empty", ob)
	}
}

func TestSendToolsAbsentWithoutOutbox(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.Maildir = t.TempDir()
	srv, ob, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ob != nil {
		t.Fatal("outbox built without configuration")
	}
	client := testutil.NewTestClient(t, srv)
	tools, err := client.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool["name"].(string), "email.send") {
			t.Errorf("send tool registered without outbox: %v", tool["name"])
		}
	}
}

func TestSendToolRoundTrip(t *testing.T) {
	delivery := t.TempDir()
	cfg, _ := LoadConfig("")
	cfg.Maildir = t.TempDir()
	cfg.Outbox.Dir = filepath.Join(t.TempDir(), "out")
	cfg.Outbox.From = "nexa@local.example"
	cfg.Outbox.DeliverDir = delivery

	srv, ob, err := NewConfigServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ob == nil {
		t.Fatal("outbox not built")
	}
	client := testutil.NewTestClient(t, srv)

	sent := callMap(t, client, "email.send", map[string]any{
		"to":      []string{"steuerberater@kanzlei.example"},
		"subject": "Belege",
		"body":    "Anbei.",
	})
	id, _ := sent["id"].(string)
	if id == "" || sent["state"] != "queued" {
		t.Fatalf("email.send = %v", sent)
	}

	status := callMap(t, client, "email.send_status", map[string]any{"id": id})
	if status["state"] != "queued" {
		t.Errorf("status = %v", status)
	}

	if _, err := ob.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	status = callMap(t, client, "email.send_status", map[string]any{"id": id})
	if status["state"] != "sent" {
		t.Errorf("status after process = %v", status)
	}

	files, _ := filepath.Glob(filepath.Join(delivery, "new", "*.eml"))
	if len(files) != 1 {
		t.Fatalf("delivered = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if !strings.Contains(string(raw), "To: steuerberater@kanzlei.example") {
		t.Errorf("delivered:\n%s", raw)
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
