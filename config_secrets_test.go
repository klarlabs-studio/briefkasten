package briefkasten

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg writes a config file and loads it with the env applied, the
// way cmd/briefkasten does.
func writeCfg(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyEnv()
	return cfg
}

// saved returns what Save actually wrote to disk.
func saved(t *testing.T, cfg *Config) string {
	t.Helper()
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A password kept in the environment must stay there. Persisting it
// would quietly convert a 12-factor deployment into one with the
// password on disk, in backups, and possibly in git.
func TestSaveOmitsEnvSourcedPasswords(t *testing.T) {
	t.Setenv("BRIEFKASTEN_IMAP_PASSWORD", "imap-env-secret")
	t.Setenv("BRIEFKASTEN_SMTP_PASSWORD", "smtp-env-secret")
	t.Setenv("BRIEFKASTEN_AUTH_PASSWORD", "auth-env-secret")

	cfg := writeCfg(t, "maildir: "+newRootDir(t)+"\nruntime_config: true\n")

	// The secrets are live in memory — the server still works.
	if cfg.IMAP.Password != "imap-env-secret" || cfg.Outbox.SMTP.Password != "smtp-env-secret" {
		t.Fatalf("env secrets not applied to the live config: %+v", cfg.IMAP)
	}

	out := saved(t, cfg)
	for _, secret := range []string{"imap-env-secret", "smtp-env-secret", "auth-env-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("Save wrote the env-sourced secret %q to disk:\n%s", secret, out)
		}
	}

	// Still usable after the round trip.
	if cfg.IMAP.Password != "imap-env-secret" {
		t.Errorf("Save mutated the live config: %q", cfg.IMAP.Password)
	}
}

// The env-sourced hash is a credential too.
func TestSaveOmitsEnvSourcedPasswordHash(t *testing.T) {
	t.Setenv("BRIEFKASTEN_AUTH_PASSWORD_HASH", "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA")
	cfg := writeCfg(t, "maildir: "+newRootDir(t)+"\n")

	if out := saved(t, cfg); strings.Contains(out, "argon2id") {
		t.Errorf("Save wrote the env-sourced password hash:\n%s", out)
	}
}

// A password the operator put in the file belongs to the file and must
// survive a save — this fix must not silently delete configuration.
func TestSavePreservesFileSourcedPassword(t *testing.T) {
	cfg := writeCfg(t, "backend: imap\nimap:\n  addr: imap.example.org:993\n  username: alice\n  password: from-the-file\n")

	if out := saved(t, cfg); !strings.Contains(out, "from-the-file") {
		t.Errorf("Save dropped a file-sourced password:\n%s", out)
	}
}

// An env var must not suppress an unrelated value that merely matches
// nothing — and with no env set at all, everything persists as before.
func TestSaveWithoutEnvIsUnchanged(t *testing.T) {
	cfg := writeCfg(t, "backend: imap\nimap:\n  addr: imap.example.org:993\n  password: plain\n")

	out := saved(t, cfg)
	if !strings.Contains(out, "plain") {
		t.Errorf("Save dropped a password with no env override in play:\n%s", out)
	}
}

// OAuth2 client secrets hydrated from a credentials file belong to that
// file — copying them into the config file defeats the purpose of
// credentials_file.
func TestSaveOmitsOAuth2SecretsHydratedFromCredentialsFile(t *testing.T) {
	credPath := filepath.Join(t.TempDir(), "google.json")
	clientJSON := `{"web":{"client_id":"cid.apps.googleusercontent.com","client_secret":"the-client-secret","token_uri":"https://oauth2.googleapis.com/token","auth_uri":"https://accounts.google.com/o/oauth2/auth","redirect_uris":["http://127.0.0.1/cb"]}}`
	if err := os.WriteFile(credPath, []byte(clientJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := writeCfg(t, "backend: imap\nimap:\n  addr: imap.gmail.com:993\n  username: you@gmail.com\n  oauth2:\n    credentials_file: "+credPath+"\n")

	// Building the mailbox hydrates the settings from the file.
	if _, _, err := cfg.BuildMailbox(); err != nil {
		t.Fatalf("BuildMailbox: %v", err)
	}
	if cfg.IMAP.OAuth2.ClientSecret != "the-client-secret" {
		t.Fatalf("precondition: credentials not hydrated (%+v)", cfg.IMAP.OAuth2)
	}

	out := saved(t, cfg)
	if strings.Contains(out, "the-client-secret") {
		t.Errorf("Save copied the client secret out of the credentials file:\n%s", out)
	}
	if strings.Contains(out, "cid.apps.googleusercontent.com") {
		t.Errorf("Save copied the hydrated client id:\n%s", out)
	}
	// The pointer to the credentials file must survive, or the next load
	// has nothing to hydrate from.
	if !strings.Contains(out, credPath) {
		t.Errorf("Save dropped credentials_file, breaking the next load:\n%s", out)
	}
	// And the live config still holds the hydrated values.
	if cfg.IMAP.OAuth2.ClientSecret != "the-client-secret" {
		t.Errorf("Save mutated the live OAuth2 settings: %+v", cfg.IMAP.OAuth2)
	}
}

// An operator who wrote client_id explicitly wins over the credentials
// file today, and must keep winning across a save.
func TestSavePreservesExplicitOAuth2Values(t *testing.T) {
	credPath := filepath.Join(t.TempDir(), "google.json")
	clientJSON := `{"web":{"client_id":"from-file","client_secret":"secret-from-file","token_uri":"https://oauth2.googleapis.com/token","auth_uri":"https://a","redirect_uris":["http://127.0.0.1/cb"]}}`
	if err := os.WriteFile(credPath, []byte(clientJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := writeCfg(t, "backend: imap\nimap:\n  addr: imap.gmail.com:993\n  username: you@gmail.com\n  oauth2:\n    client_id: explicitly-mine\n    credentials_file: "+credPath+"\n")
	if _, _, err := cfg.BuildMailbox(); err != nil {
		t.Fatalf("BuildMailbox: %v", err)
	}

	out := saved(t, cfg)
	if !strings.Contains(out, "explicitly-mine") {
		t.Errorf("Save dropped an explicitly configured client_id:\n%s", out)
	}
	if strings.Contains(out, "secret-from-file") {
		t.Errorf("Save still copied the hydrated client secret:\n%s", out)
	}
}

func TestWithoutHydratedNilIsNil(t *testing.T) {
	var o *OAuth2Settings
	if got := o.WithoutHydrated(); got != nil {
		t.Errorf("WithoutHydrated on nil = %v, want nil", got)
	}
}
