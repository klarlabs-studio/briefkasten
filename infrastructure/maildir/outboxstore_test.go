package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/briefkasten/domain"
)

func newOutbox(t *testing.T) (*OutboxStore, string) {
	t.Helper()
	root := t.TempDir()
	st, err := NewOutboxStore(root)
	if err != nil {
		t.Fatalf("NewOutboxStore: %v", err)
	}
	return st, root
}

func TestOutboxStoreNewCreatesStateDirs(t *testing.T) {
	_, root := newOutbox(t)
	for _, state := range []string{"queued", "sending", "sent", "failed"} {
		info, err := os.Stat(filepath.Join(root, state))
		if err != nil || !info.IsDir() {
			t.Errorf("state dir %s not created: %v", state, err)
		}
	}
}

func TestOutboxStoreWriteFindRoundtrip(t *testing.T) {
	st, _ := newOutbox(t)
	msg := domain.OutboundMessage{
		ID:      "m1",
		To:      []string{"you@example.com"},
		Subject: "Hello",
		Body:    "hi there",
		State:   "queued",
	}
	if err := st.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := st.Find("m1")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.ID != msg.ID || got.Subject != msg.Subject || got.Body != msg.Body || got.State != "queued" {
		t.Errorf("Find = %+v, want %+v", got, msg)
	}
	if len(got.To) != 1 || got.To[0] != "you@example.com" {
		t.Errorf("Find To = %v, want [you@example.com]", got.To)
	}
}

func TestOutboxStoreFindAfterStateChange(t *testing.T) {
	st, _ := newOutbox(t)
	msg := domain.OutboundMessage{ID: "m1", To: []string{"you@example.com"}, State: "queued"}
	if err := st.Write(msg); err != nil {
		t.Fatalf("Write queued: %v", err)
	}

	msg.State = "sent"
	if err := st.Write(msg); err != nil {
		t.Fatalf("Write sent: %v", err)
	}
	if err := st.Remove("queued", "m1"); err != nil {
		t.Fatalf("Remove queued: %v", err)
	}

	got, err := st.Find("m1")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.State != "sent" {
		t.Errorf("State = %q, want sent", got.State)
	}
}

func TestOutboxStoreFindUnknown(t *testing.T) {
	st, _ := newOutbox(t)
	_, err := st.Find("nope")
	if err == nil {
		t.Fatal("unknown id accepted")
	}
	if !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Find error = %v, want ErrBadID", err)
	}
}

func TestOutboxStoreFindRejectsTraversal(t *testing.T) {
	st, _ := newOutbox(t)
	_, err := st.Find("../x")
	if err == nil {
		t.Fatal("path traversal accepted in Find")
	}
	if !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Find error = %v, want ErrBadID", err)
	}
}

func TestOutboxStoreList(t *testing.T) {
	st, _ := newOutbox(t)
	for _, id := range []string{"a", "b"} {
		msg := domain.OutboundMessage{ID: id, To: []string{"you@example.com"}, State: "queued"}
		if err := st.Write(msg); err != nil {
			t.Fatalf("Write %s: %v", id, err)
		}
	}

	ids, err := st.List("queued")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("List = %v, want [a b]", ids)
	}

	empty, err := st.List("failed")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("List failed = %v, want empty", empty)
	}
}

func TestOutboxStoreRemoveMissing(t *testing.T) {
	st, _ := newOutbox(t)
	if err := st.Remove("queued", "nope"); err == nil {
		t.Error("Remove of missing record accepted")
	}
}

