package briefkasten_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten"
)

// writeFilePath drops a plain file so directory-creating constructors fail.
func writeFilePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildAccounts(t *testing.T) {
	cfg, _ := briefkasten.LoadConfig("")
	cfg.Accounts = map[string]briefkasten.AccountSettings{
		"business": {Backend: "maildir", Maildir: t.TempDir()},
	}
	accounts, err := cfg.BuildAccounts()
	if err != nil {
		t.Fatalf("BuildAccounts: %v", err)
	}
	if _, ok := accounts["business"].(*briefkasten.DirMailbox); !ok {
		t.Errorf("business account = %T, want *DirMailbox", accounts["business"])
	}

	// A broken account fails the whole build, naming the account.
	cfg.Accounts["broken"] = briefkasten.AccountSettings{Backend: "carrier-pigeon"}
	if _, err := cfg.BuildAccounts(); err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Errorf("broken account err = %v, want the account named", err)
	}
}

func TestSaveErrors(t *testing.T) {
	// A config without a file cannot be saved.
	cfg, _ := briefkasten.LoadConfig("")
	if err := cfg.Save(); err == nil {
		t.Error("Save without a config file accepted")
	}

	// A write failure (read-only file) surfaces.
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path, []byte("addr: \":9999\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := cfg.Save(); err == nil {
		t.Error("Save onto a read-only file accepted")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path, []byte("addr: \":9999\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Maildir = "/changed/box"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Maildir != "/changed/box" || reloaded.Addr != ":9999" {
		t.Errorf("reloaded = %+v", reloaded)
	}
}

func TestApplyEnvSMTPAndOutbox(t *testing.T) {
	cfg := &briefkasten.Config{}
	t.Setenv("BRIEFKASTEN_OUTBOX_DIR", "/env/outbox")
	t.Setenv("BRIEFKASTEN_OUTBOX_FROM", "env@x.y")
	t.Setenv("BRIEFKASTEN_OUTBOX_DELIVER_DIR", "/env/delivered")
	t.Setenv("BRIEFKASTEN_SMTP_ADDR", "smtp.env.example:587")
	t.Setenv("BRIEFKASTEN_SMTP_USER", "env-user")
	t.Setenv("BRIEFKASTEN_SMTP_PASSWORD", "env-pass")
	t.Setenv("BRIEFKASTEN_SMTP_INSECURE", "true")
	cfg.ApplyEnv()
	if cfg.Outbox.Dir != "/env/outbox" || cfg.Outbox.From != "env@x.y" ||
		cfg.Outbox.DeliverDir != "/env/delivered" {
		t.Errorf("outbox = %+v", cfg.Outbox)
	}
	if cfg.Outbox.SMTP.Addr != "smtp.env.example:587" || cfg.Outbox.SMTP.Username != "env-user" ||
		cfg.Outbox.SMTP.Password != "env-pass" || !cfg.Outbox.SMTP.Insecure {
		t.Errorf("smtp = %+v", cfg.Outbox.SMTP)
	}
}

func TestBuildMailboxErrors(t *testing.T) {
	// The maildir backend cannot prepare its directories under a file.
	cfg, _ := briefkasten.LoadConfig("")
	cfg.Maildir = filepath.Join(writeFilePath(t), "sub")
	if _, _, err := cfg.BuildMailbox(); err == nil {
		t.Error("maildir under a file accepted")
	}

	// The imap backend requires an address.
	cfg, _ = briefkasten.LoadConfig("")
	cfg.Backend = "imap"
	if _, _, err := cfg.BuildMailbox(); err == nil {
		t.Error("imap backend without addr accepted")
	}

	// Bad OAuth2 credentials fail the build before any dial.
	cfg, _ = briefkasten.LoadConfig("")
	cfg.Backend = "imap"
	cfg.IMAP = briefkasten.IMAPSettings{
		Addr:     "imap.example.org:993",
		Username: "alice@example.org",
		OAuth2:   &briefkasten.OAuth2Settings{CredentialsFile: "/no/such/creds.json"},
	}
	if _, _, err := cfg.BuildMailbox(); err == nil || !strings.Contains(err.Error(), "imap oauth2") {
		t.Errorf("bad imap oauth2 err = %v", err)
	}
}

func TestBuildWatcherUnknownBackend(t *testing.T) {
	cfg := &briefkasten.Config{Backend: "carrier-pigeon"}
	if w := cfg.BuildWatcher(); w != nil {
		t.Errorf("unknown backend watcher = %T, want nil", w)
	}
}

func TestBuildOutbox(t *testing.T) {
	// No outbox configured: sending is simply off.
	cfg, _ := briefkasten.LoadConfig("")
	ob, desc, err := cfg.BuildOutbox()
	if ob != nil || desc != "" || err != nil {
		t.Errorf("unconfigured outbox = %v, %q, %v; want nil, \"\", nil", ob, desc, err)
	}

	// Dir sender when no SMTP address is set.
	root := t.TempDir()
	cfg.Outbox = briefkasten.OutboxSettings{
		Dir:        filepath.Join(root, "outbox"),
		From:       "me@x.y",
		DeliverDir: filepath.Join(root, "delivered"),
	}
	ob, desc, err = cfg.BuildOutbox()
	if err != nil || ob == nil || !strings.HasPrefix(desc, "dir ") {
		t.Fatalf("dir outbox = %v, %q, %v", ob, desc, err)
	}

	// SMTP wins over the dir sender when both are configured.
	cfg.Outbox.SMTP = briefkasten.SMTPSettings{Addr: "smtp.example.org:587"}
	ob, desc, err = cfg.BuildOutbox()
	if err != nil || ob == nil || !strings.HasPrefix(desc, "smtp ") {
		t.Fatalf("smtp outbox = %v, %q, %v", ob, desc, err)
	}
}

func TestBuildOutboxErrors(t *testing.T) {
	root := t.TempDir()

	// The dir sender requires a From address.
	cfg, _ := briefkasten.LoadConfig("")
	cfg.Outbox = briefkasten.OutboxSettings{
		Dir:        filepath.Join(root, "outbox"),
		DeliverDir: filepath.Join(root, "delivered"),
	}
	if _, _, err := cfg.BuildOutbox(); err == nil {
		t.Error("dir sender without From accepted")
	}

	// So does the SMTP sender.
	cfg.Outbox.SMTP = briefkasten.SMTPSettings{Addr: "smtp.example.org:587"}
	if _, _, err := cfg.BuildOutbox(); err == nil {
		t.Error("smtp sender without From accepted")
	}

	// Bad SMTP OAuth2 credentials fail the build.
	cfg.Outbox.From = "me@x.y"
	cfg.Outbox.SMTP.OAuth2 = &briefkasten.OAuth2Settings{CredentialsFile: "/no/such/creds.json"}
	if _, _, err := cfg.BuildOutbox(); err == nil || !strings.Contains(err.Error(), "smtp oauth2") {
		t.Errorf("bad smtp oauth2 err = %v", err)
	}

	// An outbox dir that cannot be created fails the build.
	cfg.Outbox = briefkasten.OutboxSettings{
		Dir:        filepath.Join(writeFilePath(t), "outbox"),
		From:       "me@x.y",
		DeliverDir: filepath.Join(root, "delivered"),
	}
	if _, _, err := cfg.BuildOutbox(); err == nil {
		t.Error("outbox dir under a file accepted")
	}
}

func TestRootFacadeConstructors(t *testing.T) {
	// NewService wires the same use-case layer the server uses.
	mb, err := briefkasten.NewDirMailbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := briefkasten.NewService(mb, nil)
	ids, err := svc.ListUnread(t.Context(), "", "")
	if err != nil || len(ids) != 0 {
		t.Errorf("fresh service ListUnread = %v, %v", ids, err)
	}

	// NewDirSender validates the From address.
	if _, err := briefkasten.NewDirSender(t.TempDir(), ""); err == nil {
		t.Error("dir sender without From accepted")
	}
	sender, err := briefkasten.NewDirSender(t.TempDir(), "me@x.y")
	if err != nil || sender == nil {
		t.Fatalf("NewDirSender = %v, %v", sender, err)
	}

	// NewOutbox refuses a store root it cannot prepare.
	if _, err := briefkasten.NewOutbox(filepath.Join(writeFilePath(t), "out"), sender); err == nil {
		t.Error("outbox under a file accepted")
	}

	// NewOutbox surfaces a recovery failure: a message duplicated across
	// queued/ and sending/ (an unclean shutdown) whose stale record cannot
	// be repaired must not be silently skipped.
	outboxRoot := t.TempDir()
	record := []byte(`{"id":"x","state":"sending","to":["a@b.c"],"subject":"s","body":"b"}`)
	queued := filepath.Join(outboxRoot, "queued")
	for _, state := range []string{"queued", "sending"} {
		dir := filepath.Join(outboxRoot, state)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.json"), record, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(queued, 0o500); err != nil { // repair cannot delete the stale record
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(queued, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := briefkasten.NewOutbox(outboxRoot, sender); err == nil ||
		!strings.Contains(err.Error(), "recover") {
		t.Errorf("unrepairable duplicate err = %v, want recover error", err)
	}
}
