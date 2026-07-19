package briefkasten

import (
	"context"
	"fmt"
	"os"
	"sort"

	authdomain "github.com/klarlabs-studio/auth-go/domain"
	mcp "go.klarlabs.de/mcp"
	"gopkg.in/yaml.v3"

	"go.klarlabs.de/briefkasten/infrastructure/mcpserver"
)

// Config configures the briefkasten server. Values are resolved in
// 12-factor precedence: environment variables override the config file,
// which overrides defaults.
type Config struct {
	// Transport selects how the MCP server is exposed: "http" (default,
	// listens on Addr) or "stdio" (JSON-RPC over stdin/stdout, for
	// clients that spawn the binary as a child process).
	Transport string `yaml:"transport"`
	// Addr is the MCP listen address. Ignored when Transport is "stdio".
	Addr string `yaml:"addr"`
	// Backend selects the mailbox backend: "maildir" or "imap". When empty,
	// "imap" is inferred if IMAP.Addr is set, "maildir" otherwise.
	Backend string `yaml:"backend"`
	// Maildir is the maildir root for the maildir backend.
	Maildir string `yaml:"maildir"`
	// IMAP configures the IMAP backend.
	IMAP IMAPSettings `yaml:"imap"`
	// Outbox configures outbound mail; the email.send tools register only
	// when Outbox.Dir is set.
	Outbox OutboxSettings `yaml:"outbox"`
	// Accounts are additional named mailboxes; the top-level backend is
	// the default account. Tools route via their optional account
	// argument.
	Accounts map[string]AccountSettings `yaml:"accounts"`
	// Profiles are whole configurations the operator declares up front,
	// switchable at runtime by name via config.set {"profile": "..."}.
	//
	// A profile is the safe way to move the mailbox somewhere else: the
	// endpoint and its credentials are written together, by the operator,
	// in this file. Switching applies them as a unit and inherits nothing
	// from the live config, so a caller choosing a profile is choosing
	// among destinations the operator already approved — it can never
	// name an endpoint of its own.
	Profiles map[string]ProfileSettings `yaml:"profiles"`
	// RuntimeConfig enables the config.get / config.set MCP tools that
	// reconfigure the backend at runtime. Off by default, and hardened
	// when on: config.set requires human confirmation, credentials never
	// follow an addr change (supply them for the new endpoint or pass
	// clear_credentials), TLS cannot be disabled at runtime, and the
	// maildir stays inside the one chosen at startup. Those rules matter
	// because the caller may be a model acting on mail content it just
	// read — enable this only where that caller is trusted.
	RuntimeConfig bool `yaml:"runtime_config"`
	// Auth guards the MCP endpoint. Off by default (open endpoint —
	// fine on localhost; configure it before exposing the port).
	Auth AuthSettings `yaml:"auth"`

	// path remembers where the config was loaded from so runtime changes
	// can be persisted back. Empty when no file was used.
	path string
	// envSecrets records which secrets came from the environment rather
	// than the file, so Save can leave them where the operator put them.
	// Keeping a password in BRIEFKASTEN_IMAP_PASSWORD is a deliberate
	// choice; writing it to disk on the first config.set would quietly
	// undo it and leak the secret into backups and version control.
	envSecrets map[string]bool
}

// AuthSettings guards the MCP HTTP endpoint with basic auth. Every
// request must carry the credential; the MCP handshake (initialize,
// ping) stays open so clients can negotiate first.
type AuthSettings struct {
	Basic BasicAuthSettings `yaml:"basic"`
}

// BasicAuthSettings is one username with either a plaintext password
// (hashed with argon2id at startup — consistent with the mailbox
// passwords this file already holds) or a pre-computed argon2id PHC
// hash, which wins when both are set.
type BasicAuthSettings struct {
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordHash string `yaml:"password_hash"`
}

// Enabled reports whether basic auth is configured.
func (b BasicAuthSettings) Enabled() bool {
	return b.Username != "" && (b.Password != "" || b.PasswordHash != "")
}

