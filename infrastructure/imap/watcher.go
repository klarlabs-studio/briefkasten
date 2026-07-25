package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"go.klarlabs.de/briefkasten/infrastructure/auth"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// idleRefresh re-issues IDLE before servers drop it (RFC 2177 suggests ~29
// minutes); we refresh comfortably inside that window.
const idleRefresh = 25 * time.Minute

// Reconnect backoff bounds. A blip deserves a quick retry, but an outage
// lasting hours must not become a reconnect storm, so the delay doubles up to
// the cap and stays there.
const (
	reconnectMinDelay = 5 * time.Second
	reconnectMaxDelay = 5 * time.Minute
)

// transientResponseCodes are the NO/BAD response codes that describe a passing
// condition — capacity, contention, a server that will come back. Every other
// tagged refusal is the server rejecting this account, this mailbox, or this
// command, and will be repeated verbatim on every reconnect.
var transientResponseCodes = map[imap.ResponseCode]bool{
	imap.ResponseCodeUnavailable: true,
	imap.ResponseCodeInUse:       true,
	imap.ResponseCodeLimit:       true,
	imap.ResponseCodeOverQuota:   true,
	imap.ResponseCodeServerBug:   true,
}

// Watcher reports new mail on an IMAP mailbox using IDLE: it holds a
// connection open and fires onChange whenever the server announces additional
// messages, reconnecting on failure. Implements domain.MailboxWatcher.
type Watcher struct {
	cfg Config

	minDelay    time.Duration
	maxDelay    time.Duration
	retryNotify func(attempt int, delay time.Duration, err error)
}

// NewWatcher builds an IDLE-based watcher for the configured mailbox.
func NewWatcher(cfg Config) *Watcher {
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	return &Watcher{cfg: cfg, minDelay: reconnectMinDelay, maxDelay: reconnectMaxDelay}
}

// SetBackoff overrides the reconnect backoff bounds; non-positive values keep
// the defaults. Tests use it to exercise the growth curve without waiting out
// production delays. Call it before Watch — a running watcher reads these.
func (w *Watcher) SetBackoff(initial, maximum time.Duration) {
	if initial > 0 {
		w.minDelay = initial
	}
	if maximum > 0 {
		w.maxDelay = maximum
	}
	if w.maxDelay < w.minDelay {
		w.maxDelay = w.minDelay
	}
}

// SetRetryNotify registers fn, called before each backoff wait with the failure
// that triggered it. Watch reports nothing until it gives up for good — which
// for a transient outage is never — so without a hook the operator has no way
// to learn that push notifications have gone quiet. Call it before Watch.
func (w *Watcher) SetRetryNotify(fn func(attempt int, delay time.Duration, err error)) {
	w.retryNotify = fn
}

// Watch blocks until ctx is cancelled or the mailbox becomes permanently
// unwatchable, calling onChange each time the server reports new messages.
//
// Two exits, both surfaced through the domain.MailboxWatcher contract's error
// return: ctx.Err() means a clean stop, any other error means the watcher gave
// up because retrying cannot help (see permanentFailure). Callers treat the
// latter as degraded-not-broken — mail stays pollable — but must report it,
// because nothing else will.
//
// Recoverable failures are retried with exponential backoff. A session that
// reached IDLE proves the credentials and mailbox are good, so the next drop
// starts the backoff over rather than inheriting an old outage's delay.
func (w *Watcher) Watch(ctx context.Context, onChange func()) error {
	delay, attempt := w.minDelay, 0
	for {
		connected, err := w.idleSession(ctx, onChange)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if permanentFailure(err) {
			return err
		}
		if connected {
			delay, attempt = w.minDelay, 0
		}
		attempt++
		if w.retryNotify != nil {
			w.retryNotify(attempt, delay, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, w.maxDelay)
	}
}

// permanentFailure reports whether err will recur identically on every
// reconnect: a rejected credential, a mailbox that is not there, a certificate
// this client will never trust. Those need an operator, not a retry — looping
// on them is the silent reconnect storm this classification exists to prevent.
func permanentFailure(err error) bool {
	if err == nil {
		return false
	}
	// TLS trust is decided from our config and the server's chain; dialling
	// again feeds the same inputs to the same verifier. A record-header error
	// means we spoke TLS to a plaintext port, which is a misconfigured Addr.
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &recordErr) {
		return true
	}
	// A tagged NO/BAD means the server understood the command and refused it.
	// BYE is different: that is the server hanging up on us (restart, idle
	// timeout, maintenance), which is exactly what reconnecting is for.
	var imapErr *imap.Error
	if errors.As(err, &imapErr) {
		return imapErr.Type != imap.StatusResponseTypeBye && !transientResponseCodes[imapErr.Code]
	}
	// Everything else is transport — refused dial, DNS, timeout, dropped
	// connection — and is worth another attempt.
	return false
}

// idleSession holds one connection open in IDLE, refreshing periodically,
// until ctx is cancelled or the connection fails. It reports whether the
// session ever reached IDLE, which tells Watch the failure was a drop rather
// than a rejection.
func (w *Watcher) idleSession(ctx context.Context, onChange func()) (bool, error) {
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
	c, err := w.connect(ctx, opts)
	if err != nil {
		return false, err
	}
	// The watcher's own context: once it is cancelled the LOGOUT courtesy
	// is skipped and the socket just closes, which is what a shutdown
	// wants.
	defer closeClient(ctx, c)

	connected := false
	for {
		idleCmd, err := c.Idle()
		if err != nil {
			return connected, fmt.Errorf("imap watch: idle: %w", err)
		}
		connected = true
		timer := time.NewTimer(idleRefresh)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = idleCmd.Close()
			_ = idleCmd.Wait()
			return connected, ctx.Err()
		case <-timer.C:
			if err := idleCmd.Close(); err != nil {
				return connected, fmt.Errorf("imap watch: idle close: %w", err)
			}
			if err := idleCmd.Wait(); err != nil {
				return connected, fmt.Errorf("imap watch: idle wait: %w", err)
			}
		}
	}
}

// connect dials, authenticates, and selects the mailbox with the IDLE-aware
// options. It mirrors Mailbox.dial but keeps the unilateral-data handler.
func (w *Watcher) connect(ctx context.Context, opts *imapclient.Options) (*imapclient.Client, error) {
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
		saslAuth, err := w.cfg.OAuth2.SASLClient(ctx, w.cfg.Username, host, port)
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
