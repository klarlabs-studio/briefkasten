package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/application"
	"go.klarlabs.de/briefkasten/domain"
)

// memBox is an in-memory Mailbox with optional capabilities.
type memBox struct {
	msgs     map[string]string
	seen     map[string]bool
	archived map[string]bool
	trashed  map[string]bool
	folders  map[string]*memBox
}

func newMemBox(msgs map[string]string) *memBox {
	return &memBox{
		msgs: msgs, seen: map[string]bool{},
		archived: map[string]bool{}, trashed: map[string]bool{},
		folders: map[string]*memBox{},
	}
}

func (m *memBox) ListUnread(context.Context) ([]string, error) {
	var ids []string
	for id := range m.msgs {
		if !m.seen[id] && !m.archived[id] && !m.trashed[id] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memBox) Fetch(_ context.Context, id string) ([]byte, error) {
	raw, ok := m.msgs[id]
	if !ok {
		return nil, domain.ErrBadID
	}
	return []byte(raw), nil
}

func (m *memBox) MarkSeen(_ context.Context, id string) error { m.seen[id] = true; return nil }

func (m *memBox) Folders(context.Context) ([]string, error) {
	out := []string{"INBOX"}
	for name := range m.folders {
		out = append(out, name)
	}
	return out, nil
}

func (m *memBox) InFolder(_ context.Context, name string) (domain.Mailbox, error) {
	if name == "INBOX" {
		return m, nil
	}
	f, ok := m.folders[name]
	if !ok {
		return nil, errors.New("no such folder")
	}
	return f, nil
}

func (m *memBox) Archive(_ context.Context, id string) error { m.archived[id] = true; return nil }
func (m *memBox) Delete(_ context.Context, id string) error  { m.trashed[id] = true; return nil }

// bareBox has no optional capabilities at all.
type bareBox struct{ inner *memBox }

func (b bareBox) ListUnread(ctx context.Context) ([]string, error) { return b.inner.ListUnread(ctx) }

func (b bareBox) Fetch(ctx context.Context, id string) ([]byte, error) {
	return b.inner.Fetch(ctx, id)
}
func (b bareBox) MarkSeen(ctx context.Context, id string) error { return b.inner.MarkSeen(ctx, id) }

func TestServiceRoutingAndReads(t *testing.T) {
	inbox := newMemBox(map[string]string{"a.eml": "From: x\r\nSubject: Spende\r\n\r\nDanke"})
	steuern := newMemBox(map[string]string{"s.eml": "From: amt\r\n\r\nBescheid"})
	inbox.folders["steuern"] = steuern
	business := newMemBox(map[string]string{"b.eml": "From: kunde\r\n\r\nAuftrag"})

	svc := application.NewService(inbox, map[string]domain.Mailbox{"business": business})

	ids, err := svc.ListUnread(t.Context(), "", "")
	if err != nil || len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("default = %v err %v", ids, err)
	}
	ids, err = svc.ListUnread(t.Context(), "", "steuern")
	if err != nil || len(ids) != 1 || ids[0] != "s.eml" {
		t.Errorf("folder = %v err %v", ids, err)
	}
	ids, err = svc.ListUnread(t.Context(), "business", "")
	if err != nil || len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("account = %v err %v", ids, err)
	}
	if _, err := svc.ListUnread(t.Context(), "nope", ""); err == nil {
		t.Error("unknown account accepted")
	}

	raw, err := svc.Read(t.Context(), "", "", "a.eml")
	if err != nil || !strings.Contains(string(raw), "Spende") {
		t.Errorf("read = %q err %v", raw, err)
	}
	if err := svc.MarkSeen(t.Context(), "", "", "a.eml"); err != nil {
		t.Fatal(err)
	}
	ids, _ = svc.ListUnread(t.Context(), "", "")
	if len(ids) != 0 {
		t.Errorf("unread after seen = %v", ids)
	}
}

func TestServiceSearchFallbackAndFolders(t *testing.T) {
	// bareBox lacks Searcher: the service scans.
	inner := newMemBox(map[string]string{
		"a.eml": "From: x\r\nSubject: Spende\r\n\r\nDanke",
		"b.eml": "From: y\r\nSubject: Rechnung\r\n\r\nBetrag",
	})
	svc := application.NewService(bareBox{inner}, nil)

	ids, err := svc.Search(t.Context(), "", "", "spende")
	if err != nil || len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("search = %v err %v", ids, err)
	}
	ids, err = svc.Search(t.Context(), "", "", "nirgends")
	if err != nil || len(ids) != 0 {
		t.Errorf("no-match = %v err %v", ids, err)
	}

	// bareBox lacks folders: default folder list, scoped folder errors.
	folders, err := svc.Folders(t.Context(), "")
	if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Errorf("folders = %v err %v", folders, err)
	}
	if _, err := svc.ListUnread(t.Context(), "", "steuern"); err == nil {
		t.Error("folder on folderless backend accepted")
	}
}

