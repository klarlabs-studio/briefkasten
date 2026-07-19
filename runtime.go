package briefkasten

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	mcp "go.klarlabs.de/mcp"

	"go.klarlabs.de/briefkasten/infrastructure/mcpserver"
)

// NewConfigServer builds the configured backend and serves it behind a
// Switchable. When cfg.RuntimeConfig is enabled, config.get and config.set
// tools allow inspecting and reconfiguring the backend — including OAuth2
// credentials and the outbound sender — at runtime, without a restart. When the
// outbox is configured, email.send / email.send_status register and the
// returned Outbox is non-nil — the caller runs its delivery worker.
func NewConfigServer(cfg *Config) (*mcp.Server, *Outbox, error) {
	mb, _, err := cfg.BuildMailbox()
	if err != nil {
		return nil, nil, err
	}
	accounts, err := cfg.BuildAccounts()
	if err != nil {
		return nil, nil, err
	}

	// The outbound sender is held behind a SwitchableSender so config.set can
	// repoint it at a freshly-built sender (new credentials/provider) while the
	// delivery worker keeps running on the same *Outbox.
	var (
		ob       *Outbox
		swSender *SwitchableSender
	)
	if cfg.Outbox.Dir != "" {
		sender, _, serr := cfg.buildSender()
		if serr != nil {
			return nil, nil, serr
		}
		swSender = NewSwitchableSender(sender)
		ob, err = NewOutbox(cfg.Outbox.Dir, swSender)
		if err != nil {
			return nil, nil, err
		}
	}

	sw := NewSwitchable(mb)
	opts := []ServerOption{WithAccounts(accounts)}
	if ob != nil {
		opts = append(opts, WithOutbox(ob))
	}
	srv := NewServer(sw, opts...)
	if cfg.RuntimeConfig {
		registerConfigTools(srv, cfg, sw, swSender)
	}
	return srv, ob, nil
}

// oauth2Patch carries a partial OAuth2 settings update. A patch that touches
// any field rebuilds the OAuth2 settings from scratch, so a new credentials
// file is re-read and a stale token source is dropped.
type oauth2Patch struct {
	CredentialsFile string `json:"credentials_file,omitempty"`
	ClientID        string `json:"client_id,omitempty"`
	ClientSecret    string `json:"client_secret,omitempty"`
	RefreshToken    string `json:"refresh_token,omitempty"`
	TokenURL        string `json:"token_url,omitempty"`
	AccessToken     string `json:"access_token,omitempty"`
	Mechanism       string `json:"mechanism,omitempty"`
}

// imapPatch carries a partial IMAP settings update; nil/empty fields keep
// current values — except credentials when Addr changes, see
// errCredentialBinding.
type imapPatch struct {
	Addr     string       `json:"addr,omitempty"`
	Username string       `json:"username,omitempty"`
	Password string       `json:"password,omitempty"`
	Mailbox  string       `json:"mailbox,omitempty"`
	Insecure *bool        `json:"insecure,omitempty"`
	OAuth2   *oauth2Patch `json:"oauth2,omitempty"`
	// ClearCredentials sends the new endpoint no credentials at all. It is
	// the explicit way to move addr without carrying secrets across.
	ClearCredentials bool `json:"clear_credentials,omitempty"`
}

// smtpPatch carries a partial SMTP settings update.
type smtpPatch struct {
	Addr        string       `json:"addr,omitempty"`
	Username    string       `json:"username,omitempty"`
	Password    string       `json:"password,omitempty"`
	ImplicitTLS *bool        `json:"implicit_tls,omitempty"`
	Insecure    *bool        `json:"insecure,omitempty"`
	OAuth2      *oauth2Patch `json:"oauth2,omitempty"`
	// ClearCredentials sends the new endpoint no credentials at all.
	ClearCredentials bool `json:"clear_credentials,omitempty"`
}

// confirmReconfigure puts a human in the loop before the mailbox is
// repointed. config.set can move where credentials are sent and where
// mail is read from, which makes it the most consequential tool on the
// surface — more so than the curation tools that already gate.
func confirmReconfigure(ctx context.Context, confirmed bool, backend, maildir string, imap *imapPatch) error {
	target := "the mailbox configuration"
	switch {
	case imap != nil && imap.Addr != "":
		target = "the IMAP server to " + imap.Addr
	case maildir != "":
		target = "the maildir to " + maildir
	case backend != "":
		target = "the backend to " + backend
	}
	return mcpserver.ConfirmAction(ctx, confirmed,
		"reconfigure "+target,
		"Confirm reconfiguring "+target+"? This changes where mail is read from and where credentials are sent.")
}

