package briefkasten

import (
	"path/filepath"
	"strings"
	"testing"
)

// profileConfig declares two maildir profiles in sibling directories —
// the shape the confinement rule refuses for field-level patches, and
// exactly what profiles exist to make safe.
func profileConfig(t *testing.T) (*Config, string, string) {
	t.Helper()
	work, personal := newRootDir(t), newRootDir(t)
	dropRoot(t, work, "w.eml", "From: w@w\r\n\r\nwork")
	dropRoot(t, personal, "p.eml", "From: p@p\r\n\r\npersonal")

	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = work
	cfg.Profiles = map[string]ProfileSettings{
		"work":     {Backend: "maildir", Maildir: work},
		"personal": {Backend: "maildir", Maildir: personal},
	}
	return cfg, work, personal
}

// The point of the feature: switching between operator-declared
// mailboxes, including ones a field-level maildir patch would refuse
// for being outside the startup subtree.
func TestProfileSwitchReachesSiblingMaildir(t *testing.T) {
	cfg, _, personal := profileConfig(t)
	client := newConfigClient(t, cfg)

	// The same destination via a field-level patch is refused...
	if _, err := client.CallToolRaw("config.set", map[string]any{
		"confirm": true, "maildir": personal,
	}); err == nil {
		t.Fatal("field-level patch reached a sibling maildir — confinement is gone")
	}

	// ...but the operator declared it as a profile, so this works.
	got := rootCallMap(t, client, "config.set", map[string]any{
		"confirm": true, "profile": "personal",
	})
	if got["ok"] != true || got["profile"] != "personal" {
		t.Fatalf("profile switch = %v", got)
	}
	if cfg.Maildir != personal {
		t.Errorf("live maildir = %q, want %q", cfg.Maildir, personal)
	}

	// And the mailbox really moved.
	listed := rootCallMap(t, client, "email.list_unread", map[string]any{})
	ids := listed["ids"].([]string)
	if len(ids) != 1 || ids[0] != "p.eml" {
		t.Errorf("ids after switch = %v, want [p.eml]", ids)
	}
}

// A profile replaces the mailbox settings wholesale. Credentials from
// the previous profile must not linger, or switching would be another
// way to smuggle secrets to a different endpoint.
func TestProfileSwitchInheritsNoCredentials(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Backend = "imap"
	cfg.IMAP = IMAPSettings{Addr: "imap.work.example:993", Username: "alice", Password: "geheim"}
	cfg.Profiles = map[string]ProfileSettings{
		// Declared with no credentials at all.
		"open": {Backend: "imap", IMAP: IMAPSettings{Addr: "imap.open.example:993"}},
	}
	client := newConfigClient(t, cfg)

	got := rootCallMap(t, client, "config.set", map[string]any{
		"confirm": true, "profile": "open",
	})
	if got["ok"] != true {
		t.Fatalf("profile switch = %v", got)
	}
	if cfg.IMAP.Username != "" || cfg.IMAP.Password != "" {
		t.Errorf("credentials leaked into the new profile: %+v", cfg.IMAP)
	}
	if cfg.IMAP.Addr != "imap.open.example:993" {
		t.Errorf("addr = %q, want the profile's", cfg.IMAP.Addr)
	}
}

func TestProfileUnknownNameIsRejected(t *testing.T) {
	cfg, work, _ := profileConfig(t)
	client := newConfigClient(t, cfg)

	_, err := client.CallToolRaw("config.set", map[string]any{
		"confirm": true, "profile": "attacker",
	})
	if err == nil {
		t.Fatal("unknown profile accepted")
	}
	// The error must list the real options, not echo the bad name alone.
	if !strings.Contains(err.Error(), "work") || !strings.Contains(err.Error(), "personal") {
		t.Errorf("error = %v, want it to list the declared profiles", err)
	}
	if cfg.Maildir != work {
		t.Errorf("live config changed on a rejected switch: %q", cfg.Maildir)
	}
}

func TestProfileRequiresConfirmation(t *testing.T) {
	cfg, _, _ := profileConfig(t)
	client := newConfigClient(t, cfg)

	_, err := client.CallToolRaw("config.set", map[string]any{"profile": "personal"})
	if err == nil {
		t.Fatal("profile switch ran without confirmation")
	}
	if !strings.Contains(err.Error(), "confirmation required") {
		t.Errorf("error = %v, want the confirmation refusal", err)
	}
}

// Mixing the two forms would silently drop one; reject it loudly.
func TestProfileCannotBeCombinedWithFieldPatches(t *testing.T) {
	cfg, _, _ := profileConfig(t)
	client := newConfigClient(t, cfg)

	_, err := client.CallToolRaw("config.set", map[string]any{
		"confirm": true, "profile": "personal",
		"imap": map[string]any{"addr": "attacker.example:143"},
	})
	if err == nil {
		t.Fatal("profile combined with a field-level patch was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error = %v, want the combination refusal", err)
	}
}

// A profile that cannot be built must leave the running config alone.
func TestProfileBuildFailureKeepsLiveConfig(t *testing.T) {
	cfg, work, _ := profileConfig(t)
	cfg.Profiles["broken"] = ProfileSettings{Backend: "carrier-pigeon"}
	client := newConfigClient(t, cfg)

	if _, err := client.CallToolRaw("config.set", map[string]any{
		"confirm": true, "profile": "broken",
	}); err == nil {
		t.Fatal("unbuildable profile accepted")
	}
	if cfg.Maildir != work || cfg.Backend == "carrier-pigeon" {
		t.Errorf("failed switch leaked into the live config: backend=%q maildir=%q", cfg.Backend, cfg.Maildir)
	}
}

func TestConfigGetListsProfiles(t *testing.T) {
	cfg, _, _ := profileConfig(t)
	client := newConfigClient(t, cfg)

	got := rootCallMap(t, client, "config.get", map[string]any{})
	names, ok := got["profiles"].([]string)
	if !ok {
		t.Fatalf("profiles = %T, want []string", got["profiles"])
	}
	if len(names) != 2 || names[0] != "personal" || names[1] != "work" {
		t.Errorf("profiles = %v, want [personal work]", names)
	}
}

// Profiles carry the outbound account too, when they declare one.
func TestProfileSwitchesSender(t *testing.T) {
	cfg, work, _ := profileConfig(t)
	outDir := filepath.Join(t.TempDir(), "outbox")
	cfg.Outbox = OutboxSettings{
		Dir: outDir, From: "work@example.org",
		SMTP: SMTPSettings{Addr: "smtp.work.example:587"},
	}
	cfg.Profiles["personal"] = ProfileSettings{
		Backend: "maildir", Maildir: cfg.Profiles["personal"].Maildir,
		Outbox: &OutboxSettings{
			Dir: outDir, From: "me@personal.example",
			SMTP: SMTPSettings{Addr: "smtp.personal.example:587"},
		},
	}
	client := newConfigClient(t, cfg)

	got := rootCallMap(t, client, "config.set", map[string]any{
		"confirm": true, "profile": "personal",
	})
	if got["ok"] != true {
		t.Fatalf("profile switch = %v", got)
	}
	if s, _ := got["sender"].(string); !strings.Contains(s, "smtp.personal.example") {
		t.Errorf("sender = %v, want the profile's SMTP", got["sender"])
	}
	if cfg.Outbox.From != "me@personal.example" {
		t.Errorf("from = %q, want the profile's", cfg.Outbox.From)
	}
	_ = work
}
