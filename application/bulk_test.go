package application_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
)

// newBulkDir prepares a maildir holding the named unread messages. The
// maildir backend has no native batching on purpose — local renames have
// no round trips to save — so it exercises the shared fallback, which is
// what every non-batching backend gets.
func newBulkDir(t *testing.T, names ...string) (*maildir.Mailbox, string) {
	t.Helper()
	root := t.TempDir()
	mb, err := maildir.New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, "new", name),
			[]byte("From: a@b.c\r\nSubject: "+name+"\r\n\r\nhallo"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return mb, root
}

func failedIDs(res domain.BulkResult) []string {
	out := make([]string, 0, len(res.Failed))
	for _, f := range res.Failed {
		out = append(out, f.ID)
	}
	return out
}

// The fallback must behave exactly as the IMAP batch does from the
// caller's side: the good ids move, each bad id is reported on its own,
// and the batch as a whole does not fail.
func TestBulkCurationOverFallbackReportsPerID(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
		run  func(*application.Service, []string) (domain.BulkResult, error)
	}{
		{"archive", ".archive", func(svc *application.Service, ids []string) (domain.BulkResult, error) {
			return svc.ArchiveMany(context.Background(), "", "", ids)
		}},
		{"delete", ".trash", func(svc *application.Service, ids []string) (domain.BulkResult, error) {
			return svc.DeleteMany(context.Background(), "", "", ids)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mb, root := newBulkDir(t, "a.eml", "b.eml")
			svc := application.NewService(mb, nil)

			res, err := tc.run(svc, []string{"a.eml", "ghost.eml", "b.eml"})
			if err != nil {
				t.Fatalf("%sMany: %v", tc.name, err)
			}
			if !slices.Equal(res.Succeeded, []string{"a.eml", "b.eml"}) {
				t.Errorf("succeeded = %v, want the two real messages", res.Succeeded)
			}
			if got := failedIDs(res); !slices.Equal(got, []string{"ghost.eml"}) {
				t.Fatalf("failed = %v, want only ghost.eml", got)
			}
			if !errors.Is(res.Failed[0].Err, domain.ErrBadID) {
				t.Errorf("ghost failure = %v, want ErrBadID", res.Failed[0].Err)
			}
			// The moves are real: assert the bytes on disk, not the report.
			for _, name := range []string{"a.eml", "b.eml"} {
				if _, err := os.Stat(filepath.Join(root, tc.dir, "new", name)); err != nil {
					t.Errorf("%s not filed into %s: %v", name, tc.dir, err)
				}
			}
		})
	}
}

// Bulk mark-seen inherits MarkSeen's idempotence: acknowledging read
// mail again succeeds and changes nothing.
func TestBulkMarkSeenIsIdempotent(t *testing.T) {
	mb, _ := newBulkDir(t, "a.eml", "b.eml")
	svc := application.NewService(mb, nil)
	ids := []string{"a.eml", "b.eml"}

	for i := range 2 {
		res, err := svc.MarkSeenMany(t.Context(), "", "", ids)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !slices.Equal(res.Succeeded, ids) || len(res.Failed) != 0 {
			t.Fatalf("pass %d = %v marked, %v failed; want both marked", i, res.Succeeded, failedIDs(res))
		}
	}
	left, err := svc.ListUnread(t.Context(), "", "")
	if err != nil || len(left) != 0 {
		t.Errorf("unread after bulk mark seen = %v, err %v", left, err)
	}
}

// The cap bounds what one human confirmation can authorise, so an
// oversized batch is refused outright — with the cap named, because a
// caller that cannot see the limit cannot split the work.
func TestBulkSizeIsChecked(t *testing.T) {
	mb, _ := newBulkDir(t, "a.eml")
	svc := application.NewService(mb, nil)

	oversized := make([]string, domain.MaxBulkIDs+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("m%d.eml", i)
	}

	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"over the cap", oversized, "100"},
		{"empty", nil, "no ids"},
		{"duplicate", []string{"a.eml", "a.eml"}, "more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, call := range []struct {
				name string
				run  func([]string) (domain.BulkResult, error)
			}{
				{"MarkSeenMany", func(ids []string) (domain.BulkResult, error) {
					return svc.MarkSeenMany(context.Background(), "", "", ids)
				}},
				{"ArchiveMany", func(ids []string) (domain.BulkResult, error) {
					return svc.ArchiveMany(context.Background(), "", "", ids)
				}},
				{"DeleteMany", func(ids []string) (domain.BulkResult, error) {
					return svc.DeleteMany(context.Background(), "", "", ids)
				}},
			} {
				_, err := call.run(tc.ids)
				if !errors.Is(err, domain.ErrBulkSize) {
					t.Errorf("%s = %v, want ErrBulkSize", call.name, err)
				}
				if err != nil && !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s error = %q, want it to mention %q", call.name, err, tc.want)
				}
			}
		})
	}
	// The cap is refused, not trimmed: nothing moved.
	if ids, err := svc.ListUnread(t.Context(), "", ""); err != nil || len(ids) != 1 {
		t.Errorf("mailbox after refused batches = %v, err %v, want it untouched", ids, err)
	}
}

