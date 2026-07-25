// Package resilience decorates remote backends with fortify patterns.
package resilience

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/briefkasten/domain"

	"go.klarlabs.de/fortify/circuitbreaker"
	"go.klarlabs.de/fortify/retry"
	"go.klarlabs.de/fortify/timeout"
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
//
// Errors come out in three kinds, and callers can tell them apart:
//
//   - errors.Is(err, context.Canceled) — the caller walked away. The
//     backend was never judged: nothing is retried, and the breaker is
//     not moved.
//   - errors.Is(err, context.DeadlineExceeded) — the call ran out of
//     time, either the caller's deadline or OpTimeout. A per-attempt
//     overrun additionally matches ferrors.ErrTimeout. This does count
//     against the backend: a server that will not answer inside its
//     budget is unhealthy by definition.
//   - anything else — a backend fault, retried and counted.
//
// The timeout is only real because domain.Mailbox takes a context and
// its implementations honour it; a backend that ignored the context
// would be timed out on paper and still hold the caller.
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
				// Caller errors are not backend health signals — neither a
				// bad id nor a caller that hung up. Without the second
				// clause a client that closes its connection five times
				// would open the breaker on every other client's behalf,
				// having learnt nothing about the server at all. A batch
				// the caller shaped badly — malformed, or measured over the
				// fetch budget — says exactly as little about the server:
				// the backend answered the question correctly.
				return err == nil ||
					errors.Is(err, domain.ErrBadID) ||
					errors.Is(err, domain.ErrBulkSize) ||
					errors.Is(err, domain.ErrFetchTooLarge) ||
					errors.Is(err, context.Canceled)
			},
		}),
		rt: retry.New[any](retry.Config{
			MaxAttempts:  cfg.MaxAttempts,
			InitialDelay: cfg.InitialDelay,
			// A call that ran out of time is not a transient failure to
			// paper over: the backend was handed its full budget and did
			// not answer, so a retry buys the caller a second identical
			// hang — three attempts turning one stalled IMAP session into
			// three, with the MCP request (and the human behind it) held
			// for the whole of it. Failures that come back fast — a reset
			// connection, a refused dial — never reach this clause and are
			// still retried. context.Canceled is listed for the same
			// reason from the other direction: nobody is waiting for the
			// answer any more. A batch that is malformed or measured over
			// the fetch budget joins them: it is refused identically on
			// every attempt, so retrying only spends the pre-flight round
			// trip again on the caller's behalf.
			NonRetryableErrors: []error{
				domain.ErrBadID, domain.ErrBulkSize, domain.ErrFetchTooLarge,
				context.DeadlineExceeded, context.Canceled,
			},
			Jitter: true,
		}),
		to: timeout.New[any](timeout.Config{}),
		op: cfg.OpTimeout,
	}
}

// execute runs fn as breaker(retry(timeout(fn))): the breaker sees the
// final outcome after retries, each attempt individually bounded.
//
// The context is the caller's, not a fresh Background one: a deadline
// the caller set, or a cancellation when it gives up, has to reach the
// backend for either to mean anything. fortify's timeout derives the
// per-attempt deadline from it and returns once fn does, so the bound is
// only as real as the backend's own respect for the context — which is
// why domain.Mailbox demands it.
func (r *Mailbox) execute(ctx context.Context, fn func(context.Context) (any, error)) (any, error) {
	return r.cb.Execute(ctx, func(ctx context.Context) (any, error) {
		return r.rt.Execute(ctx, func(ctx context.Context) (any, error) {
			return r.to.Execute(ctx, r.op, fn)
		})
	})
}

// ListUnread lists unread ids through the resilience pipeline.
func (r *Mailbox) ListUnread(ctx context.Context) ([]string, error) {
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return r.mb.ListUnread(ctx) })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Fetch returns raw message bytes through the resilience pipeline.
func (r *Mailbox) Fetch(ctx context.Context, id string) ([]byte, error) {
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return r.mb.Fetch(ctx, id) })
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// FetchMany reads a batch through the resilience pipeline. The whole
// batch is one call through it — one backend operation, one timeout, one
// verdict for the breaker — and without this forward the wrapped
// backend's native batching would be invisible.
func (r *Mailbox) FetchMany(ctx context.Context, ids []string) (domain.FetchResult, error) {
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) {
		return fetchManyFallback(ctx, r.mb, ids)
	})
	if err != nil {
		return domain.FetchResult{}, err
	}
	return v.(domain.FetchResult), nil
}

