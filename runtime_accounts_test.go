package briefkasten

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/mcp/testutil"
)

// accountName is the account these tests configure beside the default
// mailbox — the shape where a runtime reconfiguration used to stop short.
const accountName = "work"

// accountConfig serves one named maildir account beside the default mailbox.
func accountConfig(t *testing.T) (*Config, string) {
	t.Helper()
	acct := newRootDir(t)
	dropRoot(t, acct, "acct.eml", "From: a@a\r\n\r\naccount")

	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	dropRoot(t, cfg.Maildir, "default.eml", "From: d@d\r\n\r\ndefault")
	cfg.Accounts = map[string]AccountSettings{
		accountName: {Backend: "maildir", Maildir: acct},
	}
	return cfg, acct
}

// accountIDs lists one account's unread ids through the tool surface.
func accountIDs(t *testing.T, client *testutil.TestClient, account string) []string {
	t.Helper()
	args := map[string]any{}
	if account != "" {
		args["account"] = account
	}
	return rootCallMap(t, client, "email.list_unread", args)["ids"].([]string)
}

// accountEntry finds one account's config.get entry.
func accountEntry(t *testing.T, got map[string]any, name string) map[string]any {
	t.Helper()
	entries, ok := got["accounts"].([]map[string]any)
	if !ok {
		t.Fatalf("accounts = %T, want []map[string]any", got["accounts"])
	}
	for _, e := range entries {
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("account %q missing from config.get: %v", name, entries)
	return nil
}

// The defect: a committed configuration change applied to the default
// mailbox only, while every account= request went on using the mailbox
// materialised at startup.
func TestConfigSetRebuildsNamedAccounts(t *testing.T) {
	cfg, _ := accountConfig(t)
	client := newConfigClient(t, cfg)

	if ids := accountIDs(t, client, "work"); len(ids) != 1 || ids[0] != "acct.eml" {
		t.Fatalf("account ids before set = %v", ids)
	}

	// The live configuration is what a commit materialises, so moving the
	// account there is what config.set must now pick up.
	moved := newRootDir(t)
	dropRoot(t, moved, "moved.eml", "From: m@m\r\n\r\nmoved")
	cfg.Accounts["work"] = AccountSettings{Backend: "maildir", Maildir: moved}

	got := rootCallMap(t, client, "config.set", map[string]any{"confirm": true})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	names, _ := got["accounts_rebuilt"].([]string)
	if len(names) != 1 || names[0] != "work" {
		t.Errorf("accounts_rebuilt = %v, want [work]", got["accounts_rebuilt"])
	}

	if ids := accountIDs(t, client, "work"); len(ids) != 1 || ids[0] != "moved.eml" {
		t.Errorf("account ids after set = %v, want [moved.eml] — the account kept its startup mailbox", ids)
	}
}

// A profile switch commits a whole configuration too, so it rebuilds the
// accounts along with the default mailbox.
func TestProfileSwitchRebuildsNamedAccounts(t *testing.T) {
	cfg, _ := accountConfig(t)
	personal := newRootDir(t)
	dropRoot(t, personal, "p.eml", "From: p@p\r\n\r\npersonal")
	cfg.Profiles = map[string]ProfileSettings{
		"personal": {Backend: "maildir", Maildir: personal},
	}
	client := newConfigClient(t, cfg)

	moved := newRootDir(t)
	dropRoot(t, moved, "moved.eml", "From: m@m\r\n\r\nmoved")
	cfg.Accounts["work"] = AccountSettings{Backend: "maildir", Maildir: moved}

	got := rootCallMap(t, client, "config.set", map[string]any{"confirm": true, "profile": "personal"})
	if got["ok"] != true {
		t.Fatalf("profile switch = %v", got)
	}
	if ids := accountIDs(t, client, "work"); len(ids) != 1 || ids[0] != "moved.eml" {
		t.Errorf("account ids after profile switch = %v, want [moved.eml]", ids)
	}
}

// All-or-nothing: an account that cannot be rebuilt aborts the commit
// before anything is swapped, so no request sees a configuration half of
// which the operator wrote and half of which they replaced.
func TestConfigSetAccountBuildFailureKeepsEverything(t *testing.T) {
	cfg, acct := accountConfig(t)
	cfg.Outbox = OutboxSettings{
		Dir:  filepath.Join(t.TempDir(), "outbox"),
		From: "ops@example.org",
		SMTP: SMTPSettings{Addr: "smtp.old.example:587"},
	}
	client := newConfigClient(t, cfg)
	before := cfg.Maildir

	// The account's maildir becomes a regular file: the directory layout
	// can no longer be prepared, so the rebuild fails — while the default
	// mailbox and the new sender both build fine.
	if err := os.RemoveAll(acct); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acct, []byte("not a maildir"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := client.CallToolRaw("config.set", map[string]any{
		"confirm": true,
		"maildir": subMaildir(t, before, "nested"),
		"outbox": map[string]any{"smtp": map[string]any{
			"addr": "smtp.new.example:587", "clear_credentials": true,
		}},
	})
	if err == nil {
		t.Fatal("unbuildable account accepted")
	}
	if !strings.Contains(err.Error(), `account "work"`) {
		t.Errorf("error = %v, want it to name the failing account", err)
	}

	// Nothing moved: not the default mailbox, not the sender.
	if ids := accountIDs(t, client, ""); len(ids) != 1 || ids[0] != "default.eml" {
		t.Errorf("default ids after failed set = %v, want [default.eml]", ids)
	}
	if cfg.Maildir != before {
		t.Errorf("live maildir = %q, want %q", cfg.Maildir, before)
	}
	if cfg.Outbox.SMTP.Addr != "smtp.old.example:587" {
		t.Errorf("sender was swapped by a failed commit: %q", cfg.Outbox.SMTP.Addr)
	}
	got := rootCallMap(t, client, "config.get", map[string]any{})
	if got["maildir"] != before {
		t.Errorf("config.get maildir = %v, want %q", got["maildir"], before)
	}
}

// Rebuilding must not become a way to aim an account at settings the
// operator never wrote for it. Each account is built from its own block,
// so moving the default endpoint — credentials and all — leaves every
// account exactly where it was.
func TestConfigSetLeaksNothingIntoAccounts(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Backend = "imap"
	cfg.IMAP = IMAPSettings{Addr: "imap.work.example:993", Username: "alice", Password: "geheim"}
	cfg.Accounts = map[string]AccountSettings{
		"reports": {Backend: "imap", IMAP: IMAPSettings{
			Addr: "imap.reports.example:993", Username: "reports-bot",
		}},
	}
	client := newConfigClient(t, cfg)

	set := rootCallMap(t, client, "config.set", map[string]any{
		"confirm": true,
		"imap": map[string]any{
			"addr": "imap.other.example:993", "username": "bob", "password": "neu",
		},
	})
	if set["ok"] != true {
		t.Fatalf("config.set = %v", set)
	}

	got := rootCallMap(t, client, "config.get", map[string]any{})
	entry := accountEntry(t, got, "reports")
	if entry["addr"] != "imap.reports.example:993" {
		t.Errorf("account addr = %v, want its own endpoint", entry["addr"])
	}
	if entry["username"] != "reports-bot" {
		t.Errorf("account username = %v, want its own", entry["username"])
	}
	// TLS is one-way for accounts too: a patch cannot reach the block, so
	// the operator's choice stands.
	if entry["insecure"] != false {
		t.Errorf("account insecure = %v, want false", entry["insecure"])
	}
	for _, secret := range []string{"geheim", "neu"} {
		if blob := fmt.Sprint(got, set); strings.Contains(blob, secret) {
			t.Errorf("credential %q reached the tool response", secret)
		}
	}
}

// The account set itself cannot change at runtime — the service's routing
// table is fixed when the server is built. A name the configuration drops
// is revoked rather than left serving the connection it had, and the gap
// is reported instead of left to be inferred.
func TestConfigSetRevokesAccountDroppedFromConfig(t *testing.T) {
	cfg, _ := accountConfig(t)
	client := newConfigClient(t, cfg)

	delete(cfg.Accounts, "work")
	got := rootCallMap(t, client, "config.set", map[string]any{"confirm": true})
	if got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}
	if d, _ := got["accounts_diverged"].([]string); len(d) != 1 || d[0] != "work" {
		t.Errorf("accounts_diverged = %v, want [work]", got["accounts_diverged"])
	}

	_, err := client.CallToolRaw("email.list_unread", map[string]any{"account": "work"})
	if err == nil {
		t.Fatal("a removed account still served its startup mailbox")
	}
	if !strings.Contains(err.Error(), "no longer configured") {
		t.Errorf("error = %v, want the revocation refusal", err)
	}
	// Every listing path says the same thing, not just the unread one.
	_, err = client.CallToolRaw("email.list", map[string]any{"account": "work", "scope": "all"})
	if err == nil || !strings.Contains(err.Error(), "no longer configured") {
		t.Errorf("scoped list error = %v, want the revocation refusal", err)
	}

	info := rootCallMap(t, client, "config.get", map[string]any{})
	if d, _ := info["accounts_diverged"].([]string); len(d) != 1 || d[0] != "work" {
		t.Errorf("config.get accounts_diverged = %v, want [work]", info["accounts_diverged"])
	}
}

// The other direction: a name the configuration gains has no slot to be
// served from, so config.get says so rather than implying it is live.
func TestConfigGetReportsUnservableAccount(t *testing.T) {
	cfg, _ := accountConfig(t)
	client := newConfigClient(t, cfg)

	cfg.Accounts["later"] = AccountSettings{Backend: "maildir", Maildir: newRootDir(t)}
	if got := rootCallMap(t, client, "config.set", map[string]any{"confirm": true}); got["ok"] != true {
		t.Fatalf("config.set = %v", got)
	}

	got := rootCallMap(t, client, "config.get", map[string]any{})
	if entry := accountEntry(t, got, "later"); entry["served"] != false {
		t.Errorf("account entry = %v, want served=false", entry)
	}
	if entry := accountEntry(t, got, "work"); entry["served"] != nil {
		t.Errorf("served account marked unserved: %v", entry)
	}
	if d, _ := got["accounts_diverged"].([]string); len(d) != 1 || d[0] != "later" {
		t.Errorf("accounts_diverged = %v, want [later]", got["accounts_diverged"])
	}
	if _, err := client.CallToolRaw("email.list_unread", map[string]any{"account": "later"}); err == nil {
		t.Error("unroutable account accepted")
	}
}

// config.get describes every mailbox the server serves; without the
// accounts there is no way to tell what a routed request would reach.
func TestConfigGetListsAccounts(t *testing.T) {
	cfg, acct := accountConfig(t)
	client := newConfigClient(t, cfg)

	entry := accountEntry(t, rootCallMap(t, client, "config.get", map[string]any{}), "work")
	if entry["backend"] != "maildir" || entry["maildir"] != acct {
		t.Errorf("account entry = %v, want the maildir it was declared with", entry)
	}
	if entry["served"] != nil {
		t.Errorf("account entry = %v, want no served marker when it is live", entry)
	}
}
