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
	moved    map[string]string
	folders  map[string]*memBox
}

func (m *memBox) FetchHeaders(_ context.Context, id string) (domain.MessageHeaders, error) {
	if _, ok := m.msgs[id]; !ok {
		return domain.MessageHeaders{}, domain.ErrBadID
	}
	return domain.MessageHeaders{Subject: "subject " + id}, nil
}

func (m *memBox) FetchHeadersMany(ctx context.Context, ids []string) (map[string]domain.MessageHeaders, map[string]error) {
	got, failures := map[string]domain.MessageHeaders{}, map[string]error{}
	for _, id := range ids {
		h, err := m.FetchHeaders(ctx, id)
		if err != nil {
			failures[id] = err
		} else {
			got[id] = h
		}
	}
	return got, failures
}
func (m *memBox) BulkLimit() int   { return 0 }
func (m *memBox) SetBulkLimit(int) {}
func (m *memBox) CurationPlan(context.Context) (domain.CurationPlan, error) {
	return domain.CurationPlan{Archive: domain.CurationDestination{Folder: "Archive"}, Trash: domain.CurationDestination{Folder: "Trash"}}, nil
}

type limitedBox struct {
	*memBox
	limit int
}

type senderSearchBox struct{ *memBox }

func (m *senderSearchBox) SearchFromScope(context.Context, domain.Scope, string, string) ([]string, error) {
	return []string{"sender-match"}, nil
}

func TestSearchMessagesFromUsesSenderSearchCapability(t *testing.T) {
	mb := &senderSearchBox{memBox: newMemBox(map[string]string{"sender-match": "Subject: product\r\n\r\n"})}
	svc := application.NewService(mb, nil)
	page, err := svc.SearchMessagesFrom(t.Context(), "", "", "product", "person@example.com", domain.ScopeAll, 0, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.IDs) != 1 || page.IDs[0] != "sender-match" || page.Total != 1 {
		t.Fatalf("sender search page = %#v, want sender-match only", page)
	}
}

func (m *limitedBox) BulkLimit() int         { return m.limit }
func (m *limitedBox) SetBulkLimit(limit int) { m.limit = limit }

