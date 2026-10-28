package imap_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"go.klarlabs.de/briefkasten/domain"

	bimap "go.klarlabs.de/briefkasten/infrastructure/imap"
)

// unreadIDs returns every seeded id, in the order the server listed them.
func unreadIDs(t *testing.T, mb *bimap.Mailbox) []string {
	t.Helper()
	ids, err := mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	return ids
}

// failedIDs renders a result's failures as ids, for comparison.
func failedIDs(res domain.BulkResult) []string {
	out := make([]string, 0, len(res.Failed))
	for _, f := range res.Failed {
		out = append(out, f.ID)
	}
	return out
}

// A bulk UIDPLUS move performs one COPY, one STORE, and a UID-scoped
// EXPUNGE, then verifies source absence and destination presence.
func TestIMAPBulkDeleteIssuesOneUIDPlusMove(t *testing.T) {
	const batch = 50
	addr, ln := startSeededIMAPServer(t, batch)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)
	if len(ids) != batch {
		t.Fatalf("seeded %d ids, want %d", len(ids), batch)
	}

	ln.commands.reset()
	res, err := mb.DeleteMany(t.Context(), ids)
	if err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	if len(res.Succeeded) != batch || len(res.Failed) != 0 {
		t.Fatalf("DeleteMany = %d moved, %v failed; want all %d moved", len(res.Succeeded), failedIDs(res), batch)
	}
	if n := filedIn(t, addr, "Trash"); n != batch {
		t.Errorf("Trash holds %d messages, want %d", n, batch)
	}
	if n := filedIn(t, addr, "INBOX"); n != 0 {
		t.Errorf("INBOX holds %d messages after move, want 0", n)
	}

	for _, cmd := range []string{"UID COPY", "UID STORE", "UID EXPUNGE", "UID FETCH", "LIST"} {
		want := 1
		if cmd == "UID FETCH" {
			want = 3 // initial existence, post-move source, and destination verification
		}
		if got := ln.commands.count(cmd); got != want {
			t.Errorf("%s issued %d times for a %d-id batch, want %d", cmd, got, batch, want)
		}
	}
}

func TestIMAPMoveRefusesServerWithoutSafeMoveCapability(t *testing.T) {
	addr, ln := startMessagesIMAPServerCaps(t, imap.CapSet{imap.CapIMAP4rev1: {}}, [][]byte{[]byte(testMessage)}, "Trash")
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)
	ln.commands.reset()
	if err := mb.Delete(t.Context(), ids[0]); err == nil || !strings.Contains(err.Error(), "lacks MOVE and UIDPLUS") {
		t.Fatalf("Delete error = %v, want explicit safe-move refusal", err)
	}
	if n := filedIn(t, addr, "INBOX"); n != 1 {
		t.Errorf("INBOX holds %d messages after refused move, want 1", n)
	}
	if n := filedIn(t, addr, "Trash"); n != 0 {
		t.Errorf("Trash holds %d messages after refused move, want 0", n)
	}
	if n := ln.commands.count("UID MOVE"); n != 0 {
		t.Errorf("UID MOVE issued %d times, want 0", n)
	}
	if n := ln.commands.count("UID COPY"); n != 0 {
		t.Errorf("UID COPY issued %d times, want 0", n)
	}
}

// Single deletes issue one UIDPLUS move per message.
func TestIMAPSingleDeleteCostsPerMessage(t *testing.T) {
	const batch = 50
	addr, ln := startSeededIMAPServer(t, batch)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)

	ln.commands.reset()
	for _, id := range ids {
		if err := mb.Delete(t.Context(), id); err != nil {
			t.Fatalf("Delete %s: %v", id, err)
		}
	}
	for _, cmd := range []string{"UID COPY", "UID STORE", "UID EXPUNGE"} {
		if got := ln.commands.count(cmd); got != batch {
			t.Errorf("%s issued %d times for %d single deletes, want %d", cmd, got, batch, batch)
		}
	}
}

