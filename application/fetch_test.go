package application_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// sizedBox is a backend that can measure and that counts what it read.
// The count is the whole point: the size guard's job is to refuse before
// a body is touched, and only a reader that keeps score can show it did.
type sizedBox struct {
	*memBox
	fetched   []string
	sizeCalls int
}

func newSizedBox(msgs map[string]string) *sizedBox {
	return &sizedBox{memBox: newMemBox(msgs)}
}

func (s *sizedBox) Fetch(ctx context.Context, id string) ([]byte, error) {
	s.fetched = append(s.fetched, id)
	return s.memBox.Fetch(ctx, id)
}

func (s *sizedBox) Sizes(_ context.Context, ids []string) (map[string]int64, error) {
	s.sizeCalls++
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		if raw, ok := s.msgs[id]; ok {
			out[id] = int64(len(raw))
		}
	}
	return out, nil
}

// bigMessages builds count messages of size bytes each, keyed m0..mN.
func bigMessages(count, size int) map[string]string {
	msgs := make(map[string]string, count)
	for i := range count {
		msgs["m"+strconv.Itoa(i)+".eml"] = strings.Repeat("x", size)
	}
	return msgs
}

func idsOf(msgs map[string]string) []string {
	ids := make([]string, 0, len(msgs))
	for id := range msgs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func fetchedIDs(res domain.FetchResult) []string {
	out := make([]string, 0, len(res.Fetched))
	for _, m := range res.Fetched {
		out = append(out, m.ID)
	}
	return out
}

func fetchFailedIDs(res domain.FetchResult) []string {
	out := make([]string, 0, len(res.Failed))
	for _, f := range res.Failed {
		out = append(out, f.ID)
	}
	return out
}

// A batch over the budget must be refused before a body is read. Only
// asserting the error would pass over an implementation that fetched
// everything first and then thought better of it — which has already
// spent the memory the budget exists to protect.
func TestFetchBudgetRefusesBeforeReadingBodies(t *testing.T) {
	// Four messages of 8 MiB: 32 MiB, over the 25 MiB budget.
	const each = 8 << 20
	msgs := bigMessages(4, each)
	box := newSizedBox(msgs)
	svc := application.NewService(box, nil)
	ids := idsOf(msgs)

	res, err := svc.ReadMany(t.Context(), "", "", ids)
	if !errors.Is(err, domain.ErrFetchTooLarge) {
		t.Fatalf("oversized ReadMany = %v, want ErrFetchTooLarge", err)
	}
	if len(box.fetched) != 0 {
		t.Errorf("backend read %v before refusing — the guard must fire on the measurement, not the bytes", box.fetched)
	}
	if box.sizeCalls != 1 {
		t.Errorf("size pre-flight ran %d times, want exactly 1", box.sizeCalls)
	}
	if len(res.Fetched) != 0 {
		t.Errorf("refused batch returned %d messages", len(res.Fetched))
	}
	// The refusal has to be actionable: the budget, the measured total,
	// and how many ids it covers.
	for _, want := range []string{"25 MiB", strconv.Itoa(4 * each), "4 ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
}

// Just under the budget is a batch the caller is entitled to, whole and
// untruncated.
func TestFetchBudgetAcceptsBatchUnderBudget(t *testing.T) {
	const each = 8 << 20 // three of these is 24 MiB
	msgs := bigMessages(3, each)
	box := newSizedBox(msgs)
	svc := application.NewService(box, nil)
	ids := idsOf(msgs)

	res, err := svc.ReadMany(t.Context(), "", "", ids)
	if err != nil {
		t.Fatalf("ReadMany just under the budget: %v", err)
	}
	if !slices.Equal(fetchedIDs(res), ids) || len(res.Failed) != 0 {
		t.Fatalf("fetched = %v, failed = %v; want all three", fetchedIDs(res), fetchFailedIDs(res))
	}
	for _, m := range res.Fetched {
		if len(m.Raw) != each {
			t.Errorf("message %s is %d bytes, want %d — a batch inside the budget is never trimmed", m.ID, len(m.Raw), each)
		}
	}
}

// The shared fallback must behave exactly as a native batch does from
// the caller's side: the readable ids come back with their bytes, each
// unusable id is reported on its own, and the batch does not fail.
func TestFetchFallbackReportsPerID(t *testing.T) {
	mb, root := newBulkDir(t, "a.eml", "b.eml")
	svc := application.NewService(mb, nil)

	res, err := svc.ReadMany(t.Context(), "", "", []string{"a.eml", "ghost.eml", "b.eml"})
	if err != nil {
		t.Fatalf("ReadMany: %v", err)
	}
	if !slices.Equal(fetchedIDs(res), []string{"a.eml", "b.eml"}) {
		t.Errorf("fetched = %v, want the two real messages", fetchedIDs(res))
	}
	if got := fetchFailedIDs(res); !slices.Equal(got, []string{"ghost.eml"}) {
		t.Fatalf("failed = %v, want only ghost.eml", got)
	}
	if !errors.Is(res.Failed[0].Err, domain.ErrBadID) {
		t.Errorf("ghost failure = %v, want ErrBadID", res.Failed[0].Err)
	}
	// The bytes are the file's, not a summary of it.
	for _, m := range res.Fetched {
		want, err := os.ReadFile(filepath.Join(root, "new", m.ID))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(m.Raw, want) {
			t.Errorf("%s = %q, want the file's contents", m.ID, m.Raw)
		}
	}
	// Reading never changes state: both are still unread.
	if left, err := svc.ListUnread(t.Context(), "", ""); err != nil || len(left) != 2 {
		t.Errorf("unread after a batch fetch = %v, err %v, want both still unread", left, err)
	}
}

// The batch rules are the same rules: one call is capped, and a repeated
// id would make its own outcome ambiguous.
func TestFetchBatchSizeIsChecked(t *testing.T) {
	mb, _ := newBulkDir(t, "a.eml")
	svc := application.NewService(mb, nil)

	oversized := make([]string, domain.MaxBulkIDs+1)
	for i := range oversized {
		oversized[i] = "m" + strconv.Itoa(i) + ".eml"
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
			_, err := svc.ReadMany(context.Background(), "", "", tc.ids)
			if !errors.Is(err, domain.ErrBulkSize) {
				t.Fatalf("ReadMany = %v, want ErrBulkSize", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A backend that cannot say how big its messages are cannot have a batch
// bounded, and must say so rather than serve an unbounded response. The
// single-message fetch is unaffected — that is the way through.
func TestFetchRefusesUnmeasurableBackend(t *testing.T) {
	box := bareBox{newMemBox(map[string]string{"a.eml": "hallo"})}
	svc := application.NewService(box, nil)

	_, err := svc.ReadMany(t.Context(), "", "", []string{"a.eml", "b.eml"})
	if err == nil || !strings.Contains(err.Error(), "measure") {
		t.Errorf("batch over an unmeasurable backend = %v, want a refusal naming the missing capability", err)
	}
	if raw, err := svc.Read(t.Context(), "", "", "a.eml"); err != nil || string(raw) != "hallo" {
		t.Errorf("single fetch = %q, err %v; want it unaffected", raw, err)
	}
}

// fetchSpy is a backend with native batched fetch, recording the batch
// sizes it was handed — the only way to see whether a decorator
// forwarded the capability or quietly fell back to one call per id.
type fetchSpy struct {
	*sizedBox
	batches []int
}

func (f *fetchSpy) FetchMany(_ context.Context, ids []string) (domain.FetchResult, error) {
	f.batches = append(f.batches, len(ids))
	res := domain.NewFetchResult(len(ids))
	for _, id := range ids {
		raw, ok := f.msgs[id]
		if !ok {
			res.Fail(id, domain.ErrBadID)
			continue
		}
		res.Add(id, []byte(raw))
	}
	return res, nil
}

// A capability that is not forwarded through the decorators vanishes
// behind them, and the batch silently degrades to a call per message —
// the saving disappears with nothing failing to say so.
func TestSwitchableForwardsFetchCapability(t *testing.T) {
	spy := &fetchSpy{sizedBox: newSizedBox(map[string]string{"a.eml": "x", "b.eml": "y", "c.eml": "z"})}
	sw := application.NewSwitchable(spy)
	ids := []string{"a.eml", "b.eml", "c.eml"}

	res, err := sw.FetchMany(t.Context(), ids)
	if err != nil || !slices.Equal(fetchedIDs(res), ids) {
		t.Fatalf("FetchMany = %v, err %v", fetchedIDs(res), err)
	}
	if !slices.Equal(spy.batches, []int{3}) {
		t.Errorf("backend saw batches %v, want one call of 3", spy.batches)
	}
	if len(spy.fetched) != 0 {
		t.Errorf("backend saw %d single-message fetches, want none", len(spy.fetched))
	}

	// Sizing forwards too, or the pre-flight on top of a decorator would
	// have nothing to measure with.
	sizes, err := sw.Sizes(t.Context(), ids)
	if err != nil || len(sizes) != 3 {
		t.Errorf("Sizes through the decorator = %v, err %v", sizes, err)
	}

	// A backend without native batching still gets the pre-flight and
	// correct per-id behaviour from the shared fallback.
	plain := newSizedBox(map[string]string{"a.eml": "x", "b.eml": "y"})
	res, err = application.NewSwitchable(plain).FetchMany(t.Context(), []string{"a.eml", "ghost.eml"})
	if err != nil {
		t.Fatalf("fallback FetchMany: %v", err)
	}
	if !slices.Equal(fetchedIDs(res), []string{"a.eml"}) || len(res.Failed) != 1 {
		t.Errorf("fallback = %v fetched, %v failed", fetchedIDs(res), fetchFailedIDs(res))
	}
	if plain.sizeCalls != 1 {
		t.Errorf("fallback ran the pre-flight %d times, want 1", plain.sizeCalls)
	}

	// A backend that cannot measure is refused through the decorator too.
	bare := application.NewSwitchable(bareBox{newMemBox(map[string]string{"a.eml": "x"})})
	if _, err := bare.FetchMany(t.Context(), []string{"a.eml", "b.eml"}); err == nil {
		t.Error("batch fetch over an unmeasurable backend accepted")
	}
}
