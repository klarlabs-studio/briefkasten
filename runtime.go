package briefkasten

import (
	"context"
	"sync"

	mcp "github.com/felixgeelhaar/mcp-go"
)

// Switchable is a Mailbox whose backend can be swapped at runtime. All
// calls go to the current backend under a read lock; Swap replaces it
// atomically.
type Switchable struct {
	mu sync.RWMutex
	mb Mailbox
}

// NewSwitchable wraps an initial backend.
func NewSwitchable(mb Mailbox) *Switchable {
	return &Switchable{mb: mb}
}

// Swap replaces the backend for all subsequent calls.
func (s *Switchable) Swap(mb Mailbox) {
	s.mu.Lock()
	s.mb = mb
	s.mu.Unlock()
}

func (s *Switchable) current() Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mb
}

func (s *Switchable) ListUnread() ([]string, error) { return s.current().ListUnread() }
func (s *Switchable) Fetch(id string) ([]byte, error) {
	return s.current().Fetch(id)
}
func (s *Switchable) MarkSeen(id string) error { return s.current().MarkSeen(id) }

var _ Mailbox = (*Switchable)(nil)

// NewConfigServer builds the configured backend and serves it behind a
// Switchable. When cfg.RuntimeConfig is enabled, config.get and config.set
// tools allow inspecting and reconfiguring the backend at runtime. When the
// outbox is configured, email.send / email.send_status register and the
// returned Outbox is non-nil — the caller runs its delivery worker.
func NewConfigServer(cfg *Config) (*mcp.Server, *Outbox, error) {
	mb, _, err := cfg.BuildMailbox()
	if err != nil {
		return nil, nil, err
	}
	sw := NewSwitchable(mb)
	srv := NewServer(sw)
	if cfg.RuntimeConfig {
		registerConfigTools(srv, cfg, sw)
	}
	ob, _, err := cfg.BuildOutbox()
	if err != nil {
		return nil, nil, err
	}
	if ob != nil {
		registerSendTools(srv, ob)
	}
	RegisterResources(srv, sw, ob)
	RegisterPrompts(srv, sw)
	return srv, ob, nil
}

// imapPatch carries a partial IMAP settings update; nil/empty fields keep
// current values.
type imapPatch struct {
	Addr     string `json:"addr,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Mailbox  string `json:"mailbox,omitempty"`
	Insecure *bool  `json:"insecure,omitempty"`
}

func registerConfigTools(srv *mcp.Server, cfg *Config, sw *Switchable) {
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
					// password intentionally omitted
				},
			}
			if cfg.Path() != "" {
				out["config_file"] = cfg.Path()
			}
			return out, nil
		})

	srv.Tool("config.set").
		Description("Reconfigure the mailbox backend at runtime. Partial update: omitted fields keep their current values. The new backend is validated before it replaces the old one; when the server was started from a config file the change is persisted there.").
		Destructive().
		Handler(func(_ context.Context, in struct {
			Backend string     `json:"backend,omitempty"`
			Maildir string     `json:"maildir,omitempty"`
			IMAP    *imapPatch `json:"imap,omitempty"`
		}) (map[string]any, error) {
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
			}

			mb, desc, err := next.BuildMailbox()
			if err != nil {
				return nil, err
			}

			*cfg = next
			sw.Swap(mb)

			persisted := false
			if cfg.Path() != "" {
				if err := cfg.Save(); err != nil {
					return map[string]any{"ok": true, "backend": desc, "persisted": false, "persist_error": err.Error()}, nil
				}
				persisted = true
			}
			return map[string]any{"ok": true, "backend": desc, "persisted": persisted}, nil
		})
}
