package briefkasten

import (
	"context"
	"errors"
	"time"

	"github.com/felixgeelhaar/fortify/circuitbreaker"
	"github.com/felixgeelhaar/fortify/retry"
	"github.com/felixgeelhaar/fortify/timeout"
)

// ResilienceConfig tunes the fortify patterns around a remote backend.
// The zero value gives sensible production defaults.
type ResilienceConfig struct {
	// OpTimeout bounds a single backend call (default 30s).
	OpTimeout time.Duration
	// MaxAttempts is the total number of attempts per call, including the
	// first (default 3).
	MaxAttempts int
	// InitialDelay is the delay before the first retry (default 100ms,
	// exponential backoff after that).
	InitialDelay time.Duration
}

// ResilientMailbox decorates a Mailbox with fortify resilience patterns:
// a per-call timeout, retry with exponential backoff for transient
// failures, and a circuit breaker that fast-fails while the backend is
// down. ErrBadID is never retried and never trips the breaker — a bad id
// is the caller's mistake, not a backend fault.
type ResilientMailbox struct {
	mb Mailbox
	cb circuitbreaker.CircuitBreaker[any]
	rt retry.Retry[any]
	to timeout.Timeout[any]
	op time.Duration
}

// Resilient wraps mb with timeout, retry, and circuit breaker.
func Resilient(mb Mailbox, cfg ResilienceConfig) *ResilientMailbox {
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 30 * time.Second
	}
	return &ResilientMailbox{
		mb: mb,
		cb: circuitbreaker.New[any](circuitbreaker.Config{
			IsSuccessful: func(err error) bool {
				// Caller errors are not backend health signals.
				return err == nil || errors.Is(err, ErrBadID)
			},
		}),
		rt: retry.New[any](retry.Config{
			MaxAttempts:        cfg.MaxAttempts,
			InitialDelay:       cfg.InitialDelay,
			NonRetryableErrors: []error{ErrBadID},
			Jitter:             true,
		}),
		to: timeout.New[any](timeout.Config{}),
		op: cfg.OpTimeout,
	}
}

// execute runs fn as breaker(retry(timeout(fn))): the breaker sees the
// final outcome after retries, each attempt individually bounded.
func (r *ResilientMailbox) execute(fn func() (any, error)) (any, error) {
	ctx := context.Background()
	return r.cb.Execute(ctx, func(ctx context.Context) (any, error) {
		return r.rt.Execute(ctx, func(ctx context.Context) (any, error) {
			return r.to.Execute(ctx, r.op, func(context.Context) (any, error) {
				return fn()
			})
		})
	})
}

func (r *ResilientMailbox) ListUnread() ([]string, error) {
	v, err := r.execute(func() (any, error) { return r.mb.ListUnread() })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

func (r *ResilientMailbox) Fetch(id string) ([]byte, error) {
	v, err := r.execute(func() (any, error) { return r.mb.Fetch(id) })
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (r *ResilientMailbox) MarkSeen(id string) error {
	_, err := r.execute(func() (any, error) { return nil, r.mb.MarkSeen(id) })
	return err
}

// Search forwards to the wrapped backend's Searcher (or the generic
// fallback), guarded by the same resilience pipeline.
func (r *ResilientMailbox) Search(query string) ([]string, error) {
	v, err := r.execute(func() (any, error) { return searchMailbox(r.mb, query) })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Folders forwards to the wrapped backend when it supports folders.
func (r *ResilientMailbox) Folders() ([]string, error) {
	fm, ok := r.mb.(FolderMailbox)
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
func (r *ResilientMailbox) InFolder(name string) (Mailbox, error) {
	fm, ok := r.mb.(FolderMailbox)
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
	return Resilient(inner, ResilienceConfig{}), nil
}

// Archive forwards to the wrapped backend's Curator through the pipeline.
func (r *ResilientMailbox) Archive(id string) error {
	cu, ok := r.mb.(Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	_, err := r.execute(func() (any, error) { return nil, cu.Archive(id) })
	return err
}

// Delete forwards to the wrapped backend's Curator through the pipeline.
func (r *ResilientMailbox) Delete(id string) error {
	cu, ok := r.mb.(Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	_, err := r.execute(func() (any, error) { return nil, cu.Delete(id) })
	return err
}

var (
	_ Mailbox       = (*ResilientMailbox)(nil)
	_ Searcher      = (*ResilientMailbox)(nil)
	_ FolderMailbox = (*ResilientMailbox)(nil)
	_ Curator       = (*ResilientMailbox)(nil)
)