// IMAPSettings is the serializable subset of IMAPConfig.
type IMAPSettings struct {
	Addr     string          `yaml:"addr"`
	Username string          `yaml:"username"`
	Password string          `yaml:"password"`
	Mailbox  string          `yaml:"mailbox"`
	Insecure bool            `yaml:"insecure"`
	OAuth2   *OAuth2Settings `yaml:"oauth2"`
}

// OutboxSettings configures outbound mail.
type OutboxSettings struct {
	// Dir is the outbox state root (out/<state>/). Empty disables sending.
	Dir string `yaml:"dir"`
	// From is the sender address. Required when Dir is set.
	From string `yaml:"from"`
	// DeliverDir selects the DirSender: messages land as .eml in
	// <deliver_dir>/new. Used when SMTP.Addr is empty.
	DeliverDir string `yaml:"deliver_dir"`
	// SMTP selects the SMTPSender when Addr is set.
	SMTP SMTPSettings `yaml:"smtp"`
}

// SMTPSettings is the serializable subset of SMTPConfig.
type SMTPSettings struct {
	Addr        string          `yaml:"addr"`
	Username    string          `yaml:"username"`
	Password    string          `yaml:"password"`
	ImplicitTLS bool            `yaml:"implicit_tls"`
	Insecure    bool            `yaml:"insecure"`
	OAuth2      *OAuth2Settings `yaml:"oauth2"`
}

// AccountSettings configures one named mailbox.
type AccountSettings struct {
	Backend string       `yaml:"backend"`
	Maildir string       `yaml:"maildir"`
	IMAP    IMAPSettings `yaml:"imap"`
}

// ProfileSettings is one named, operator-declared configuration that
// config.set can activate wholesale. Outbox is optional: a profile that
// omits it leaves outbound mail as configured.
type ProfileSettings struct {
	Backend string          `yaml:"backend"`
	Maildir string          `yaml:"maildir"`
	IMAP    IMAPSettings    `yaml:"imap"`
	Outbox  *OutboxSettings `yaml:"outbox,omitempty"`
}

// apply returns a copy of c with the profile's mailbox settings
// substituted wholesale. Nothing carries over from the live mailbox
// config: the profile names its own endpoint and credentials, so a
// switch cannot smuggle the current password to a different server.
// Non-mailbox settings (transport, auth, accounts, profiles) are
// untouched — a profile switches where mail comes from, not how the
// server is exposed.
func (c *Config) applyProfile(p ProfileSettings) Config {
	next := *c
	next.Backend = p.Backend
	next.Maildir = p.Maildir
	next.IMAP = p.IMAP
	if p.Outbox != nil {
		next.Outbox = *p.Outbox
	}
	return next
}

// ProfileNames lists the declared profiles in stable order.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BuildAccounts constructs the named mailboxes.
func (c *Config) BuildAccounts() (map[string]Mailbox, error) {
	if len(c.Accounts) == 0 {
		return nil, nil
	}
	out := make(map[string]Mailbox, len(c.Accounts))
	for name, a := range c.Accounts {
		sub := &Config{Backend: a.Backend, Maildir: a.Maildir, IMAP: a.IMAP}
		mb, _, err := sub.BuildMailbox()
		if err != nil {
			return nil, fmt.Errorf("config: account %q: %w", name, err)
		}
		out[name] = mb
	}
	return out, nil
}

// LoadConfig returns the default configuration overlaid with the YAML file
// at path. An empty path skips the file (defaults only); a non-empty path
// must exist and parse.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		Addr:    ":8090",
		Maildir: "./maildir",
	}
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- config path is supplied by the operator running the server
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.path = path
	return cfg, nil
}

// Path returns the config file path the configuration was loaded from,
// or "" when no file was used.
func (c *Config) Path() string { return c.path }