// checkMaildirConfinement keeps a runtime maildir switch inside the
// subtree the operator chose at startup. Without it, config.set doubles
// as an arbitrary-file-read primitive: point the mailbox at /etc and
// email.list/email.fetch will serve it.
func checkMaildirConfinement(startup, next string) error {
	if startup == "" {
		return errors.New("briefkasten: maildir cannot be set at runtime because the server did not start with one — configure it in the config file and restart")
	}
	root, err := filepath.Abs(startup)
	if err != nil {
		return fmt.Errorf("briefkasten: resolve startup maildir: %w", err)
	}
	target, err := filepath.Abs(next)
	if err != nil {
		return fmt.Errorf("briefkasten: resolve maildir %q: %w", next, err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("briefkasten: maildir %q is outside the startup maildir %q — runtime changes stay within it", next, startup)
	}
	return nil
}

// errCredentialBinding explains why a bare endpoint change is refused.
// Credentials belong to the endpoint they were issued for: inheriting
// them across an addr change would let a caller who does not know the
// password choose where it is sent.
func errCredentialBinding(which, from, to string) error {
	return fmt.Errorf(
		"briefkasten: %s addr changes from %q to %q, so the configured credentials do not carry over — "+
			"supply username/password (or oauth2) for the new endpoint in the same call, "+
			"or set clear_credentials=true to connect without them",
		which, from, to)
}

// hasCredentials reports whether a patch supplies its own credentials.
func hasCredentials(username, password string, oa *oauth2Patch) bool {
	return username != "" || password != "" || oa != nil
}

// outboxPatch carries a partial outbound configuration update.
type outboxPatch struct {
	From       string     `json:"from,omitempty"`
	DeliverDir string     `json:"deliver_dir,omitempty"`
	SMTP       *smtpPatch `json:"smtp,omitempty"`
}

// applyOAuth2Patch returns a FRESH OAuth2Settings: current values overlaid with
// the patch. Building a new struct drops any cached token source so the next
// send mints tokens from the new credentials.
func applyOAuth2Patch(cur *OAuth2Settings, p *oauth2Patch) *OAuth2Settings {
	n := &OAuth2Settings{}
	if cur != nil {
		n.ClientID, n.ClientSecret = cur.ClientID, cur.ClientSecret
		n.RefreshToken, n.TokenURL = cur.RefreshToken, cur.TokenURL
		n.AccessToken, n.Mechanism = cur.AccessToken, cur.Mechanism
		n.CredentialsFile = cur.CredentialsFile
	}
	if p.CredentialsFile != "" && p.CredentialsFile != n.CredentialsFile {
		// A new credentials file is the source of truth for the client identity.
		// Clear the carried-over (old-file) client_id/secret/token_url so
		// LoadCredentials refills them from the new file; an explicit override in
		// this same patch is applied below and still wins.
		n.ClientID, n.ClientSecret, n.TokenURL = "", "", ""
		n.CredentialsFile = p.CredentialsFile
	} else if p.CredentialsFile != "" {
		n.CredentialsFile = p.CredentialsFile
	}
	if p.ClientID != "" {
		n.ClientID = p.ClientID
	}
	if p.ClientSecret != "" {
		n.ClientSecret = p.ClientSecret
	}
	if p.RefreshToken != "" {
		n.RefreshToken = p.RefreshToken
	}
	if p.TokenURL != "" {
		n.TokenURL = p.TokenURL
	}
	if p.AccessToken != "" {
		n.AccessToken = p.AccessToken
	}
	if p.Mechanism != "" {
		n.Mechanism = p.Mechanism
	}
	return n
}

func registerConfigTools(srv *mcp.Server, cfg *Config, sw *Switchable, swSender *SwitchableSender) {
	var mu sync.Mutex // serializes config mutations

	// The maildir the operator chose at startup bounds every later
	// runtime change; captured before any patch can move it.
	startupMaildir := cfg.Maildir

	srv.Tool("config.get").
		Description("Inspect the active mailbox configuration, including the profiles available to config.set. Credentials are redacted.").
		ReadOnly().
		Handler(func(_ context.Context, _ struct{}) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			out := map[string]any{
				"backend": cfg.ResolvedBackend(),
				"maildir": cfg.Maildir,
				"imap": map[string]any{
					"addr":     cfg.IMAP.Addr,
					"username": cfg.IMAP.Username,
					"mailbox":  cfg.IMAP.Mailbox,
					"insecure": cfg.IMAP.Insecure,
					// password / oauth2 secrets intentionally omitted
				},
				"sending": swSender != nil,
				// The switchable destinations, so a caller can pick one
				// instead of inventing an endpoint.
				"profiles": cfg.ProfileNames(),
			}
			if cfg.Path() != "" {
				out["config_file"] = cfg.Path()
			}
			return out, nil
		})

	srv.Tool("config.set").
		Description("Reconfigure the mailbox and outbound sender at runtime — including OAuth2 credentials (oauth2.credentials_file) for Gmail/Outlook. Partial update: omitted fields keep their current values, EXCEPT credentials when addr changes — those must be supplied for the new endpoint (or cleared with clear_credentials). TLS cannot be disabled at runtime and the maildir stays within the startup one. Requires human confirmation — the host is asked via elicitation, or pass confirm=true after asking the user yourself. The new backend and sender are validated before they replace the old ones; when started from a config file the change is persisted there.").
		Destructive().
		Handler(func(ctx context.Context, in struct {
			Profile string       `json:"profile,omitempty" jsonschema:"description=Name of a profile declared in the config file (see config.get); applied wholesale and cannot be combined with the field-level patches"`
			Backend string       `json:"backend,omitempty"`
			Maildir string       `json:"maildir,omitempty"`
			IMAP    *imapPatch   `json:"imap,omitempty"`
			Outbox  *outboxPatch `json:"outbox,omitempty"`
			Confirm bool         `json:"confirm,omitempty" jsonschema:"description=Set true only after the user explicitly approved reconfiguring the mailbox"`
		},
		) (map[string]any, error) {
			if in.Profile != "" {
				if in.Backend != "" || in.Maildir != "" || in.IMAP != nil || in.Outbox != nil {
					return nil, errors.New("briefkasten: profile cannot be combined with field-level settings — a profile is applied whole, so mixing the two would silently drop one of them")
				}
				return applyProfile(ctx, in.Profile, in.Confirm, &mu, cfg, sw, swSender)
			}
			if err := confirmReconfigure(ctx, in.Confirm, in.Backend, in.Maildir, in.IMAP); err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()

			// Merge the patch onto a copy; only a successful build mutates cfg.
			next := *cfg
			if in.Backend != "" {
				next.Backend = in.Backend
			}
			if in.Maildir != "" && in.Maildir != cfg.Maildir {
				if err := checkMaildirConfinement(startupMaildir, in.Maildir); err != nil {
					return nil, err
				}
				next.Maildir = in.Maildir
			}
			if in.IMAP != nil {
				if in.IMAP.Addr != "" && in.IMAP.Addr != cfg.IMAP.Addr {
					// Endpoint change: credentials do not follow.
					if !in.IMAP.ClearCredentials && !hasCredentials(in.IMAP.Username, in.IMAP.Password, in.IMAP.OAuth2) {
						return nil, errCredentialBinding("imap", cfg.IMAP.Addr, in.IMAP.Addr)
					}
					next.IMAP.Username, next.IMAP.Password, next.IMAP.OAuth2 = "", "", nil
					next.IMAP.Addr = in.IMAP.Addr
				}
				if in.IMAP.Username != "" {
					next.IMAP.Username = in.IMAP.Username
				}
				if in.IMAP.Password != "" {
					next.IMAP.Password = in.IMAP.Password
				}
				if in.IMAP.Mailbox != "" {
					next.IMAP.Mailbox = in.IMAP.Mailbox
				}
				if in.IMAP.Insecure != nil {
					// TLS is one-way at runtime: a caller may re-enable it but
					// never turn it off, so no patch can downgrade a live
					// connection to cleartext.
					if *in.IMAP.Insecure && !cfg.IMAP.Insecure {
						return nil, errors.New("briefkasten: imap insecure cannot be enabled at runtime — set it in the config file and restart")
					}
					next.IMAP.Insecure = *in.IMAP.Insecure
				}
				if in.IMAP.OAuth2 != nil {
					next.IMAP.OAuth2 = applyOAuth2Patch(next.IMAP.OAuth2, in.IMAP.OAuth2)
				}
			}
			if in.Outbox != nil {
				if in.Outbox.From != "" {
					next.Outbox.From = in.Outbox.From
				}
				if in.Outbox.DeliverDir != "" {
					next.Outbox.DeliverDir = in.Outbox.DeliverDir
				}
				if sp := in.Outbox.SMTP; sp != nil {
					if sp.Addr != "" && sp.Addr != cfg.Outbox.SMTP.Addr {
						if !sp.ClearCredentials && !hasCredentials(sp.Username, sp.Password, sp.OAuth2) {
							return nil, errCredentialBinding("smtp", cfg.Outbox.SMTP.Addr, sp.Addr)
						}
						next.Outbox.SMTP.Username, next.Outbox.SMTP.Password, next.Outbox.SMTP.OAuth2 = "", "", nil
						next.Outbox.SMTP.Addr = sp.Addr
					}
					if sp.Username != "" {
						next.Outbox.SMTP.Username = sp.Username
					}
					if sp.Password != "" {
						next.Outbox.SMTP.Password = sp.Password
					}
					if sp.ImplicitTLS != nil {
						next.Outbox.SMTP.ImplicitTLS = *sp.ImplicitTLS
					}
					if sp.Insecure != nil {
						if *sp.Insecure && !cfg.Outbox.SMTP.Insecure {
							return nil, errors.New("briefkasten: smtp insecure cannot be enabled at runtime — set it in the config file and restart")
						}
						next.Outbox.SMTP.Insecure = *sp.Insecure
					}
					if sp.OAuth2 != nil {
						next.Outbox.SMTP.OAuth2 = applyOAuth2Patch(next.Outbox.SMTP.OAuth2, sp.OAuth2)
					}
				}
			}

			return commitConfig(&next, cfg, sw, swSender)
		})
}