// Sizes measures a batch through the resilience pipeline. Sizing talks to
// the server, so it deserves the same timeout and breaker as any other
// call.
func (r *Mailbox) Sizes(ctx context.Context, ids []string) (map[string]int64, error) {
	sizer, ok := r.mb.(domain.MessageSizer)
	if !ok {
		return nil, errors.New("briefkasten: backend cannot measure message sizes")
	}
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return sizer.Sizes(ctx, ids) })
	if err != nil {
		return nil, err
	}
	return v.(map[string]int64), nil
}

// MarkSeen acknowledges a message through the resilience pipeline.
func (r *Mailbox) MarkSeen(ctx context.Context, id string) error {
	_, err := r.execute(ctx, func(ctx context.Context) (any, error) { return nil, r.mb.MarkSeen(ctx, id) })
	return err
}

// MarkSeenMany acknowledges a batch through the resilience pipeline.
// The whole batch is one call through the pipeline, which is the point:
// it is one backend operation, so it gets one timeout and counts once
// towards the breaker rather than a hundred times.
func (r *Mailbox) MarkSeenMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return r.bulk(ctx, func(ctx context.Context) (domain.BulkResult, error) {
		return markSeenManyFallback(ctx, r.mb, ids)
	})
}

// ArchiveMany archives a batch through the resilience pipeline. Without
// this forward the wrapped backend's native batching would be invisible
// and every batch would cost one round of commands per message.
func (r *Mailbox) ArchiveMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return r.bulk(ctx, func(ctx context.Context) (domain.BulkResult, error) {
		return curateManyFallback(ctx, r.mb, ids, false)
	})
}

// DeleteMany trashes a batch through the resilience pipeline.
func (r *Mailbox) DeleteMany(ctx context.Context, ids []string) (domain.BulkResult, error) {
	return r.bulk(ctx, func(ctx context.Context) (domain.BulkResult, error) {
		return curateManyFallback(ctx, r.mb, ids, true)
	})
}

// bulk runs a bulk operation through the pipeline. Per-id failures ride
// along inside the result and so are never retried — they are the
// caller's bad ids, not a backend fault, exactly as domain.ErrBadID is
// treated for a single message.
func (r *Mailbox) bulk(
	ctx context.Context, fn func(context.Context) (domain.BulkResult, error),
) (domain.BulkResult, error) {
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return fn(ctx) })
	if err != nil {
		return domain.BulkResult{}, err
	}
	return v.(domain.BulkResult), nil
}

// List lists a scope through the resilience pipeline.
func (r *Mailbox) List(ctx context.Context, scope domain.Scope) ([]string, error) {
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return listFallback(ctx, r.mb, scope) })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Search forwards to the wrapped backend's domain.Searcher (or the generic
// fallback), guarded by the same resilience pipeline.
func (r *Mailbox) Search(ctx context.Context, query string) ([]string, error) {
	return r.SearchScope(ctx, domain.ScopeUnread, query)
}

// SearchScope searches within a scope through the resilience pipeline.
func (r *Mailbox) SearchScope(ctx context.Context, scope domain.Scope, query string) ([]string, error) {
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) {
		return searchFallback(ctx, r.mb, scope, query)
	})
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Folders forwards to the wrapped backend when it supports folders.
func (r *Mailbox) Folders(ctx context.Context) ([]string, error) {
	fm, ok := r.mb.(domain.FolderMailbox)
	if !ok {
		return []string{"INBOX"}, nil
	}
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return fm.Folders(ctx) })
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// InFolder returns a resilience-wrapped folder-scoped instance.
func (r *Mailbox) InFolder(ctx context.Context, name string) (domain.Mailbox, error) {
	fm, ok := r.mb.(domain.FolderMailbox)
	if !ok {
		if name == "INBOX" {
			return r, nil
		}
		return nil, errors.New("briefkasten: backend has no folder support")
	}
	inner, err := fm.InFolder(ctx, name)
	if err != nil {
		return nil, err
	}
	return Wrap(inner, Config{}), nil
}