// forSave returns the copy of the configuration that goes to disk:
// identical to the live one except that secrets the operator kept
// elsewhere are left out.
//
// Two sources are elided. Secrets read from BRIEFKASTEN_* variables stay
// in the environment — persisting them would silently convert a
// 12-factor deployment into one with passwords on disk. And OAuth2
// fields hydrated from a credentials file stay in that file, which is
// the whole reason credentials_file exists; they are re-read on the next
// load, so nothing is lost.
//
// A value the operator wrote in the file, or set deliberately through
// config.set, is untouched and still persists.
func (c *Config) forSave() *Config {
	out := *c
	if c.envSecrets["BRIEFKASTEN_IMAP_PASSWORD"] {
		out.IMAP.Password = ""
	}
	if c.envSecrets["BRIEFKASTEN_SMTP_PASSWORD"] {
		out.Outbox.SMTP.Password = ""
	}
	if c.envSecrets["BRIEFKASTEN_AUTH_PASSWORD"] {
		out.Auth.Basic.Password = ""
	}
	if c.envSecrets["BRIEFKASTEN_AUTH_PASSWORD_HASH"] {
		out.Auth.Basic.PasswordHash = ""
	}
	out.IMAP.OAuth2 = c.IMAP.OAuth2.WithoutHydrated()
	out.Outbox.SMTP.OAuth2 = c.Outbox.SMTP.OAuth2.WithoutHydrated()
	return &out
}

// Save writes the configuration back to the file it was loaded from,
// minus the secrets forSave elides. It fails when the configuration did
// not come from a file.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config: no config file to save to")
	}
	raw, err := yaml.Marshal(c.forSave())
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(c.path, raw, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", c.path, err)
	}
	return nil
}

// ApplyEnv overlays BRIEFKASTEN_* environment variables onto the
// configuration. Unset variables leave existing values untouched.
func (c *Config) ApplyEnv() {
	overlay(&c.Transport, "BRIEFKASTEN_TRANSPORT")
	overlay(&c.Addr, "BRIEFKASTEN_ADDR")
	overlay(&c.Backend, "BRIEFKASTEN_BACKEND")
	overlay(&c.Maildir, "BRIEFKASTEN_MAILDIR")
	overlay(&c.IMAP.Addr, "BRIEFKASTEN_IMAP_ADDR")
	overlay(&c.IMAP.Username, "BRIEFKASTEN_IMAP_USER")
	c.overlaySecret(&c.IMAP.Password, "BRIEFKASTEN_IMAP_PASSWORD")
	overlay(&c.IMAP.Mailbox, "BRIEFKASTEN_IMAP_MAILBOX")
	if v := os.Getenv("BRIEFKASTEN_IMAP_INSECURE"); v != "" {
		c.IMAP.Insecure = v == "1" || v == "true"
	}
	overlay(&c.Outbox.Dir, "BRIEFKASTEN_OUTBOX_DIR")
	overlay(&c.Outbox.From, "BRIEFKASTEN_OUTBOX_FROM")
	overlay(&c.Outbox.DeliverDir, "BRIEFKASTEN_OUTBOX_DELIVER_DIR")
	overlay(&c.Outbox.SMTP.Addr, "BRIEFKASTEN_SMTP_ADDR")
	overlay(&c.Outbox.SMTP.Username, "BRIEFKASTEN_SMTP_USER")
	c.overlaySecret(&c.Outbox.SMTP.Password, "BRIEFKASTEN_SMTP_PASSWORD")
	if v := os.Getenv("BRIEFKASTEN_SMTP_INSECURE"); v != "" {
		c.Outbox.SMTP.Insecure = v == "1" || v == "true"
	}
	// Google credentials file (service-account key or OAuth client secret) for
	// IMAP and SMTP OAuth2. Setting it allocates the OAuth2 block if absent.
	overlayCredentialsFile(&c.IMAP.OAuth2, "BRIEFKASTEN_IMAP_OAUTH2_CREDENTIALS_FILE")
	overlayCredentialsFile(&c.Outbox.SMTP.OAuth2, "BRIEFKASTEN_SMTP_OAUTH2_CREDENTIALS_FILE")
	if v := os.Getenv("BRIEFKASTEN_RUNTIME_CONFIG"); v != "" {
		c.RuntimeConfig = v == "1" || v == "true"
	}
	overlay(&c.Auth.Basic.Username, "BRIEFKASTEN_AUTH_USER")
	c.overlaySecret(&c.Auth.Basic.Password, "BRIEFKASTEN_AUTH_PASSWORD")
	c.overlaySecret(&c.Auth.Basic.PasswordHash, "BRIEFKASTEN_AUTH_PASSWORD_HASH")
}

