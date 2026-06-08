package imap

import (
	"context"
	"fmt"
	"time"

	"go.klarlabs.de/briefkasten/infrastructure/auth"

	"github.com/emersion/go-imap/v2/imapclient"
)

// idleRefresh re-issues IDLE before servers drop it (RFC 2177 suggests ~29
// minutes); we refresh comfortably inside that window.
const idleRefresh = 25 * time.Minute

// reconnectDelay backs off before re-dialing after a dropped connection so a
// flapping server is not hammered.
const reconnectDelay = 5 * time.Second

// Watcher reports new mail on an IMAP mailbox using IDLE: it holds a
// connection open and fires onChange whenever the server announces additional
// messages, reconnecting on failure. Implements domain.MailboxWatcher.
type Watcher struct {
	cfg Config
}

// NewWatcher builds an IDLE-based watcher for the configured mailbox.
func NewWatcher(cfg Config) *Watcher {
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	return &Watcher{cfg: cfg}
}

// Watch blocks until ctx is cancelled, calling onChange each time the server
// reports new messages. A dropped session is retried after reconnectDelay.
func (w *Watcher) Watch(ctx context.Context, onChange func()) error {
	for {
		_ = w.idleSession(ctx, onChange)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// idleSession holds one connection open in IDLE, refreshing periodically,
// until ctx is cancelled or the connection fails.
func (w *Watcher) idleSession(ctx context.Context, onChange func()) error {
	opts := &imapclient.Options{
		TLSConfig: w.cfg.TLSConfig,
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				// A non-nil NumMessages means the mailbox grew — new mail.
				if d.NumMessages != nil {
					onChange()
				}
			},
		},
	}
	c, err := w.connect(opts)
	if err != nil {
		return err
	}
	defer closeClient(c)

	for {
		idleCmd, err := c.Idle()
		if err != nil {
			return fmt.Errorf("imap watch: idle: %w", err)
		}
		timer := time.NewTimer(idleRefresh)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = idleCmd.Close()
			_ = idleCmd.Wait()
			return ctx.Err()
		case <-timer.C:
			if err := idleCmd.Close(); err != nil {
				return fmt.Errorf("imap watch: idle close: %w", err)
			}
			if err := idleCmd.Wait(); err != nil {
				return fmt.Errorf("imap watch: idle wait: %w", err)
			}
		}
	}
}

// connect dials, authenticates, and selects the mailbox with the IDLE-aware
// options. It mirrors Mailbox.dial but keeps the unilateral-data handler.
func (w *Watcher) connect(opts *imapclient.Options) (*imapclient.Client, error) {
	var (
		c   *imapclient.Client
		err error
	)
	if w.cfg.Insecure {
		c, err = imapclient.DialInsecure(w.cfg.Addr, opts)
	} else {
		c, err = imapclient.DialTLS(w.cfg.Addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("imap watch: dial %s: %w", w.cfg.Addr, err)
	}
	if w.cfg.OAuth2 != nil {
		host, port := auth.SplitHostPort(w.cfg.Addr, 993)
		saslAuth, err := w.cfg.OAuth2.SASLClient(context.Background(), w.cfg.Username, host, port)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		if err := c.Authenticate(saslAuth); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imap watch: authenticate: %w", err)
		}
	} else if err := c.Login(w.cfg.Username, w.cfg.Password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap watch: login: %w", err)
	}
	if _, err := c.Select(w.cfg.Mailbox, nil).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap watch: select %s: %w", w.cfg.Mailbox, err)
	}
	return c, nil
}