// commitConfig validates the candidate configuration, and only once both
// the mailbox and (when sending) the sender build does it swap them in.
// A configuration that cannot be built leaves the running one untouched.
func commitConfig(next, cfg *Config, sw *Switchable, swSender *SwitchableSender) (map[string]any, error) {
	mb, mdesc, err := next.BuildMailbox()
	if err != nil {
		return nil, err
	}
	var newSender Sender
	sdesc := ""
	if swSender != nil && next.Outbox.Dir != "" {
		newSender, sdesc, err = next.buildSender()
		if err != nil {
			return nil, err
		}
	}

	*cfg = *next
	sw.Swap(mb)
	if newSender != nil {
		swSender.Swap(newSender)
	}

	result := map[string]any{"ok": true, "backend": mdesc}
	if sdesc != "" {
		result["sender"] = sdesc
	}
	if cfg.Path() != "" {
		if err := cfg.Save(); err != nil {
			result["persisted"] = false
			result["persist_error"] = err.Error()
			return result, nil
		}
		result["persisted"] = true
	} else {
		result["persisted"] = false
	}
	return result, nil
}

// applyProfile activates a declared profile by name. Because the profile
// supplies its own endpoint and credentials from the config file, none
// of the runtime guards that constrain field-level patches apply here —
// there is nothing to inherit and no caller-chosen destination. The
// operator already made this choice; the caller only picks from it.
func applyProfile(
	ctx context.Context, name string, confirmed bool, mu *sync.Mutex,
	cfg *Config, sw *Switchable, swSender *SwitchableSender,
) (map[string]any, error) {
	mu.Lock()
	defer mu.Unlock()

	profile, ok := cfg.Profiles[name]
	if !ok {
		known := cfg.ProfileNames()
		if len(known) == 0 {
			return nil, fmt.Errorf("briefkasten: unknown profile %q — none are declared in the config file", name)
		}
		return nil, fmt.Errorf("briefkasten: unknown profile %q (declared: %s)", name, strings.Join(known, ", "))
	}

	if err := mcpserver.ConfirmAction(ctx, confirmed,
		"switch to profile "+name,
		fmt.Sprintf("Switch the mailbox to profile %q? This changes which mailbox is read and which account sends.", name),
	); err != nil {
		return nil, err
	}

	candidate := cfg.applyProfile(profile)
	result, err := commitConfig(&candidate, cfg, sw, swSender)
	if err != nil {
		return nil, err
	}
	result["profile"] = name
	return result, nil
}
