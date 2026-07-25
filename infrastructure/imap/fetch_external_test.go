package imap_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
	"go.klarlabs.de/briefkasten/infrastructure/maildir"
)

// fetchedIDs renders a result's messages as ids, for comparison.
func fetchedIDs(res domain.FetchResult) []string {
	out := make([]string, 0, len(res.Fetched))
	for _, m := range res.Fetched {
		out = append(out, m.ID)
	}
	return out
}

// fetchFailedIDs renders a fetch result's failures as ids.
func fetchFailedIDs(res domain.FetchResult) []string {
	out := make([]string, 0, len(res.Failed))
	for _, f := range res.Failed {
		out = append(out, f.ID)
	}
	return out
}

// message builds a message of roughly size bytes, so a test can control
// what a batch weighs.
func message(subject string, size int) []byte {
	head := fmt.Sprintf("From: a@b.c\r\nSubject: %s\r\n\r\n", subject)
	if size <= len(head) {
		return []byte(head)
	}
	return []byte(head + strings.Repeat("x", size-len(head)))
}

// The batching is the whole point: one command carries every body,
// whatever the size of the set. Asserting the bytes came back would pass
// just as happily over a loop — which is the implementation this exists
// to prevent.
func TestIMAPBulkFetchIssuesOneBodyFetch(t *testing.T) {
	const batch = 50
	addr, ln := startSeededIMAPServer(t, batch)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)
	if len(ids) != batch {
		t.Fatalf("seeded %d ids, want %d", len(ids), batch)
	}

	ln.commands.reset()
	res, err := mb.FetchMany(t.Context(), ids)
	if err != nil {
		t.Fatalf("FetchMany: %v", err)
	}
	if !slices.Equal(fetchedIDs(res), ids) || len(res.Failed) != 0 {
		t.Fatalf("FetchMany = %v fetched, %v failed; want all %d", fetchedIDs(res), fetchFailedIDs(res), batch)
	}
	for _, m := range res.Fetched {
		if string(m.Raw) != testMessage {
			t.Fatalf("message %s = %q, want the seeded body", m.ID, m.Raw)
		}
	}

	// One pre-flight and one body fetch: two UID FETCHes for fifty
	// messages, not fifty-one.
	if got := ln.commands.count("UID FETCH"); got != 2 {
		t.Errorf("UID FETCH issued %d times for a %d-id batch, want 2 (one size, one body)", got, batch)
	}
	if got := ln.commands.matching("RFC822.SIZE"); got != 1 {
		t.Errorf("size pre-flight issued %d times, want exactly 1", got)
	}
	if got := ln.commands.matching("BODY.PEEK"); got != 1 {
		t.Errorf("body fetch issued %d times, want exactly 1 for the whole batch", got)
	}
	// Reading never marks anything seen, in bulk exactly as singly.
	if left, err := mb.ListUnread(t.Context()); err != nil || len(left) != batch {
		t.Errorf("unread after bulk fetch = %d, err %v, want all %d still unread", len(left), err, batch)
	}
}

// The measurement the batching exists for: the same work one message at
// a time costs a command per message.
func TestIMAPSingleFetchCostsPerMessage(t *testing.T) {
	const batch = 50
	addr, ln := startSeededIMAPServer(t, batch)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)

	ln.commands.reset()
	for _, id := range ids {
		if _, err := mb.Fetch(t.Context(), id); err != nil {
			t.Fatalf("Fetch %s: %v", id, err)
		}
	}
	if got := ln.commands.count("UID FETCH"); got != batch {
		t.Errorf("UID FETCH issued %d times for %d single fetches, want %d", got, batch, batch)
	}
}

// The size guard has to fire before a single body is read. A guard that
// fetches first and refuses afterwards has already spent the memory it
// exists to protect, so the assertion is on the commands, not the error.
func TestIMAPBulkFetchRefusesOversizedBatchBeforeReading(t *testing.T) {
	const (
		count = 4
		each  = 8 << 20 // 32 MiB in total, over the 25 MiB budget
	)
	bodies := make([][]byte, count)
	for i := range bodies {
		bodies[i] = message(fmt.Sprintf("big-%d", i), each)
	}
	addr, ln := startMessagesIMAPServer(t, bodies)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)

	ln.commands.reset()
	res, err := mb.FetchMany(t.Context(), ids)
	if !errors.Is(err, domain.ErrFetchTooLarge) {
		t.Fatalf("oversized FetchMany = %v (%d fetched), want ErrFetchTooLarge", err, len(res.Fetched))
	}
	// The refusal has to be actionable: the budget, what the batch
	// actually measures, and how many ids that covers.
	for _, want := range []string{"25 MiB", strconv.Itoa(count * each), strconv.Itoa(count) + " ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if len(res.Fetched) != 0 {
		t.Errorf("refused batch still returned %d messages", len(res.Fetched))
	}

	// The point of the pre-flight: the sizes were asked for, the bodies
	// never were.
	if got := ln.commands.matching("RFC822.SIZE"); got != 1 {
		t.Errorf("size pre-flight issued %d times, want 1", got)
	}
	if got := ln.commands.matching("BODY.PEEK"); got != 0 {
		t.Errorf("%d body fetches issued for a refused batch, want none", got)
	}

	// A refusal is the caller's batch being wrong, not the session: the
	// connection survives and the next call reuses it.
	before := ln.accepted()
	if _, err := mb.FetchMany(t.Context(), ids[:1]); err != nil {
		t.Fatalf("fetch after refusal: %v", err)
	}
	if got := ln.accepted(); got != before {
		t.Errorf("connections after a refusal = %d, want the cached one reused (%d)", got, before)
	}
}

