package application_test

import (
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

func (m *memBox) ListUnread() ([]string, error) {
	var ids []string
	for id := range m.msgs {
		if !m.seen[id] && !m.archived[id] && !m.trashed[id] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memBox) Fetch(id string) ([]byte, error) {
	raw, ok := m.msgs[id]
	if !ok {
		return nil, domain.ErrBadID
	}
	return []byte(raw), nil
}

func (m *memBox) MarkSeen(id string) error { m.seen[id] = true; return nil }

func (m *memBox) Folders() ([]string, error) {
	out := []string{"INBOX"}
	for name := range m.folders {
		out = append(out, name)
	}
	return out, nil
}

func (m *memBox) InFolder(name string) (domain.Mailbox, error) {
	if name == "INBOX" {
		return m, nil
	}
	f, ok := m.folders[name]
	if !ok {
		return nil, errors.New("no such folder")
	}
	return f, nil
}

func (m *memBox) Archive(id string) error { m.archived[id] = true; return nil }
func (m *memBox) Delete(id string) error  { m.trashed[id] = true; return nil }

// bareBox has no optional capabilities at all.
type bareBox struct{ inner *memBox }

func (b bareBox) ListUnread() ([]string, error)   { return b.inner.ListUnread() }
func (b bareBox) Fetch(id string) ([]byte, error) { return b.inner.Fetch(id) }
func (b bareBox) MarkSeen(id string) error        { return b.inner.MarkSeen(id) }

func TestServiceRoutingAndReads(t *testing.T) {
	inbox := newMemBox(map[string]string{"a.eml": "From: x\r\nSubject: Spende\r\n\r\nDanke"})
	steuern := newMemBox(map[string]string{"s.eml": "From: amt\r\n\r\nBescheid"})
	inbox.folders["steuern"] = steuern
	business := newMemBox(map[string]string{"b.eml": "From: kunde\r\n\r\nAuftrag"})

	svc := application.NewService(inbox, map[string]domain.Mailbox{"business": business})

	ids, err := svc.ListUnread("", "")
	if err != nil || len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("default = %v err %v", ids, err)
	}
	ids, err = svc.ListUnread("", "steuern")
	if err != nil || len(ids) != 1 || ids[0] != "s.eml" {
		t.Errorf("folder = %v err %v", ids, err)
	}
	ids, err = svc.ListUnread("business", "")
	if err != nil || len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("account = %v err %v", ids, err)
	}
	if _, err := svc.ListUnread("nope", ""); err == nil {
		t.Error("unknown account accepted")
	}

	raw, err := svc.Read("", "", "a.eml")
	if err != nil || !strings.Contains(string(raw), "Spende") {
		t.Errorf("read = %q err %v", raw, err)
	}
	if err := svc.MarkSeen("", "", "a.eml"); err != nil {
		t.Fatal(err)
	}
	ids, _ = svc.ListUnread("", "")
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

	ids, err := svc.Search("", "", "spende")
	if err != nil || len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("search = %v err %v", ids, err)
	}
	ids, err = svc.Search("", "", "nirgends")
	if err != nil || len(ids) != 0 {
		t.Errorf("no-match = %v err %v", ids, err)
	}

	// bareBox lacks folders: default folder list, scoped folder errors.
	folders, err := svc.Folders("")
	if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Errorf("folders = %v err %v", folders, err)
	}
	if _, err := svc.ListUnread("", "steuern"); err == nil {
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

	if err := svc.Archive("", "", "a.eml"); err != nil || !inbox.archived["a.eml"] {
		t.Errorf("archive err %v archived %v", err, inbox.archived)
	}
	if err := svc.Delete("", "", "b.eml"); err != nil || !inbox.trashed["b.eml"] {
		t.Errorf("delete err %v trashed %v", err, inbox.trashed)
	}

	// No Curator capability → clear error.
	bare := application.NewService(bareBox{newMemBox(map[string]string{"c.eml": "z"})}, nil)
	if err := bare.Archive("", "", "c.eml"); err == nil {
		t.Error("archive on curatorless backend accepted")
	}
	if err := bare.Delete("", "", "c.eml"); err == nil {
		t.Error("delete on curatorless backend accepted")
	}
}

func TestSwitchableSwapAndForwarding(t *testing.T) {
	a := newMemBox(map[string]string{"a.eml": "Spende"})
	b := newMemBox(map[string]string{"b.eml": "Rechnung"})
	sw := application.NewSwitchable(a)

	ids, _ := sw.ListUnread()
	if len(ids) != 1 || ids[0] != "a.eml" {
		t.Errorf("before swap = %v", ids)
	}
	sw.Swap(b)
	ids, _ = sw.ListUnread()
	if len(ids) != 1 || ids[0] != "b.eml" {
		t.Errorf("after swap = %v", ids)
	}

	if _, err := sw.Search("rechnung"); err != nil {
		t.Errorf("search: %v", err)
	}
	if _, err := sw.Folders(); err != nil {
		t.Errorf("folders: %v", err)
	}
	if err := sw.Archive("b.eml"); err != nil {
		t.Errorf("archive: %v", err)
	}
	if _, err := sw.InFolder("INBOX"); err != nil {
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

	if _, err := sw.InFolder("steuern"); err == nil {
		t.Error("folder on folderless backend accepted")
	}
	if _, err := sw.InFolder("INBOX"); err != nil {
		t.Errorf("INBOX self-resolve: %v", err)
	}
	folders, err := sw.Folders()
	if err != nil || len(folders) != 1 {
		t.Errorf("folders = %v err %v", folders, err)
	}
	if err := sw.Archive("a.eml"); err == nil {
		t.Error("archive on curatorless backend accepted")
	}
	if err := sw.Delete("a.eml"); err == nil {
		t.Error("delete on curatorless backend accepted")
	}
	if _, err := sw.Fetch("a.eml"); err != nil {
		t.Errorf("fetch: %v", err)
	}
	if err := sw.MarkSeen("a.eml"); err != nil {
		t.Errorf("seen: %v", err)
	}
}