func TestOutboxStoreInterruptedWriteLeavesRecordIntact(t *testing.T) {
	st, root := newOutbox(t)
	msg := domain.OutboundMessage{ID: "m1", To: []string{"you@example.com"}, Subject: "keep me", State: "queued"}
	if err := st.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// A crash mid-write: the record made it into the staging area and was
	// never renamed. The staged bytes must be invisible to everything.
	staged := filepath.Join(root, "tmp", "queued.m1.json")
	if err := os.WriteFile(staged, []byte(`{"id":"m1","subj`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := st.Find("m1")
	if err != nil {
		t.Fatalf("Find after interrupted write: %v", err)
	}
	if got.Subject != "keep me" {
		t.Errorf("subject = %q, want the previous record intact", got.Subject)
	}
	ids, err := st.List("queued")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("List = %v, want only the completed record", ids)
	}

	// The next write reuses the staging name, so the debris does not
	// accumulate and cannot poison a later record.
	msg.Subject = "second"
	if err := st.Write(msg); err != nil {
		t.Fatalf("Write over stale staging: %v", err)
	}
	got, _ = st.Find("m1")
	if got.Subject != "second" {
		t.Errorf("subject = %q, want second", got.Subject)
	}
}

func TestOutboxStoreFindSurfacesUndecodableRecord(t *testing.T) {
	st, root := newOutbox(t)
	// A record truncated by a crash under the old non-atomic write. It must
	// read as damage, never as "no such message" — an id that silently
	// vanishes is a lost message.
	if err := os.WriteFile(filepath.Join(root, "queued", "torn.json"), []byte(`{"id":"torn"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := st.Find("torn")
	if err == nil {
		t.Fatal("undecodable record read as valid")
	}
	if errors.Is(err, domain.ErrBadID) {
		t.Errorf("err = %v, want a decode error rather than an unknown-id error", err)
	}
	if !strings.Contains(err.Error(), "torn") {
		t.Errorf("err = %v, want the damaged record named", err)
	}
}

func TestOutboxStoreFindInAddressesOneState(t *testing.T) {
	st, _ := newOutbox(t)
	// A half-done move: the same id under two states at once.
	if err := st.Write(domain.OutboundMessage{ID: "m1", To: []string{"a@b.c"}, State: "queued"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(domain.OutboundMessage{ID: "m1", To: []string{"a@b.c"}, State: "sending", Attempts: 1}); err != nil {
		t.Fatal(err)
	}

	queued, err := st.FindIn("queued", "m1")
	if err != nil {
		t.Fatalf("FindIn queued: %v", err)
	}
	sending, err := st.FindIn("sending", "m1")
	if err != nil {
		t.Fatalf("FindIn sending: %v", err)
	}
	if queued.Attempts != 0 || sending.Attempts != 1 {
		t.Errorf("attempts = %d/%d, want the two copies told apart", queued.Attempts, sending.Attempts)
	}
	if _, err := st.FindIn("sent", "m1"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("FindIn of a state without the record = %v, want ErrBadID", err)
	}
}

func TestOutboxStoreLockKeepsASecondHolderOut(t *testing.T) {
	st, root := newOutbox(t)
	// flock is bound to the open file description, so a second store handle
	// over the same directory contends exactly as a second process does.
	other, err := NewOutboxStore(root)
	if err != nil {
		t.Fatal(err)
	}

	release, err := st.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, ok, err := other.TryLock(); err != nil || ok {
		t.Errorf("TryLock on a held outbox = %v, %v; want refused", ok, err)
	}

	release()
	release() // releasing twice is harmless

	stolen, ok, err := other.TryLock()
	if err != nil || !ok {
		t.Fatalf("TryLock after release = %v, %v; want granted", ok, err)
	}
	stolen()
}

func TestOutboxStoreLockWaitsForTheHolder(t *testing.T) {
	st, root := newOutbox(t)
	other, err := NewOutboxStore(root)
	if err != nil {
		t.Fatal(err)
	}
	release, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}

	granted := make(chan struct{})
	go func() {
		secondRelease, err := other.Lock()
		if err == nil {
			secondRelease()
		}
		close(granted)
	}()

	select {
	case <-granted:
		t.Fatal("Lock granted while another holder had the outbox")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-granted:
	case <-time.After(5 * time.Second):
		t.Fatal("Lock never granted after the holder released")
	}
}