// Just under the budget is not a near miss to be rounded away — it is a
// batch the caller is entitled to.
func TestIMAPBulkFetchAcceptsBatchUnderBudget(t *testing.T) {
	const (
		count = 3
		each  = 8 << 20 // 24 MiB in total, inside the 25 MiB budget
	)
	bodies := make([][]byte, count)
	for i := range bodies {
		bodies[i] = message(fmt.Sprintf("big-%d", i), each)
	}
	addr, _ := startMessagesIMAPServer(t, bodies)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)

	res, err := mb.FetchMany(t.Context(), ids)
	if err != nil {
		t.Fatalf("FetchMany just under the budget: %v", err)
	}
	if len(res.Fetched) != count || len(res.Failed) != 0 {
		t.Fatalf("fetched %d, failed %v; want all %d", len(res.Fetched), fetchFailedIDs(res), count)
	}
	for _, m := range res.Fetched {
		if len(m.Raw) != each {
			t.Errorf("message %s is %d bytes, want %d — nothing may be truncated", m.ID, len(m.Raw), each)
		}
	}
}

// A batch is not a transaction. An id the server does not hold costs
// itself an entry in the report; the good mail still reaches the caller.
func TestIMAPBulkFetchReportsBadIDsPerID(t *testing.T) {
	addr, _ := startSeededIMAPServer(t, 3)
	mb := newTestIMAPMailbox(t, addr)
	good := unreadIDs(t, mb)

	stale := strconv.Itoa(4242)
	batch := []string{good[0], stale, good[1], "not-a-uid", good[2]}

	res, err := mb.FetchMany(t.Context(), batch)
	if err != nil {
		t.Fatalf("FetchMany: %v", err)
	}
	if !slices.Equal(fetchedIDs(res), good) {
		t.Errorf("fetched = %v, want the three real ids %v", fetchedIDs(res), good)
	}
	got := fetchFailedIDs(res)
	if len(got) != 2 || !slices.Contains(got, stale) || !slices.Contains(got, "not-a-uid") {
		t.Errorf("failed = %v, want exactly %q and \"not-a-uid\"", got, stale)
	}
	for _, f := range res.Failed {
		if !errors.Is(f.Err, domain.ErrBadID) {
			t.Errorf("failure for %s = %v, want ErrBadID", f.ID, f.Err)
		}
	}
}

// The batch rules are the bulk rules: the cap bounds one call, and a
// repeated id would make its own outcome ambiguous.
func TestIMAPBulkFetchRejectsOversizedBatch(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	ids := make([]string, domain.MaxBulkIDs+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	if _, err := mb.FetchMany(t.Context(), ids); !errors.Is(err, domain.ErrBulkSize) {
		t.Errorf("over-cap FetchMany = %v, want ErrBulkSize", err)
	}
	if _, err := mb.FetchMany(t.Context(), nil); !errors.Is(err, domain.ErrBulkSize) {
		t.Errorf("empty FetchMany = %v, want ErrBulkSize", err)
	}
	if _, err := mb.FetchMany(t.Context(), []string{"1", "1"}); !errors.Is(err, domain.ErrBulkSize) {
		t.Errorf("duplicate FetchMany = %v, want ErrBulkSize", err)
	}
}

// Two backends, one contract: the caller must not be able to tell which
// one answered. IMAP batches natively, maildir loops through the shared
// fallback, and both have to produce the same report for the same batch.
func TestBulkFetchIdenticalAcrossBackends(t *testing.T) {
	addr, _ := startSeededIMAPServer(t, 2)
	imapBox := newTestIMAPMailbox(t, addr)
	imapIDs := unreadIDs(t, imapBox)

	root := t.TempDir()
	dirBox, err := maildir.New(root)
	if err != nil {
		t.Fatal(err)
	}
	// Same bodies, same order, so only the backend differs.
	dirIDs := []string{"m1.eml", "m2.eml"}
	for _, name := range dirIDs {
		if err := os.WriteFile(filepath.Join(root, "new", name), []byte(testMessage), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name string
		svc  *application.Service
		ids  []string
	}{
		{"imap", application.NewService(imapBox, nil), append(slices.Clone(imapIDs), "4242")},
		{"maildir", application.NewService(dirBox, nil), append(slices.Clone(dirIDs), "ghost.eml")},
	} {
		res, err := tc.svc.ReadMany(t.Context(), "", "", tc.ids)
		if err != nil {
			t.Fatalf("%s ReadMany: %v", tc.name, err)
		}
		if !slices.Equal(fetchedIDs(res), tc.ids[:2]) {
			t.Errorf("%s fetched = %v, want %v", tc.name, fetchedIDs(res), tc.ids[:2])
		}
		for _, m := range res.Fetched {
			if string(m.Raw) != testMessage {
				t.Errorf("%s message %s = %q, want the seeded body", tc.name, m.ID, m.Raw)
			}
		}
		if len(res.Failed) != 1 || res.Failed[0].ID != tc.ids[2] {
			t.Fatalf("%s failed = %v, want only the unknown id", tc.name, fetchFailedIDs(res))
		}
		if !errors.Is(res.Failed[0].Err, domain.ErrBadID) {
			t.Errorf("%s failure = %v, want ErrBadID", tc.name, res.Failed[0].Err)
		}
	}
}