func newMemBox(msgs map[string]string) *memBox {
	return &memBox{
		msgs: msgs, seen: map[string]bool{},
		archived: map[string]bool{}, trashed: map[string]bool{}, moved: map[string]string{},
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
func (m *memBox) Move(_ context.Context, id, destination string) error {
	m.moved[id] = destination
	return nil
}

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

func TestMessagePagesAndBulkLimit(t *testing.T) {
	box := newMemBox(map[string]string{"a": "x", "b": "y"})
	svc := application.NewService(box, nil)
	svc.SetBulkLimit(7)
	if svc.BulkLimit() != 7 {
		t.Fatalf("bulk limit = %d", svc.BulkLimit())
	}
	page, err := svc.ListMessages(t.Context(), "", "", domain.ScopeUnread, 1, 1, true)
	if err != nil || page.Total != 2 || len(page.IDs) != 1 || len(page.Messages) != 1 || page.Messages[0].Subject == "" {
		t.Fatalf("page = %#v, err %v", page, err)
	}
	page, err = svc.SearchMessages(t.Context(), "", "", "x", domain.ScopeUnread, 0, 1, true)
	if err != nil || page.Total != 1 || len(page.Messages) != 1 {
		t.Fatalf("search page = %#v, err %v", page, err)
	}
	if _, err := svc.ListMessages(t.Context(), "", "", domain.ScopeUnread, -1, 1, false); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, err := svc.SearchMessages(t.Context(), "", "", "x", "bad", 0, 1, false); err == nil {
		t.Fatal("invalid scope accepted")
	}
	if _, err := svc.ListMessages(t.Context(), "missing", "", domain.ScopeUnread, 0, 1, false); err == nil {
		t.Fatal("list page accepted unknown account")
	}
	if _, err := svc.SearchMessages(t.Context(), "missing", "", "x", domain.ScopeUnread, 0, 1, false); err == nil {
		t.Fatal("search page accepted unknown account")
	}
	if _, err := svc.Folders(t.Context(), "missing"); err == nil {
		t.Fatal("folder listing accepted unknown account")
	}
	if _, err := svc.ReadHeaders(t.Context(), "", "", "missing"); !errors.Is(err, domain.ErrBadID) {
		t.Fatalf("header fetch error = %v", err)
	}
	if _, err := svc.CurationPlan(t.Context(), "", ""); err != nil {
		t.Fatalf("curation plan = %v", err)
	}
}

func TestServiceBulkLimitUsesProviderAndForwardsToAccounts(t *testing.T) {
	defaultBox := &limitedBox{memBox: newMemBox(nil), limit: 12}
	accountBox := &limitedBox{memBox: newMemBox(nil), limit: 3}
	svc := application.NewService(defaultBox, map[string]domain.Mailbox{"other": accountBox})
	if svc.BulkLimit() != 12 {
		t.Fatalf("provider limit = %d", svc.BulkLimit())
	}
	svc.SetBulkLimit(0)
	if svc.BulkLimit() != domain.MaxBulkIDs || defaultBox.limit != domain.MaxBulkIDs || accountBox.limit != domain.MaxBulkIDs {
		t.Fatalf("reset limits service=%d default=%d account=%d", svc.BulkLimit(), defaultBox.limit, accountBox.limit)
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

func TestServiceMove(t *testing.T) {
	inbox := newMemBox(map[string]string{"a.eml": "x"})
	work := newMemBox(map[string]string{"b.eml": "y"})
	inbox.folders["Work"] = work
	business := newMemBox(map[string]string{"c.eml": "z"})
	svc := application.NewService(inbox, map[string]domain.Mailbox{"business": business})

	if err := svc.Move(t.Context(), "", "", "a.eml", "Work"); err != nil || inbox.moved["a.eml"] != "Work" {
		t.Errorf("Move inbox = %v, moved %v", err, inbox.moved)
	}
	if err := svc.Move(t.Context(), "", "Work", "b.eml", "INBOX"); err != nil || work.moved["b.eml"] != "INBOX" {
		t.Errorf("Move from folder = %v, moved %v", err, work.moved)
	}
	if err := svc.Move(t.Context(), "business", "", "c.eml", "Archive"); err != nil || business.moved["c.eml"] != "Archive" {
		t.Errorf("Move account = %v, moved %v", err, business.moved)
	}
	if err := svc.Move(t.Context(), "", "", "a.eml", ""); !errors.Is(err, domain.ErrBadFolder) {
		t.Errorf("Move empty destination = %v, want ErrBadFolder", err)
	}
	if _, err := svc.MoveMany(t.Context(), "", "", []string{"a.eml"}, ""); !errors.Is(err, domain.ErrBadFolder) {
		t.Errorf("MoveMany empty destination = %v, want ErrBadFolder", err)
	}
	if _, err := svc.MoveMany(t.Context(), "missing", "", []string{"a.eml"}, "Work"); err == nil {
		t.Error("MoveMany accepted an unknown account")
	}
	bare := application.NewService(bareBox{newMemBox(nil)}, nil)
	if err := bare.Move(t.Context(), "", "", "a.eml", "Work"); err == nil {
		t.Error("Move on backend without Mover accepted")
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
	if err := sw.Move(t.Context(), "b.eml", "Work"); err != nil || b.moved["b.eml"] != "Work" {
		t.Errorf("move: %v", err)
	}
	if _, err := sw.CurationPlan(t.Context()); err != nil {
		t.Errorf("curation plan: %v", err)
	}
	if sw.BulkLimit() != domain.MaxBulkIDs {
		t.Errorf("default bulk limit = %d", sw.BulkLimit())
	}
	sw.SetBulkLimit(23)
	if got, failures := sw.FetchHeadersMany(t.Context(), []string{"b.eml"}); len(got) != 1 || len(failures) != 0 {
		t.Errorf("batch headers: got=%v failures=%v", got, failures)
	}
	if got, failures := sw.FetchHeadersMany(t.Context(), []string{"missing"}); len(got) != 0 || len(failures) != 1 {
		t.Errorf("missing batch headers: got=%v failures=%v", got, failures)
	}
	sw.Swap(bareBox{newMemBox(map[string]string{"b.eml": "body"})})
	if got, failures := sw.FetchHeadersMany(t.Context(), []string{"b.eml"}); len(got) != 0 || len(failures) != 1 {
		t.Errorf("headers unsupported: got=%v failures=%v", got, failures)
	}
	sw.Swap(b)
	if _, err := sw.InFolder(t.Context(), "INBOX"); err != nil {
		t.Errorf("infolder: %v", err)
	}
}

type selfAddressSender struct{ countingSender }

func (selfAddressSender) From() string { return "sender@example.org" }

func TestSwitchableSenderFromTracksCurrentSender(t *testing.T) {
	sender := application.NewSwitchableSender(&selfAddressSender{})
	if got := sender.From(); got != "sender@example.org" {
		t.Fatalf("From() = %q", got)
	}
	sender.Swap(&countingSender{})
	if got := sender.From(); got != "" {
		t.Fatalf("From() without address = %q", got)
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
