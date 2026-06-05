package briefkasten

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config configures the briefkasten server. Values are resolved in
// 12-factor precedence: environment variables override the config file,
// which overrides defaults.
type Config struct {
	// Addr is the MCP listen address.
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
	// RuntimeConfig enables the config.get / config.set MCP tools that
	// reconfigure the backend at runtime. Off by default: config.set
	// accepts mailbox credentials, so only enable it on trusted networks.
	RuntimeConfig bool `yaml:"runtime_config"`

	// path remembers where the config was loaded from so runtime changes
	// can be persisted back. Empty when no file was used.
	path string
}

// IMAPSettings is the serializable subset of IMAPConfig.
type IMAPSettings struct {
	Addr     string `yaml:"addr"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Mailbox  string `yaml:"mailbox"`
	Insecure bool   `yaml:"insecure"`
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
	Addr        string `yaml:"addr"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	ImplicitTLS bool   `yaml:"implicit_tls"`
	Insecure    bool   `yaml:"insecure"`
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
	raw, err := os.ReadFile(path)
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

// Save writes the configuration back to the file it was loaded from.
// It fails when the configuration did not come from a file.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config: no config file to save to")
	}
	raw, err := yaml.Marshal(c)
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
	overlay(&c.Addr, "BRIEFKASTEN_ADDR")
	overlay(&c.Backend, "BRIEFKASTEN_BACKEND")
	overlay(&c.Maildir, "BRIEFKASTEN_MAILDIR")
	overlay(&c.IMAP.Addr, "BRIEFKASTEN_IMAP_ADDR")
	overlay(&c.IMAP.Username, "BRIEFKASTEN_IMAP_USER")
	overlay(&c.IMAP.Password, "BRIEFKASTEN_IMAP_PASSWORD")
	overlay(&c.IMAP.Mailbox, "BRIEFKASTEN_IMAP_MAILBOX")
	if v := os.Getenv("BRIEFKASTEN_IMAP_INSECURE"); v != "" {
		c.IMAP.Insecure = v == "1" || v == "true"
	}
	overlay(&c.Outbox.Dir, "BRIEFKASTEN_OUTBOX_DIR")
	overlay(&c.Outbox.From, "BRIEFKASTEN_OUTBOX_FROM")
	overlay(&c.Outbox.DeliverDir, "BRIEFKASTEN_OUTBOX_DELIVER_DIR")
	overlay(&c.Outbox.SMTP.Addr, "BRIEFKASTEN_SMTP_ADDR")
	overlay(&c.Outbox.SMTP.Username, "BRIEFKASTEN_SMTP_USER")
	overlay(&c.Outbox.SMTP.Password, "BRIEFKASTEN_SMTP_PASSWORD")
	if v := os.Getenv("BRIEFKASTEN_SMTP_INSECURE"); v != "" {
		c.Outbox.SMTP.Insecure = v == "1" || v == "true"
	}
	if v := os.Getenv("BRIEFKASTEN_RUNTIME_CONFIG"); v != "" {
		c.RuntimeConfig = v == "1" || v == "true"
	}
}

func overlay(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
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
		mb, err := NewIMAPMailbox(IMAPConfig{
			Addr:     c.IMAP.Addr,
			Username: c.IMAP.Username,
			Password: c.IMAP.Password,
			Mailbox:  c.IMAP.Mailbox,
			Insecure: c.IMAP.Insecure,
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

// BuildOutbox constructs the configured outbox with its sender, or
// (nil, "", nil) when sending is not configured. SMTP wins over the dir
// sender when both are set.
func (c *Config) BuildOutbox() (*Outbox, string, error) {
	if c.Outbox.Dir == "" {
		return nil, "", nil
	}
	var (
		sender Sender
		desc   string
		err    error
	)
	if c.Outbox.SMTP.Addr != "" {
		sender, err = NewSMTPSender(SMTPConfig{
			Addr:        c.Outbox.SMTP.Addr,
			From:        c.Outbox.From,
			Username:    c.Outbox.SMTP.Username,
			Password:    c.Outbox.SMTP.Password,
			ImplicitTLS: c.Outbox.SMTP.ImplicitTLS,
			Insecure:    c.Outbox.SMTP.Insecure,
		})
		desc = "smtp " + c.Outbox.SMTP.Addr
	} else {
		sender, err = NewDirSender(c.Outbox.DeliverDir, c.Outbox.From)
		desc = "dir " + c.Outbox.DeliverDir
	}
	if err != nil {
		return nil, "", err
	}
	ob, err := NewOutbox(c.Outbox.Dir, sender)
	if err != nil {
		return nil, "", err
	}
	return ob, desc, nil
}