// A batch is not a transaction: an id the server does not hold must cost
// itself an entry in the report and nothing more. Sinking the batch — or,
// worse, reporting the whole batch moved because COPY answered OK — are
// the two failures this asserts against.
func TestIMAPBulkArchiveReportsBadIDsPerID(t *testing.T) {
	addr, _ := startSeededIMAPServer(t, 3)
	mb := newTestIMAPMailbox(t, addr)
	good := unreadIDs(t, mb)

	// A UID far beyond anything seeded, and something that is not a UID
	// at all: the two ways an id can be unusable.
	stale := strconv.Itoa(4242)
	batch := []string{good[0], stale, good[1], "not-a-uid", good[2]}

	res, err := mb.ArchiveMany(t.Context(), batch)
	if err != nil {
		t.Fatalf("ArchiveMany: %v", err)
	}
	if !slices.Equal(res.Succeeded, good) {
		t.Errorf("archived = %v, want the three real ids %v", res.Succeeded, good)
	}
	if got := failedIDs(res); len(got) != 2 || !slices.Contains(got, stale) || !slices.Contains(got, "not-a-uid") {
		t.Errorf("failed = %v, want exactly %q and \"not-a-uid\"", got, stale)
	}
	for _, f := range res.Failed {
		if !errors.Is(f.Err, domain.ErrBadID) {
			t.Errorf("failure for %s = %v, want ErrBadID", f.ID, f.Err)
		}
	}
	if n := filedIn(t, addr, "Archive"); n != 3 {
		t.Errorf("Archive holds %d messages, want the 3 good ones", n)
	}
}

// Bulk mark-seen carries MarkSeen's guarantees: an id the mailbox does
// not hold is never claimed as marked, and re-acknowledging read mail
// stays a success.
func TestIMAPBulkMarkSeen(t *testing.T) {
	addr, ln := startSeededIMAPServer(t, 3)
	mb := newTestIMAPMailbox(t, addr)
	ids := unreadIDs(t, mb)

	ln.commands.reset()
	res, err := mb.MarkSeenMany(t.Context(), append(slices.Clone(ids), "4242"))
	if err != nil {
		t.Fatalf("MarkSeenMany: %v", err)
	}
	if !slices.Equal(res.Succeeded, ids) || len(res.Failed) != 1 {
		t.Fatalf("MarkSeenMany = %v marked, %v failed", res.Succeeded, failedIDs(res))
	}
	if !errors.Is(res.Failed[0].Err, domain.ErrBadID) {
		t.Errorf("stale id failure = %v, want ErrBadID", res.Failed[0].Err)
	}
	if got := ln.commands.count("UID STORE"); got != 1 {
		t.Errorf("UID STORE issued %d times, want 1 for the batch", got)
	}
	if left, err := mb.ListUnread(t.Context()); err != nil || len(left) != 0 {
		t.Fatalf("unread after bulk mark seen = %v, err %v, want none", left, err)
	}

	// Idempotent: acknowledging read mail again changes nothing.
	res, err = mb.MarkSeenMany(t.Context(), ids)
	if err != nil || len(res.Succeeded) != len(ids) || len(res.Failed) != 0 {
		t.Errorf("second MarkSeenMany = %v / %v, err %v; want all marked again", res.Succeeded, failedIDs(res), err)
	}
}

// The cap bounds what one confirmation can authorise, so it is refused
// with the number named rather than quietly trimmed to the first 100.
func TestIMAPBulkRejectsOversizedBatch(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	ids := make([]string, domain.MaxBulkIDs+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	if _, err := mb.DeleteMany(t.Context(), ids); !errors.Is(err, domain.ErrBulkSize) {
		t.Errorf("oversized DeleteMany = %v, want ErrBulkSize", err)
	}
	if _, err := mb.ArchiveMany(t.Context(), nil); !errors.Is(err, domain.ErrBulkSize) {
		t.Errorf("empty ArchiveMany = %v, want ErrBulkSize", err)
	}
	if _, err := mb.MarkSeenMany(t.Context(), []string{"1", "1"}); !errors.Is(err, domain.ErrBulkSize) {
		t.Errorf("duplicate MarkSeenMany = %v, want ErrBulkSize", err)
	}
}