// bulkSpy is a backend with native batching, recording the batch sizes
// it was handed — the only way to see whether a decorator forwarded the
// capability or quietly fell back to one call per id.
type bulkSpy struct {
	*memBox
	batches []int
	singles int
}

func (s *bulkSpy) record(ids []string) domain.BulkResult {
	s.batches = append(s.batches, len(ids))
	res := domain.NewBulkResult(len(ids))
	for _, id := range ids {
		res.Succeed(id)
	}
	return res
}

func (s *bulkSpy) MarkSeenMany(_ context.Context, ids []string) (domain.BulkResult, error) {
	return s.record(ids), nil
}

func (s *bulkSpy) ArchiveMany(_ context.Context, ids []string) (domain.BulkResult, error) {
	return s.record(ids), nil
}

func (s *bulkSpy) DeleteMany(_ context.Context, ids []string) (domain.BulkResult, error) {
	return s.record(ids), nil
}

func (s *bulkSpy) Archive(ctx context.Context, id string) error {
	s.singles++
	return s.memBox.Archive(ctx, id)
}

// A capability that is not forwarded through the decorators vanishes
// behind them, and the batch silently degrades to a call per message —
// the saving disappears with nothing failing to say so.
func TestSwitchableForwardsBulkCapability(t *testing.T) {
	spy := &bulkSpy{memBox: newMemBox(map[string]string{"a.eml": "x", "b.eml": "y", "c.eml": "z"})}
	sw := application.NewSwitchable(spy)
	ids := []string{"a.eml", "b.eml", "c.eml"}

	for _, call := range []struct {
		name string
		run  func() (domain.BulkResult, error)
	}{
		{"MarkSeenMany", func() (domain.BulkResult, error) { return sw.MarkSeenMany(context.Background(), ids) }},
		{"ArchiveMany", func() (domain.BulkResult, error) { return sw.ArchiveMany(context.Background(), ids) }},
		{"DeleteMany", func() (domain.BulkResult, error) { return sw.DeleteMany(context.Background(), ids) }},
	} {
		res, err := call.run()
		if err != nil || len(res.Succeeded) != len(ids) {
			t.Fatalf("%s = %v, err %v", call.name, res, err)
		}
	}
	if !slices.Equal(spy.batches, []int{3, 3, 3}) {
		t.Errorf("backend saw batches %v, want one call of 3 per operation", spy.batches)
	}
	if spy.singles != 0 {
		t.Errorf("backend saw %d single-message curations, want none", spy.singles)
	}

	// A backend without native batching still gets correct per-id
	// behaviour from the shared fallback.
	plain := newMemBox(map[string]string{"a.eml": "x", "b.eml": "y"})
	res, err := application.NewSwitchable(plain).ArchiveMany(t.Context(), []string{"a.eml", "b.eml"})
	if err != nil || len(res.Succeeded) != 2 || !plain.archived["a.eml"] || !plain.archived["b.eml"] {
		t.Errorf("fallback archive = %v, err %v, archived %v", res.Succeeded, err, plain.archived)
	}

	// No curation at all is still a clear refusal, not a silent success.
	bare := application.NewSwitchable(bareBox{newMemBox(map[string]string{"a.eml": "x"})})
	if _, err := bare.ArchiveMany(t.Context(), []string{"a.eml"}); err == nil {
		t.Error("bulk archive on a curatorless backend accepted")
	}
	if _, err := bare.DeleteMany(t.Context(), []string{"a.eml"}); err == nil {
		t.Error("bulk delete on a curatorless backend accepted")
	}
}
