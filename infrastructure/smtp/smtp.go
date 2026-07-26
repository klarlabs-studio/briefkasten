// Package smtp is the SMTP outbound transport (go-smtp), fortify-wrapped.
package smtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/auth"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"go.klarlabs.de/fortify/retry"
	"go.klarlabs.de/fortify/timeout"
)

// Config configures an Sender.
type Config struct {
	// Addr is the SMTP server address (host:port). Required.
	Addr string
	// From is the envelope and header sender. Required.
	From string
	// Username and Password authenticate via SASL PLAIN when set.
	Username string
	Password string
	// ImplicitTLS dials TLS directly (port 465 style). Default is
	// STARTTLS on a plain connection (port 587 style).
	ImplicitTLS bool
	// Insecure skips TLS entirely. For tests and local servers only.
	Insecure bool
	// TLSConfig optionally overrides the TLS client configuration.
	TLSConfig *tls.Config
	// OAuth2 switches authentication from SASL PLAIN to
	// XOAUTH2/OAUTHBEARER.
	OAuth2 *auth.OAuth2Settings
}

// errPermanent marks a failure that recurs identically on every attempt:
// a 5xx SMTP reply, or a message that cannot be rendered at all. Retrying
// those burns the outbox's attempts for nothing and makes repeated
// rejections look like abuse to the receiving server.
var errPermanent = errors.New("permanent failure")

// permanent tags err as non-retryable.
func permanent(err error) error {
	return fmt.Errorf("%w: %w", errPermanent, err)
}

// classify tags 5xx SMTP replies as permanent. 4xx replies and transport
// errors stay retryable — RFC 5321 reserves 4xx for "come back later".
func classify(err error) error {
	var reply *smtp.SMTPError
	if errors.As(err, &reply) && reply.Code >= 500 && reply.Code < 600 {
		return permanent(err)
	}
	return err
}

// Sender delivers outbound messages over SMTP (go-smtp), wrapped in
// fortify resilience: per-attempt timeout and exponential-backoff retry —
// transient SMTP failures (451) recover without reaching the outbox's
// failed state, while permanent rejections and faults raised after the
// server took the message never cause a second delivery.
type Sender struct {
	cfg Config
	rt  retry.Retry[any]
	to  timeout.Timeout[any]
}

// NewSender validates the config and builds the sender.
func NewSender(cfg Config) (*Sender, error) {
	if cfg.Addr == "" {
		return nil, errors.New("smtp: Addr is required")
	}
	if cfg.From == "" {
		return nil, errors.New("smtp: From is required")
	}
	if err := domain.ValidateAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("smtp: %w", err)
	}
	return &Sender{
		cfg: cfg,
		rt: retry.New[any](retry.Config{
			MaxAttempts:        3,
			InitialDelay:       200 * time.Millisecond,
			NonRetryableErrors: []error{errPermanent},
			Jitter:             true,
		}),
		to: timeout.New[any](timeout.Config{}),
	}, nil
}

// Send delivers the message, retrying transient failures.
func (s *Sender) Send(ctx context.Context, msg domain.OutboundMessage) error {
	var accepted bool
	_, err := s.rt.Execute(ctx, func(ctx context.Context) (any, error) {
		_, err := s.to.Execute(ctx, 30*time.Second, func(ctx context.Context) (any, error) {
			var derr error
			accepted, derr = s.deliver(ctx, msg)
			return nil, derr
		})
		if err != nil && accepted {
			// The operation deadline is checked after deliver returns, so it
			// can fire on a message the server already holds. Reporting it
			// would retry mail that is on its way and deliver it twice.
			return nil, nil
		}
		return nil, err
	})
	return err
}

// deliver runs one delivery attempt. The bool reports whether the server
// acknowledged the message; once it is true the send is finished and must
// never be attempted again, whatever happens afterwards.
func (s *Sender) deliver(ctx context.Context, msg domain.OutboundMessage) (bool, error) {
	// Render first: an unrenderable message fails the same way every time,
	// and there is no reason to open a connection to find that out.
	raw, err := domain.RenderRFC5322(s.cfg.From, msg, time.Now())
	if err != nil {
		return false, fmt.Errorf("smtp render: %w", permanent(err))
	}

	c, err := s.dial()
	if err != nil {
		return false, fmt.Errorf("smtp dial %s: %w", s.cfg.Addr, err)
	}
	defer func() { _ = c.Close() }()

	if s.cfg.OAuth2 != nil {
		host, port := auth.SplitHostPort(s.cfg.Addr, 587)
		saslAuth, err := s.cfg.OAuth2.SASLClient(ctx, s.cfg.Username, host, port)
		if err != nil {
			return false, err
		}
		if err := c.Auth(saslAuth); err != nil {
			return false, fmt.Errorf("smtp auth: %w", classify(err))
		}
	} else if s.cfg.Username != "" {
		plainAuth := sasl.NewPlainClient("", s.cfg.Username, s.cfg.Password)
		if err := c.Auth(plainAuth); err != nil {
			return false, fmt.Errorf("smtp auth: %w", classify(err))
		}
	}

	// SendMail returns only after the reply to end-of-DATA, so any error it
	// yields — including one raised mid-body — happened while the message was
	// still ours to resend.
	//
	// The recipient list is the envelope's, not the header's: Cc and Bcc
	// recipients only ever receive the message because their addresses
	// are given here, and a Bcc appears nowhere else at all.
	if err := c.SendMail(s.cfg.From, msg.Recipients(), bytes.NewReader(raw)); err != nil {
		return false, fmt.Errorf("smtp send: %w", classify(err))
	}

	// Past here the server has taken responsibility for the message. QUIT is
	// courtesy teardown: a connection lost while saying goodbye tells us
	// nothing about the delivery, and reporting it would send the mail again.
	_ = c.Quit()
	return true, nil
}

func (s *Sender) dial() (*smtp.Client, error) {
	switch {
	case s.cfg.Insecure:
		return smtp.Dial(s.cfg.Addr)
	case s.cfg.ImplicitTLS:
		return smtp.DialTLS(s.cfg.Addr, s.cfg.TLSConfig)
	default:
		return smtp.DialStartTLS(s.cfg.Addr, s.cfg.TLSConfig)
	}
}

// From reports the envelope and header sender — the address a reply
// derived here must never send to. See domain.SelfAddresser.
func (s *Sender) From() string { return s.cfg.From }

var (
	_ domain.Sender        = (*Sender)(nil)
	_ domain.SelfAddresser = (*Sender)(nil)
)