// Archive forwards to the wrapped backend's domain.Curator through the pipeline.
func (r *Mailbox) Archive(ctx context.Context, id string) error {
	cu, ok := r.mb.(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	_, err := r.execute(ctx, func(ctx context.Context) (any, error) { return nil, cu.Archive(ctx, id) })
	return err
}

// Delete forwards to the wrapped backend's domain.Curator through the pipeline.
func (r *Mailbox) Delete(ctx context.Context, id string) error {
	cu, ok := r.mb.(domain.Curator)
	if !ok {
		return errors.New("briefkasten: backend has no curation support")
	}
	_, err := r.execute(ctx, func(ctx context.Context) (any, error) { return nil, cu.Delete(ctx, id) })
	return err
}

// CurationPlan forwards to the wrapped backend's inspector through the
// pipeline. Resolution talks to the server, so it deserves the same
// timeout and breaker as any other call.
func (r *Mailbox) CurationPlan(ctx context.Context) (domain.CurationPlan, error) {
	ci, ok := r.mb.(domain.CurationInspector)
	if !ok {
		return domain.CurationPlan{}, errors.New("briefkasten: backend cannot report curation destinations")
	}
	v, err := r.execute(ctx, func(ctx context.Context) (any, error) { return ci.CurationPlan(ctx) })
	if err != nil {
		return domain.CurationPlan{}, err
	}
	return v.(domain.CurationPlan), nil
}

var (
	_ domain.Mailbox           = (*Mailbox)(nil)
	_ domain.CurationInspector = (*Mailbox)(nil)
	_ domain.ScopedMailbox     = (*Mailbox)(nil)
	_ domain.Searcher          = (*Mailbox)(nil)
	_ domain.ScopedSearcher    = (*Mailbox)(nil)
	_ domain.FolderMailbox     = (*Mailbox)(nil)
	_ domain.Curator           = (*Mailbox)(nil)
	_ domain.BulkMailbox       = (*Mailbox)(nil)
	_ domain.BulkCurator       = (*Mailbox)(nil)
	_ domain.BulkFetcher       = (*Mailbox)(nil)
	_ domain.MessageSizer      = (*Mailbox)(nil)
)

// listFallback mirrors the application-layer scoped-list fallback for
// the resilience pipeline.
func listFallback(ctx context.Context, mb domain.Mailbox, scope domain.Scope) ([]string, error) {
	if sm, ok := mb.(domain.ScopedMailbox); ok {
		return sm.List(ctx, scope)
	}
	if scope != domain.ScopeUnread {
		return nil, fmt.Errorf("briefkasten: backend lists unread mail only, scope %q unsupported", string(scope))
	}
	return mb.ListUnread(ctx)
}

// fetchManyFallback mirrors the application-layer batched-fetch
// fallback: native batching when the backend has it, one fetch per id
// otherwise — and in both cases the size pre-flight comes first.
func fetchManyFallback(ctx context.Context, mb domain.Mailbox, ids []string) (domain.FetchResult, error) {
	if err := domain.CheckBulkIDs(ids); err != nil {
		return domain.FetchResult{}, err
	}
	if bf, ok := mb.(domain.BulkFetcher); ok {
		return bf.FetchMany(ctx, ids)
	}
	if err := preflightFetchFallback(ctx, mb, ids); err != nil {
		return domain.FetchResult{}, err
	}
	res := domain.NewFetchResult(len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		raw, err := mb.Fetch(ctx, id)
		if err != nil {
			res.Fail(id, err)
			continue
		}
		res.Add(id, raw)
	}
	return res, nil
}

// preflightFetchFallback measures a batch before a byte of it is read. A
// backend that cannot measure cannot have its batch bounded, and is told
// to fetch one id at a time rather than allowed an unbounded response.
func preflightFetchFallback(ctx context.Context, mb domain.Mailbox, ids []string) error {
	sizer, ok := mb.(domain.MessageSizer)
	if !ok {
		return errors.New(
			"briefkasten: backend cannot measure message sizes, so a batch fetch cannot be bounded — fetch ids one at a time")
	}
	sizes, err := sizer.Sizes(ctx, ids)
	if err != nil {
		return err
	}
	var (
		total    int64
		measured int
	)
	for _, id := range ids {
		size, held := sizes[id]
		if !held {
			continue
		}
		total += size
		measured++
	}
	return domain.CheckFetchBudget(total, measured)
}

// markSeenManyFallback mirrors the application-layer bulk fallback:
// native batching when the backend has it, one call per id otherwise.
func markSeenManyFallback(ctx context.Context, mb domain.Mailbox, ids []string) (domain.BulkResult, error) {
	if err := domain.CheckBulkIDs(ids); err != nil {
		return domain.BulkResult{}, err
	}
	if bm, ok := mb.(domain.BulkMailbox); ok {
		return bm.MarkSeenMany(ctx, ids)
	}
	return bulkEachFallback(ctx, ids, mb.MarkSeen)
}

// curateManyFallback mirrors the application-layer bulk curation
// fallback. trash selects delete over archive.
func curateManyFallback(ctx context.Context, mb domain.Mailbox, ids []string, trash bool) (domain.BulkResult, error) {
	if err := domain.CheckBulkIDs(ids); err != nil {
		return domain.BulkResult{}, err
	}
	if bc, ok := mb.(domain.BulkCurator); ok {
		if trash {
			return bc.DeleteMany(ctx, ids)
		}
		return bc.ArchiveMany(ctx, ids)
	}
	cu, ok := mb.(domain.Curator)
	if !ok {
		return domain.BulkResult{}, errors.New("briefkasten: backend has no curation support")
	}
	if trash {
		return bulkEachFallback(ctx, ids, cu.Delete)
	}
	return bulkEachFallback(ctx, ids, cu.Archive)
}

// bulkEachFallback loops a single-message operation over a batch,
// collecting an outcome per id. One bad id fails alone; a caller that
// walked away stops the loop.
func bulkEachFallback(
	ctx context.Context, ids []string, op func(context.Context, string) error,
) (domain.BulkResult, error) {
	res := domain.NewBulkResult(len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := op(ctx, id); err != nil {
			res.Fail(id, err)
			continue
		}
		res.Succeed(id)
	}
	return res, nil
}

// searchFallback mirrors the application-layer search fallback for the
// resilience pipeline.
func searchFallback(ctx context.Context, mb domain.Mailbox, scope domain.Scope, query string) ([]string, error) {
	if s, ok := mb.(domain.ScopedSearcher); ok {
		return s.SearchScope(ctx, scope, query)
	}
	if s, ok := mb.(domain.Searcher); ok && scope == domain.ScopeUnread {
		return s.Search(ctx, query)
	}
	ids, err := listFallback(ctx, mb, scope)
	if err != nil {
		return nil, err
	}
	// The same bound the application-layer fallback applies, enforced
	// again here because this copy is the one that actually runs. A bare
	// backend is normally wrapped by Resilient before the service sees
	// it, and this decorator implements ScopedSearcher — so the service
	// takes its native-search branch into this wrapper and never reaches
	// its own budget check. A bound only one of two identical scans
	// applies is not a bound.
	if err := domain.CheckScanBudget(len(ids), scope); err != nil {
		return nil, err
	}
	needle := []byte(strings.ToLower(query))
	var out []string
	for _, id := range ids {
		raw, err := mb.Fetch(ctx, id)
		if err != nil {
			// A message that cannot be read is skipped, but a scan whose
			// time has run out must stop rather than march through the
			// remaining ids collecting the same error.
			if ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if bytes.Contains(bytes.ToLower(raw), needle) {
			out = append(out, id)
		}
	}
	return out, nil
}
