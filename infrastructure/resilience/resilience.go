// Package resilience decorates remote backends with fortify patterns.
package resilience

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/felixgeelhaar/briefkasten/domain"

	"github.com/felixgeelhaar/fortify/circuitbreaker"
	"github.com/felixgeelhaar/fortify/retry"
	"github.com/felixgeelhaar/fortify/timeout"
)

// Config tunes the fortify patterns around a remote backend.
// The zero value gives sensible production defaults.
type Config struct {
	// OpTimeout bounds a single backend call (default 30s).
	OpTimeout time.Duration
	// MaxAttempts is the total number of attempts per call, including the
	// first (default 3).
	MaxAttempts int
	// InitialDelay is the delay before the first retry (default 100ms,
	// exponential backoff after that).
	InitialDelay time.Duration
}

// Mailbox decorates a Mailbox with fortify resilience patterns:
// a per-call timeout, retry with exponential backoff for transient
// failures, and a circuit breaker that fast-fails while the backend is
// down. domain.ErrBadID is never retried and never trips the breaker — a bad id
// is the caller's mistake, not a backend fault.
type Mailbox struct {
	mb domain.Mailbox
	cb circuitbreaker.CircuitBreaker[any]
	rt retry.Retry[any]
	to timeout.Timeout[any]
	op time.Duration
}

// Wrap decorates mb with timeout, retry, and circuit breaker.
func Wrap(mb domain.Mailbox, cfg Config) *Mailbox {
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 30 * time.Second
	}
	return &Mailbox{
		mb: mb,
		cb: circuitbreaker.New[any](circuitbreaker.Config{
			IsSuccessful: func(err error) bool {
				// Caller errors are not backend health signals.
				return err == nil || errors.Is(err, domain.ErrBadID)
			},
		}),
		rt: retry.New[any](retry.Config{
			MaxAttempts:        cfg.MaxAttempts,
			InitialDelay:       cfg.InitialDelay,
			NonRetryableErrors: []error{domain.ErrBadID},
			Jitter:             true,
		}),
		to: timeout.New[any](timeout.Config{}),
		op: cfg.OpTimeout,
	}
}

// execute runs fn as breaker(retry(timeout(fn))): the breaker sees the
// final outcome after retries, each attempt individually bounded.
func (r *Mailbox) execute(fn func() (any, error)) (any, error) {
	ctx := context.Background()
	return r.cb.Execute(ctx, func(ctx context.Context) (any, error) {
		return r.rt.Execute(ctx, func(ctx context.Context) (any, error) {
			return r.to.Execute(ctx, r.op, func(context.Context) (any, error) {
				return fn()
			})
		})
	})
}

// ListUnread lists unread ids through the resilience pipeline.
func (r *Mailbox) ListUnread() ([]string, error) {
	v, err := r.execute(func() (any, error) { return r.mb.ListUnread() })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Fetch returns raw message bytes through the resilience pipeline.
func (r *Mailbox) Fetch(id string) ([]byte, error) {
	v, err := r.execute(func() (any, error) { return r.mb.Fetch(id) })
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// MarkSeen acknowledges a message through the resilience pipeline.
func (r *Mailbox) MarkSeen(id string) error {
	_, err := r.execute(func() (any, error) { return nil, r.mb.MarkSeen(id) })
	return err
}

// Search forwards to the wrapped backend's domain.Searcher (or the generic
// fallback), guarded by the same resilience pipeline.
func (r *Mailbox) Search(query string) ([]string, error) {
	v, err := r.execute(func() (any, error) { return searchFallback(r.mb, query) })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Folders forwards to the wrapped backend when it supports folders.
func (r *Mailbox) Folders() ([]string, error) {
	fm, ok := r.mb.(domain.FolderMailbox)
	if !ok {
		return []string{"INBOX"}, nil
	}
	v, err := r.execute(func() (any, error) { return fm.Folders() })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// InFolder returns a resilience-wrapped folder-scoped instance.
func (r *Mailbox) InFolder(name string) (domain.Mailbox, error) {
	fm, ok := r.mb.(domain.FolderMailbox)
	if !ok {
		if name == "INBOX" {
			return r, nil
		}
		return nil, errors.New("briefkasten: backend has no folder support")
	}
	inner, err := fm.InFolder(name)
	if err != nil {
		return nil, err
	}
	return Wrap(inner, Config{}), nil
}

// Archive forwards to the wrapped backend's domain.Curator through the pipeline.
func (r *Mailbox) Archive(id string) error {
	cu, ok := r.mb.(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	_, err := r.execute(func() (any, error) { return nil, cu.Archive(id) })
	return err
}

// Delete forwards to the wrapped backend's domain.Curator through the pipeline.
func (r *Mailbox) Delete(id string) error {
	cu, ok := r.mb.(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	_, err := r.execute(func() (any, error) { return nil, cu.Delete(id) })
	return err
}

var (
	_ domain.Mailbox       = (*Mailbox)(nil)
	_ domain.Searcher      = (*Mailbox)(nil)
	_ domain.FolderMailbox = (*Mailbox)(nil)
	_ domain.Curator       = (*Mailbox)(nil)
)

// searchFallback mirrors the application-layer search fallback for the
// resilience pipeline.
func searchFallback(mb domain.Mailbox, query string) ([]string, error) {
	if s, ok := mb.(domain.Searcher); ok {
		return s.Search(query)
	}
	ids, err := mb.ListUnread()
	if err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := mb.Fetch(id)
		if err != nil {
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}
