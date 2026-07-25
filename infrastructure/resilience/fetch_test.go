package resilience

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"
)

// sizedMailbox adds the measuring capability and counts what it read, so
// a test can tell a pre-flight from a fetch-then-regret.
type sizedMailbox struct {
	stubMailbox
	sizes      map[string]int64
	fetched    []string
	sizeCalls  int
	fetchBatch []int
}

func (s *sizedMailbox) Fetch(ctx context.Context, id string) ([]byte, error) {
	s.fetched = append(s.fetched, id)
	return s.stubMailbox.Fetch(ctx, id)
}

func (s *sizedMailbox) Sizes(_ context.Context, ids []string) (map[string]int64, error) {
	s.sizeCalls++
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		if size, ok := s.sizes[id]; ok {
			out[id] = size
		}
	}
	return out, nil
}

// batchingMailbox adds native batched fetch on top of the measuring one.
type batchingMailbox struct{ *sizedMailbox }

func (b *batchingMailbox) FetchMany(_ context.Context, ids []string) (domain.FetchResult, error) {
	b.fetchBatch = append(b.fetchBatch, len(ids))
	res := domain.NewFetchResult(len(ids))
	for _, id := range ids {
		res.Add(id, b.raws[id])
	}
	return res, nil
}

// The decorator has to forward the batched fetch or the batching behind
// it becomes invisible: every batch would degrade to a round trip per
// message, with nothing failing to announce it.
func TestResilientForwardsFetchCapability(t *testing.T) {
	inner := &sizedMailbox{
		stubMailbox: stubMailbox{raws: map[string][]byte{"a.eml": []byte("x"), "b.eml": []byte("y")}},
		sizes:       map[string]int64{"a.eml": 1, "b.eml": 1},
	}
	mb := &batchingMailbox{sizedMailbox: inner}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})
	ids := []string{"a.eml", "b.eml"}

	res, err := r.FetchMany(t.Context(), ids)
	if err != nil {
		t.Fatalf("FetchMany: %v", err)
	}
	if len(res.Fetched) != 2 {
		t.Fatalf("fetched %d messages, want 2", len(res.Fetched))
	}
	if !slices.Equal(inner.fetchBatch, []int{2}) {
		t.Errorf("backend saw batches %v, want one call of 2", inner.fetchBatch)
	}
	if len(inner.fetched) != 0 {
		t.Errorf("backend saw %d single-message fetches, want none", len(inner.fetched))
	}

	// Sizing forwards too, or a pre-flight above this decorator would
	// have nothing to measure with.
	sizes, err := r.Sizes(t.Context(), ids)
	if err != nil || len(sizes) != 2 {
		t.Errorf("Sizes through the pipeline = %v, err %v", sizes, err)
	}
}

// Without native batching the pipeline pre-flights and loops, and one
// unusable id costs itself an entry rather than the batch.
func TestResilientFetchFallbackReportsPerID(t *testing.T) {
	mb := &sizedMailbox{
		stubMailbox: stubMailbox{
			raws:      map[string][]byte{"a.eml": []byte("x")},
			fetchErrs: map[string]error{"ghost.eml": domain.ErrBadID},
		},
		sizes: map[string]int64{"a.eml": 1},
	}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	res, err := r.FetchMany(t.Context(), []string{"a.eml", "ghost.eml"})
	if err != nil {
		t.Fatalf("FetchMany: %v", err)
	}
	if len(res.Fetched) != 1 || res.Fetched[0].ID != "a.eml" {
		t.Errorf("fetched = %v, want only a.eml", res.Fetched)
	}
	if len(res.Failed) != 1 || !errors.Is(res.Failed[0].Err, domain.ErrBadID) {
		t.Errorf("failed = %v, want ghost.eml as ErrBadID", res.Failed)
	}
	if mb.sizeCalls != 1 {
		t.Errorf("pre-flight ran %d times, want 1", mb.sizeCalls)
	}
	// Per-id failures ride inside the result, so the pipeline sees a
	// successful call and never retries them.
	if !slices.Equal(mb.fetched, []string{"a.eml", "ghost.eml"}) {
		t.Errorf("backend fetches = %v, want one attempt each", mb.fetched)
	}
}

// An oversized batch is refused through the pipeline before any body is
// read — and, being the caller's mistake rather than a backend fault, it
// is not retried three times over.
func TestResilientFetchBudgetRefusedAndNotRetried(t *testing.T) {
	mb := &sizedMailbox{
		stubMailbox: stubMailbox{raws: map[string][]byte{"a.eml": []byte("x"), "b.eml": []byte("y")}},
		sizes:       map[string]int64{"a.eml": domain.MaxFetchBytes, "b.eml": 1},
	}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	_, err := r.FetchMany(t.Context(), []string{"a.eml", "b.eml"})
	if !errors.Is(err, domain.ErrFetchTooLarge) {
		t.Fatalf("oversized FetchMany = %v, want ErrFetchTooLarge", err)
	}
	if !strings.Contains(err.Error(), "25 MiB") {
		t.Errorf("refusal = %q, want it to name the budget", err)
	}
	if len(mb.fetched) != 0 {
		t.Errorf("backend read %v before the refusal", mb.fetched)
	}
	if mb.sizeCalls != 1 {
		t.Errorf("pre-flight ran %d times, want 1 — a caller's oversized batch is not a transient fault", mb.sizeCalls)
	}
}

// A backend that cannot measure refuses clearly rather than serving an
// unbounded response.
func TestResilientFetchWithoutSizer(t *testing.T) {
	r := Wrap(&stubMailbox{}, Config{InitialDelay: time.Millisecond})
	if _, err := r.FetchMany(t.Context(), []string{"a.eml", "b.eml"}); err == nil ||
		!strings.Contains(err.Error(), "measure") {
		t.Errorf("batch fetch over an unmeasurable backend = %v, want a refusal", err)
	}
	if _, err := r.Sizes(t.Context(), []string{"a.eml"}); err == nil {
		t.Error("Sizes over a backend that cannot measure accepted")
	}
	if _, err := r.FetchMany(t.Context(), nil); !errors.Is(err, domain.ErrBulkSize) {
		t.Error("empty batch accepted")
	}
}
