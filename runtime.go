package briefkasten

import (
	"context"
	"sync"

	mcp "go.klarlabs.de/mcp"
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
// current values.
type imapPatch struct {
	Addr     string       `json:"addr,omitempty"`
	Username string       `json:"username,omitempty"`
	Password string       `json:"password,omitempty"`
	Mailbox  string       `json:"mailbox,omitempty"`
	Insecure *bool        `json:"insecure,omitempty"`
	OAuth2   *oauth2Patch `json:"oauth2,omitempty"`
}

// smtpPatch carries a partial SMTP settings update.
type smtpPatch struct {
	Addr        string       `json:"addr,omitempty"`
	Username    string       `json:"username,omitempty"`
	Password    string       `json:"password,omitempty"`
	ImplicitTLS *bool        `json:"implicit_tls,omitempty"`
	Insecure    *bool        `json:"insecure,omitempty"`
	OAuth2      *oauth2Patch `json:"oauth2,omitempty"`
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

	srv.Tool("config.get").
		Description("Inspect the active mailbox configuration. Credentials are redacted.").
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
			}
			if cfg.Path() != "" {
				out["config_file"] = cfg.Path()
			}
			return out, nil
		})

	srv.Tool("config.set").
		Description("Reconfigure the mailbox and outbound sender at runtime — including OAuth2 credentials (oauth2.credentials_file) for Gmail/Outlook. Partial update: omitted fields keep their current values. The new backend and sender are validated before they replace the old ones; when started from a config file the change is persisted there. No restart required.").
		Destructive().
		Handler(func(_ context.Context, in struct {
			Backend string       `json:"backend,omitempty"`
			Maildir string       `json:"maildir,omitempty"`
			IMAP    *imapPatch   `json:"imap,omitempty"`
			Outbox  *outboxPatch `json:"outbox,omitempty"`
		},
		) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()

			// Merge the patch onto a copy; only a successful build mutates cfg.
			next := *cfg
			if in.Backend != "" {
				next.Backend = in.Backend
			}
			if in.Maildir != "" {
				next.Maildir = in.Maildir
			}
			if in.IMAP != nil {
				if in.IMAP.Addr != "" {
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
					next.IMAP.Insecure = *in.IMAP.Insecure
				}
				if in.IMAP.OAuth2 != nil {
					next.IMAP.OAuth2 = applyOAuth2Patch(cfg.IMAP.OAuth2, in.IMAP.OAuth2)
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
					if sp.Addr != "" {
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
						next.Outbox.SMTP.Insecure = *sp.Insecure
					}
					if sp.OAuth2 != nil {
						next.Outbox.SMTP.OAuth2 = applyOAuth2Patch(cfg.Outbox.SMTP.OAuth2, sp.OAuth2)
					}
				}
			}

			// Validate both the mailbox and (when sending) the sender BEFORE
			// swapping either — a bad patch leaves the running config untouched.
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

			*cfg = next
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
		})
}