// overlayCredentialsFile sets the OAuth2 credentials-file path from an env var,
// allocating the OAuth2 settings block when it is not already present.
func overlayCredentialsFile(o **OAuth2Settings, key string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	if *o == nil {
		*o = &OAuth2Settings{}
	}
	(*o).CredentialsFile = v
}

// overlaySecret is overlay for values that must not be written back to
// the config file. It records the source so Save can omit the field.
func (c *Config) overlaySecret(dst *string, key string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	*dst = v
	if c.envSecrets == nil {
		c.envSecrets = map[string]bool{}
	}
	c.envSecrets[key] = true
}

func overlay(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

// Transport names. HTTP is the default; stdio serves JSON-RPC over
// stdin/stdout for clients that spawn the binary directly.
const (
	TransportHTTP  = "http"
	TransportStdio = "stdio"
)

// ResolvedTransport returns the effective transport: the explicit
// Transport value, or "http" when unset.
func (c *Config) ResolvedTransport() string {
	if c.Transport == "" {
		return TransportHTTP
	}
	return c.Transport
}

// ValidateTransport rejects transports the server cannot serve, so a
// typo fails at startup rather than silently falling back to HTTP.
func (c *Config) ValidateTransport() error {
	switch c.ResolvedTransport() {
	case TransportHTTP, TransportStdio:
		return nil
	default:
		return fmt.Errorf("config: unknown transport %q (want %q or %q)",
			c.Transport, TransportHTTP, TransportStdio)
	}
}

// LogWriterName reports which standard stream the server must log to.
// Stdio uses stdout for the JSON-RPC framing, so diagnostics have to go
// to stderr or they corrupt the protocol stream.
func (c *Config) LogWriterName() string {
	if c.ResolvedTransport() == TransportStdio {
		return "stderr"
	}
	return "stdout"
}

// LogWriter returns the stream matching LogWriterName.
func (c *Config) LogWriter() *os.File {
	if c.LogWriterName() == "stderr" {
		return os.Stderr
	}
	return os.Stdout
}

// ResolvedBackend returns the effective backend name: the explicit Backend
// value, or "imap" when IMAP.Addr is set, "maildir" otherwise.
func (c *Config) ResolvedBackend() string {
	if c.Backend != "" {
		return c.Backend
	}
	if c.IMAP.Addr != "" {
		return "imap"
	}
	return "maildir"
}

// BuildMailbox constructs the configured backend and a short description
// for logging.
func (c *Config) BuildMailbox() (Mailbox, string, error) {
	switch backend := c.ResolvedBackend(); backend {
	case "maildir":
		mb, err := NewDirMailbox(c.Maildir)
		if err != nil {
			return nil, "", err
		}
		return mb, "maildir " + c.Maildir, nil
	case "imap":
		if c.IMAP.OAuth2 != nil {
			if err := c.IMAP.OAuth2.LoadCredentials(context.Background(), c.IMAP.Username); err != nil {
				return nil, "", fmt.Errorf("config: imap oauth2: %w", err)
			}
		}
		mb, err := NewIMAPMailbox(IMAPConfig{
			Addr:     c.IMAP.Addr,
			Username: c.IMAP.Username,
			Password: c.IMAP.Password,
			Mailbox:  c.IMAP.Mailbox,
			Insecure: c.IMAP.Insecure,
			OAuth2:   c.IMAP.OAuth2,
		})
		if err != nil {
			return nil, "", err
		}
		// Remote backend: guard with timeout, retry, circuit breaker.
		return Resilient(mb, ResilienceConfig{}), "imap " + c.IMAP.Addr, nil
	default:
		return nil, "", fmt.Errorf("config: unknown backend %q (want maildir or imap)", backend)
	}
}

// BuildWatcher constructs a new-mail watcher for the configured backend so the
// MCP server can push notifications/resources/updated to subscribers instead
// of relying on polling. Returns nil when the backend cannot be watched.
func (c *Config) BuildWatcher() Watcher {
	switch c.ResolvedBackend() {
	case "maildir":
		return NewDirWatcher(c.Maildir)
	case "imap":
		// OAuth2 credentials are hydrated by BuildMailbox, which runs first and
		// shares the same *OAuth2Settings — no separate (error-swallowing) load
		// here.
		return NewIMAPWatcher(IMAPConfig{
			Addr:     c.IMAP.Addr,
			Username: c.IMAP.Username,
			Password: c.IMAP.Password,
			Mailbox:  c.IMAP.Mailbox,
			Insecure: c.IMAP.Insecure,
			OAuth2:   c.IMAP.OAuth2,
		})
	default:
		return nil
	}
}

// buildSender constructs the configured outbound sender (SMTP when an address
// is set, else the dir sender) and a short description. It is the swappable
// unit for runtime reconfiguration.
func (c *Config) buildSender() (Sender, string, error) {
	if c.Outbox.SMTP.Addr != "" {
		if c.Outbox.SMTP.OAuth2 != nil {
			if err := c.Outbox.SMTP.OAuth2.LoadCredentials(context.Background(), c.Outbox.SMTP.Username); err != nil {
				return nil, "", fmt.Errorf("config: smtp oauth2: %w", err)
			}
		}
		sender, err := NewSMTPSender(SMTPConfig{
			Addr:        c.Outbox.SMTP.Addr,
			From:        c.Outbox.From,
			Username:    c.Outbox.SMTP.Username,
			Password:    c.Outbox.SMTP.Password,
			ImplicitTLS: c.Outbox.SMTP.ImplicitTLS,
			Insecure:    c.Outbox.SMTP.Insecure,
			OAuth2:      c.Outbox.SMTP.OAuth2,
		})
		if err != nil {
			return nil, "", err
		}
		return sender, "smtp " + c.Outbox.SMTP.Addr, nil
	}
	sender, err := NewDirSender(c.Outbox.DeliverDir, c.Outbox.From)
	if err != nil {
		return nil, "", err
	}
	return sender, "dir " + c.Outbox.DeliverDir, nil
}

// BuildOutbox constructs the configured outbox with its sender, or
// (nil, "", nil) when sending is not configured. SMTP wins over the dir
// sender when both are set.
func (c *Config) BuildOutbox() (*Outbox, string, error) {
	if c.Outbox.Dir == "" {
		return nil, "", nil
	}
	sender, desc, err := c.buildSender()
	if err != nil {
		return nil, "", err
	}
	ob, err := NewOutbox(c.Outbox.Dir, sender)
	if err != nil {
		return nil, "", err
	}
	return ob, desc, nil
}

// BuildAuthMiddleware returns the MCP middleware guarding the endpoint,
// or nil when auth is not configured. A plaintext password is hashed
// with argon2id at startup; a pre-computed password_hash (PHC string)
// wins when both are set.
func (c *Config) BuildAuthMiddleware() (mcp.Middleware, error) {
	b := c.Auth.Basic
	if !b.Enabled() {
		return nil, nil
	}
	var (
		hash authdomain.PasswordHash
		err  error
	)
	if b.PasswordHash != "" {
		hash, err = authdomain.PasswordHashFromString(b.PasswordHash)
	} else {
		hash, err = authdomain.HashPassword(b.Password, authdomain.DefaultArgon2idParams())
	}
	if err != nil {
		return nil, fmt.Errorf("config: auth.basic: %w", err)
	}
	return mcpserver.BasicAuth(b.Username, hash), nil
}
