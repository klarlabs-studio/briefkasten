package resilience

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"
)

// bulkMailbox adds native batching, recording the batch sizes it saw so
// a test can tell a forwarded capability from a silent fallback.
type bulkMailbox struct {
	curatedMailbox
	batches []int
}

func (b *bulkMailbox) record(ids []string) domain.BulkResult {
	b.batches = append(b.batches, len(ids))
	res := domain.NewBulkResult(len(ids))
	for _, id := range ids {
		res.Succeed(id)
	}
	return res
}

func (b *bulkMailbox) MarkSeenMany(_ context.Context, ids []string) (domain.BulkResult, error) {
	return b.record(ids), nil
}

func (b *bulkMailbox) ArchiveMany(_ context.Context, ids []string) (domain.BulkResult, error) {
	return b.record(ids), nil
}

func (b *bulkMailbox) DeleteMany(_ context.Context, ids []string) (domain.BulkResult, error) {
	return b.record(ids), nil
}

// The decorator has to forward bulk capabilities or the batching behind
// it becomes invisible: every batch would degrade to a call per message,
// with nothing failing to announce it.
func TestResilientForwardsBulkCapability(t *testing.T) {
	mb := &bulkMailbox{}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})
	ids := []string{"a.eml", "b.eml", "c.eml"}

	for _, call := range []struct {
		name string
		run  func() (domain.BulkResult, error)
	}{
		{"MarkSeenMany", func() (domain.BulkResult, error) { return r.MarkSeenMany(context.Background(), ids) }},
		{"ArchiveMany", func() (domain.BulkResult, error) { return r.ArchiveMany(context.Background(), ids) }},
		{"DeleteMany", func() (domain.BulkResult, error) { return r.DeleteMany(context.Background(), ids) }},
	} {
		res, err := call.run()
		if err != nil || !slices.Equal(res.Succeeded, ids) {
			t.Fatalf("%s = %v, err %v", call.name, res.Succeeded, err)
		}
	}
	if !slices.Equal(mb.batches, []int{3, 3, 3}) {
		t.Errorf("backend saw batches %v, want one call of 3 per operation", mb.batches)
	}
	if len(mb.archived)+len(mb.deleted) != 0 {
		t.Errorf("backend saw single-message curation: archived %v deleted %v", mb.archived, mb.deleted)
	}
}

// Without native batching the pipeline loops, and one unusable id costs
// itself an entry rather than the batch.
func TestResilientBulkFallbackReportsPerID(t *testing.T) {
	mb := &curatedMailbox{}
	r := Wrap(mb, Config{InitialDelay: time.Millisecond})

	res, err := r.ArchiveMany(t.Context(), []string{"a.eml", "b.eml"})
	if err != nil {
		t.Fatalf("ArchiveMany: %v", err)
	}
	if !slices.Equal(mb.archived, []string{"a.eml", "b.eml"}) || len(res.Succeeded) != 2 {
		t.Errorf("archived = %v, result = %v", mb.archived, res.Succeeded)
	}

	// Bulk mark-seen falls back to MarkSeen, whose failure is the
	// caller's bad id: reported for that id, batch intact.
	seen := &stubMailbox{seenErr: fmt.Errorf("%w: escape", domain.ErrBadID)}
	res, err = Wrap(seen, Config{InitialDelay: time.Millisecond}).
		MarkSeenMany(t.Context(), []string{"../etc/passwd", "b.eml"})
	if err != nil {
		t.Fatalf("MarkSeenMany: %v", err)
	}
	if len(res.Failed) != 2 || len(res.Succeeded) != 0 {
		t.Fatalf("result = %v / %v, want both ids reported failed", res.Succeeded, res.Failed)
	}
	if !errors.Is(res.Failed[0].Err, domain.ErrBadID) {
		t.Errorf("failure = %v, want ErrBadID preserved through the pipeline", res.Failed[0].Err)
	}
	// Per-id failures ride inside the result, so the pipeline sees a
	// successful call and never retries them.
	if seen.seenCalls != 2 {
		t.Errorf("backend calls = %d, want 2 (per-id failures are not retried)", seen.seenCalls)
	}
}

// A backend with no curation at all refuses clearly rather than
// reporting an empty batch as done.
func TestResilientBulkWithoutCurator(t *testing.T) {
	r := Wrap(&stubMailbox{}, Config{InitialDelay: time.Millisecond})
	if _, err := r.ArchiveMany(t.Context(), []string{"a.eml"}); err == nil {
		t.Error("bulk archive on a curatorless backend accepted")
	}
	if _, err := r.DeleteMany(t.Context(), []string{"a.eml"}); err == nil {
		t.Error("bulk delete on a curatorless backend accepted")
	}
	if _, err := r.MarkSeenMany(t.Context(), nil); !errors.Is(err, domain.ErrBulkSize) {
		t.Error("empty batch accepted")
	}
}
