package briefkasten

import (
	"path/filepath"
	"strings"
	"testing"
)

// newIMAPConfigClient starts a server whose IMAP backend already holds
// the operator's credentials — the secrets a redirect would exfiltrate.
func newIMAPConfigClient(t *testing.T) (*Config, func(map[string]any) (any, error)) {
	t.Helper()
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Backend = "imap"
	cfg.IMAP = IMAPSettings{
		Addr: "imap.example.org:993", Username: "alice", Password: "geheim",
	}
	client := newConfigClient(t, cfg)
	return cfg, func(args map[string]any) (any, error) {
		return client.CallToolRaw("config.set", args)
	}
}

// The core of the finding: moving the endpoint must not carry the
// operator's credentials to it. Without this, a caller who does not know
// the password chooses where it is sent.
func TestConfigSetRefusesEndpointChangeWithoutCredentials(t *testing.T) {
	cfg, set := newIMAPConfigClient(t)

	_, err := set(map[string]any{
		"confirm": true,
		"imap":    map[string]any{"addr": "attacker.example:143"},
	})
	if err == nil {
		t.Fatal("addr moved with inherited credentials — the binding rule is gone")
	}
	if !strings.Contains(err.Error(), "do not carry over") {
		t.Errorf("error = %v, want it to explain the credential binding", err)
	}
	if cfg.IMAP.Addr != "imap.example.org:993" || cfg.IMAP.Password != "geheim" {
		t.Errorf("refused patch still mutated the live config: %+v", cfg.IMAP)
	}
}

// The combined payload from the security review: redirect plus TLS
// downgrade, which would have put the password on the wire in cleartext.
func TestConfigSetRefusesRedirectWithTLSDowngrade(t *testing.T) {
	cfg, set := newIMAPConfigClient(t)

	_, err := set(map[string]any{
		"confirm": true,
		"imap":    map[string]any{"addr": "attacker.example:143", "insecure": true},
	})
	if err == nil {
		t.Fatal("the review's exfiltration payload was accepted")
	}
	if cfg.IMAP.Insecure {
		t.Error("live config was downgraded to cleartext")
	}
}

// TLS is one-way at runtime: never off.
func TestConfigSetCannotEnableInsecureAtRuntime(t *testing.T) {
	cfg, set := newIMAPConfigClient(t)

	_, err := set(map[string]any{
		"confirm": true,
		"imap": map[string]any{
			// Same addr, so the binding rule is not what rejects this.
			"addr": "imap.example.org:993", "insecure": true,
		},
	})
	if err == nil {
		t.Fatal("insecure was enabled at runtime")
	}
	if !strings.Contains(err.Error(), "cannot be enabled at runtime") {
		t.Errorf("error = %v, want the TLS-downgrade refusal", err)
	}
	if cfg.IMAP.Insecure {
		t.Error("live config was downgraded")
	}
}

// Supplying credentials for the new endpoint is the legitimate path and
// must keep working — the rule binds credentials, it does not freeze addr.
func TestConfigSetAllowsEndpointChangeWithCredentials(t *testing.T) {
	cfg, set := newIMAPConfigClient(t)

	if _, err := set(map[string]any{
		"confirm": true,
		"imap": map[string]any{
			"addr": "imap.other.example:993", "username": "bob", "password": "neu",
		},
	}); err != nil {
		t.Fatalf("legitimate endpoint change rejected: %v", err)
	}
	if cfg.IMAP.Addr != "imap.other.example:993" || cfg.IMAP.Username != "bob" || cfg.IMAP.Password != "neu" {
		t.Errorf("imap after set = %+v", cfg.IMAP)
	}
}

// clear_credentials is the explicit way to move without secrets, and it
// must actually drop them rather than merely satisfying the check.
func TestConfigSetClearCredentialsDropsSecrets(t *testing.T) {
	cfg, set := newIMAPConfigClient(t)

	if _, err := set(map[string]any{
		"confirm": true,
		"imap":    map[string]any{"addr": "imap.open.example:143", "clear_credentials": true},
	}); err != nil {
		t.Fatalf("clear_credentials rejected: %v", err)
	}
	if cfg.IMAP.Username != "" || cfg.IMAP.Password != "" || cfg.IMAP.OAuth2 != nil {
		t.Errorf("credentials survived clear_credentials: %+v", cfg.IMAP)
	}
}

// config.set must not double as an arbitrary-file-read primitive.
func TestConfigSetConfinesMaildir(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	client := newConfigClient(t, cfg)

	_, err := client.CallToolRaw("config.set", map[string]any{
		"confirm": true, "maildir": "/etc",
	})
	if err == nil {
		t.Fatal("maildir was repointed at /etc")
	}
	if !strings.Contains(err.Error(), "outside the startup maildir") {
		t.Errorf("error = %v, want the confinement refusal", err)
	}
	if cfg.Maildir == "/etc" {
		t.Error("live config was repointed")
	}
}

func TestCheckMaildirConfinement(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, startup, next string
		wantErr             bool
	}{
		{"nested ok", root, filepath.Join(root, "sub"), false},
		{"same dir ok", root, root, false},
		{"absolute escape", root, "/etc", true},
		{"dotdot escape", root, filepath.Join(root, "..", "elsewhere"), true},
		{"no startup maildir", "", "/tmp/anything", true},
	} {
		err := checkMaildirConfinement(tc.startup, tc.next)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: checkMaildirConfinement(%q,%q) = %v, wantErr=%v",
				tc.name, tc.startup, tc.next, err, tc.wantErr)
		}
	}
}

// config.set is the most consequential tool on the surface; it gates
// like the curation and send tools do.
func TestConfigSetRequiresConfirmation(t *testing.T) {
	cfg, _ := LoadConfig("")
	cfg.RuntimeConfig = true
	cfg.Maildir = newRootDir(t)
	client := newConfigClient(t, cfg)

	_, err := client.CallToolRaw("config.set", map[string]any{
		"maildir": filepath.Join(cfg.Maildir, "sub"),
	})
	if err == nil {
		t.Fatal("config.set ran without confirmation")
	}
	if !strings.Contains(err.Error(), "confirmation required") {
		t.Errorf("error = %v, want the confirmation refusal", err)
	}
}