func TestServiceAccountsAndCuration(t *testing.T) {
	inbox := newMemBox(map[string]string{"a.eml": "x", "b.eml": "y"})
	svc := application.NewService(inbox, map[string]domain.Mailbox{"zwei": newMemBox(nil), "eins": newMemBox(nil)})

	accounts := svc.Accounts()
	if len(accounts) != 3 || accounts[0] != "default" || accounts[1] != "eins" {
		t.Errorf("accounts = %v", accounts)
	}

	if err := svc.Archive(t.Context(), "", "", "a.eml"); err != nil || !inbox.archived["a.eml"] {
		t.Errorf("archive err %v archived %v", err, inbox.archived)
	}
	if err := svc.Delete(t.Context(), "", "", "b.eml"); err != nil || !inbox.trashed["b.eml"] {
		t.Errorf("delete err %v trashed %v", err, inbox.trashed)
	}

	// No Curator capability → clear error.
	bare := application.NewService(bareBox{newMemBox(map[string]string{"c.eml": "z"})}, nil)
	if err := bare.Archive(t.Context(), "", "", "c.eml"); err == nil {
		t.Error("archive on curatorless backend accepted")
	}
	if err := bare.Delete(t.Context(), "", "", "c.eml"); err == nil {
		t.Error("delete on curatorless backend accepted")
	}
}

func TestSwitchableSwapAndForwarding(t *testing.T) {
	a := newMemBox(map[string]string{"a.eml": "Spende"})
	b := newMemBox(map[string]string{"b.eml": "Rechnung"})
	sw := application.NewSwitchable(a)

	ids, _ := sw.ListUnread(t.Context())
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("before swap = %v", ids)
	}
	sw.Swap(b)
	ids, _ = sw.ListUnread(t.Context())
	if len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("after swap = %v", ids)
	}

	if _, err := sw.Search(t.Context(), "rechnung"); err != nil {
		t.Errorf("search: %v", err)
	}
	if _, err := sw.Folders(t.Context()); err != nil {
		t.Errorf("folders: %v", err)
	}
	if err := sw.Archive(t.Context(), "b.eml"); err != nil {
		t.Errorf("archive: %v", err)
	}
	if _, err := sw.InFolder(t.Context(), "INBOX"); err != nil {
		t.Errorf("infolder: %v", err)
	}
}

// failStore breaks on demand to exercise the outbox error paths.
type failStore struct {
	domain.OutboxStore
	failWrite bool
}

func (f failStore) Write(msg domain.OutboundMessage) error {
	if f.failWrite {
		return errors.New("disk full")
	}
	return f.OutboxStore.Write(msg)
}

func TestOutboxStoreFailures(t *testing.T) {
	ob := application.NewOutbox(failStore{failWrite: true}, nil)
	if _, err := ob.Enqueue(domain.OutboundMessage{To: []string{"a@b.c"}}); err == nil {
		t.Error("write failure swallowed")
	}
	if _, err := ob.Enqueue(domain.OutboundMessage{}); err == nil {
		t.Error("invalid message accepted")
	}
}

func TestSwitchableCapabilityErrors(t *testing.T) {
	sw := application.NewSwitchable(bareBox{newMemBox(map[string]string{"a.eml": "x"})})

	if _, err := sw.InFolder(t.Context(), "steuern"); err == nil {
		t.Error("folder on folderless backend accepted")
	}
	if _, err := sw.InFolder(t.Context(), "INBOX"); err != nil {
		t.Errorf("INBOX self-resolve: %v", err)
	}
	folders, err := sw.Folders(t.Context())
	if err != nil || len(folders) != 1 {
		t.Errorf("folders = %v err %v", folders, err)
	}
	if err := sw.Archive(t.Context(), "a.eml"); err == nil {
		t.Error("archive on curatorless backend accepted")
	}
	if err := sw.Delete(t.Context(), "a.eml"); err == nil {
		t.Error("delete on curatorless backend accepted")
	}
	if _, err := sw.Fetch(t.Context(), "a.eml"); err != nil {
		t.Errorf("fetch: %v", err)
	}
	if err := sw.MarkSeen(t.Context(), "a.eml"); err != nil {
		t.Errorf("seen: %v", err)
	}
}
