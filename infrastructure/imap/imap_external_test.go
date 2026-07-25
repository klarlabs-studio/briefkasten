package imap_test

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"go.klarlabs.de/briefkasten/domain"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	bimap "go.klarlabs.de/briefkasten/infrastructure/imap"
)

const testMessage = "From: amt@finanzamt.example\r\nSubject: Bescheid\r\n\r\nSehr geehrte Damen und Herren,\r\n"

type literal struct {
	*bytes.Reader
	size int64
}

func (l literal) Size() int64 { return l.size }

// commandCounter tallies the IMAP commands clients send, so a test can
// assert what an operation costs in round trips rather than only that it
// worked. Batching is invisible in the result — the mail lands either
// way — and is only observable here.
type commandCounter struct {
	mu     sync.Mutex
	counts map[string]int
	// lines keeps the commands whole as well as tallied. Two UID FETCHes
	// can differ entirely in what they cost — one asks for RFC822.SIZE,
	// the other streams every body — and the name alone cannot tell them
	// apart, which is exactly what a size pre-flight must be checked on.
	lines []string
}

func newCommandCounter() *commandCounter { return &commandCounter{counts: map[string]int{}} }

// observe records one command line ("<tag> UID COPY 1:5 Trash").
func (c *commandCounter) observe(line string) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return
	}
	name := strings.ToUpper(fields[1])
	if name == "UID" && len(fields) >= 3 {
		name += " " + strings.ToUpper(fields[2])
	}
	c.mu.Lock()
	c.counts[name]++
	c.lines = append(c.lines, line)
	c.mu.Unlock()
}

func (c *commandCounter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

// matching counts the commands whose text contains needle, so a test can
// ask what a command actually requested rather than only which verb it
// used.
func (c *commandCounter) matching(needle string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, line := range c.lines {
		if strings.Contains(strings.ToUpper(line), strings.ToUpper(needle)) {
			n++
		}
	}
	return n
}

// reset zeroes the tally so a test can measure one operation rather than
// everything the connection has done since it was dialled.
func (c *commandCounter) reset() {
	c.mu.Lock()
	c.counts = map[string]int{}
	c.lines = nil
	c.mu.Unlock()
}

// countingConn tallies the command lines a client sends. Each connection
// has its own line buffer — the server reads it from one goroutine — and
// only the shared tally is locked.
type countingConn struct {
	net.Conn
	counter *commandCounter
	pending []byte
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.pending = append(c.pending, p[:n]...)
		for {
			i := bytes.IndexByte(c.pending, '\n')
			if i < 0 {
				break
			}
			c.counter.observe(string(bytes.TrimRight(c.pending[:i], "\r")))
			c.pending = c.pending[i+1:]
		}
	}
	return n, err
}

// countingListener records how many connections a server accepted, what
// commands arrived on them, and can sever the live ones, so a test can
// tell a reused connection from a fresh dial and can play the server
// that drops an idle session.
type countingListener struct {
	net.Listener

	commands *commandCounter

	mu    sync.Mutex
	dials int
	conns []net.Conn
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	counted := &countingConn{Conn: conn, counter: l.commands}
	l.mu.Lock()
	l.dials++
	l.conns = append(l.conns, counted)
	l.mu.Unlock()
	return counted, nil
}

func (l *countingListener) accepted() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dials
}

// cut closes every connection the server accepted, the way a server or a
// NAT box drops a session that sat idle.
func (l *countingListener) cut() {
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// startIMAPServer runs an in-memory IMAP server with one user and one
// unseen message in INBOX, plus any extra mailboxes named by the caller.
// Returns the listen address.
//
// Note the memory server cannot declare SPECIAL-USE — its Create ignores
// imap.CreateOptions.SpecialUse and the attribute list is unexported —
// so folder discovery via \Trash/\Archive is covered by the unit tests
// over chooseCurationFolder instead.
func startIMAPServer(t *testing.T, extraMailboxes ...string) string {
	t.Helper()
	addr, _ := startCountedIMAPServer(t, extraMailboxes...)
	return addr
}

// startCountedIMAPServer is startIMAPServer with the listener exposed.
func startCountedIMAPServer(t *testing.T, extraMailboxes ...string) (string, *countingListener) {
	t.Helper()
	return startSeededIMAPServer(t, 1, extraMailboxes...)
}

// startSeededIMAPServer runs the in-memory server with messages unseen
// messages in INBOX — the shape a bulk test needs, where one message
// cannot show whether a batch was batched.
func startSeededIMAPServer(t *testing.T, messages int, extraMailboxes ...string) (string, *countingListener) {
	t.Helper()
	bodies := make([][]byte, messages)
	for i := range bodies {
		bodies[i] = []byte(testMessage)
	}
	return startMessagesIMAPServer(t, bodies, extraMailboxes...)
}

// startMessagesIMAPServer seeds the exact messages given, so a test can
// control what the mailbox weighs as well as how much of it there is.
func startMessagesIMAPServer(t *testing.T, bodies [][]byte, extraMailboxes ...string) (string, *countingListener) {
	t.Helper()

	user := imapmemserver.NewUser("alice", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range extraMailboxes {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range bodies {
		if _, err := user.Append("INBOX", literal{bytes.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	mem := imapmemserver.New()
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &countingListener{Listener: base, commands: newCommandCounter()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String(), ln
}

func newTestIMAPMailbox(t *testing.T, addr string) *bimap.Mailbox {
	t.Helper()
	mb, err := bimap.New(bimap.Config{
		Addr:     addr,
		Username: "alice",
		Password: "secret",
		Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mb
}

func TestIMAPMailboxRoundTrip(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	ids, err := mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("unread = %v, want one id", ids)
	}

	raw, err := mb.Fetch(t.Context(), ids[0])
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(raw) != testMessage {
		t.Errorf("raw = %q, want %q", raw, testMessage)
	}

	// Fetch must peek: message stays unread.
	ids, err = mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread after fetch: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("unread after fetch = %v, want still one (BODY.PEEK)", ids)
	}

	if err := mb.MarkSeen(t.Context(), ids[0]); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	ids, err = mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread after seen: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("unread after seen = %v, want none", ids)
	}
}

func TestIMAPMailboxBadID(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	if _, err := mb.Fetch(t.Context(), "not-a-uid"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Fetch(not-a-uid) err = %v, want ErrBadID", err)
	}
	if _, err := mb.Fetch(t.Context(), "999"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("Fetch(999) err = %v, want ErrBadID", err)
	}
	if err := mb.MarkSeen(t.Context(), "not-a-uid"); !errors.Is(err, domain.ErrBadID) {
		t.Errorf("MarkSeen(not-a-uid) err = %v, want ErrBadID", err)
	}
}

// MarkSeen is the one call an agent makes after it has finished with a
// message, so it must never claim success for a message it did not
// touch: an unknown UID is a caller mistake (ErrBadID), while marking a
// message that is already seen stays a success.
func TestIMAPMarkSeen(t *testing.T) {
	tests := []struct {
		name    string
		id      string // empty means the UID of the one message in INBOX
		markTwc bool
		wantErr error
	}{
		{name: "unseen message"},
		{name: "already seen message is idempotent", markTwc: true},
		{name: "uid not in the mailbox", id: "4242", wantErr: domain.ErrBadID},
		{name: "not a uid at all", id: "not-a-uid", wantErr: domain.ErrBadID},
		{name: "uid zero", id: "0", wantErr: domain.ErrBadID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mb := newTestIMAPMailbox(t, startIMAPServer(t))
			unread, err := mb.ListUnread(t.Context())
			if err != nil || len(unread) != 1 {
				t.Fatalf("ListUnread = %v, err %v, want one id", unread, err)
			}
			id := tt.id
			if id == "" {
				id = unread[0]
			}
			if tt.markTwc {
				if err := mb.MarkSeen(t.Context(), id); err != nil {
					t.Fatalf("first MarkSeen: %v", err)
				}
			}

			err = mb.MarkSeen(t.Context(), id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("MarkSeen(%s) error = %v, want %v", id, err, tt.wantErr)
				}
				// A rejected id must leave the real message alone.
				if still, err := mb.ListUnread(t.Context()); err != nil || len(still) != 1 {
					t.Fatalf("unread after rejected MarkSeen = %v, err %v, want the message untouched", still, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MarkSeen(%s): %v", id, err)
			}
			if still, err := mb.ListUnread(t.Context()); err != nil || len(still) != 0 {
				t.Fatalf("unread after MarkSeen = %v, err %v, want none", still, err)
			}
		})
	}
}

// The mailbox keeps one authenticated connection rather than dialling
// and logging in per operation — providers rate-limit on login frequency
// — but must survive that connection being dropped underneath it.
func TestIMAPConnectionReuse(t *testing.T) {
	addr, ln := startCountedIMAPServer(t)
	mb := newTestIMAPMailbox(t, addr)

	for i := range 3 {
		if _, err := mb.ListUnread(t.Context()); err != nil {
			t.Fatalf("ListUnread %d: %v", i, err)
		}
	}
	if got := ln.accepted(); got != 1 {
		t.Errorf("connections after three listings = %d, want 1 (reused)", got)
	}

	// Mixed operations share the same connection too.
	if _, err := mb.Folders(t.Context()); err != nil {
		t.Fatalf("Folders: %v", err)
	}
	ids, err := mb.ListUnread(t.Context())
	if err != nil {
		t.Fatalf("ListUnread: %v", err)
	}
	if _, err := mb.Fetch(t.Context(), ids[0]); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := ln.accepted(); got != 1 {
		t.Errorf("connections after mixed operations = %d, want 1 (reused)", got)
	}

	// A severed session must be discarded rather than handed out: the
	// next call re-dials instead of failing.
	ln.cut()
	if _, err := mb.ListUnread(t.Context()); err != nil {
		t.Fatalf("ListUnread after the server dropped the session: %v", err)
	}
	if got := ln.accepted(); got != 2 {
		t.Errorf("connections after a dropped session = %d, want 2 (re-dialled once)", got)
	}
	if err := mb.MarkSeen(t.Context(), ids[0]); err != nil {
		t.Fatalf("MarkSeen after the server dropped the session: %v", err)
	}
}

// The mailbox is shared across MCP requests, so concurrent callers must
// never end up on the same connection.
func TestIMAPConcurrentOperations(t *testing.T) {
	mb := newTestIMAPMailbox(t, startIMAPServer(t))

	ops := map[string]func() error{
		"list":    func() error { _, err := mb.ListUnread(t.Context()); return err },
		"folders": func() error { _, err := mb.Folders(t.Context()); return err },
		"search":  func() error { _, err := mb.Search(t.Context(), "Bescheid"); return err },
		"plan":    func() error { _, err := mb.CurationPlan(t.Context()); return err },
	}

	var wg sync.WaitGroup
	for name, op := range ops {
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := op(); err != nil {
					t.Errorf("concurrent %s: %v", name, err)
				}
			}()
		}
	}
	wg.Wait()
}

func TestIMAPMailboxBadCredentials(t *testing.T) {
	addr := startIMAPServer(t)
	mb, err := bimap.New(bimap.Config{
		Addr:     addr,
		Username: "alice",
		Password: "wrong",
		Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.ListUnread(t.Context()); err == nil {
		t.Error("ListUnread with bad credentials: want error")
	}
}

func TestNewIMAPMailboxValidation(t *testing.T) {
	if _, err := bimap.New(bimap.Config{}); err == nil {
		t.Error("empty config: want error")
	}
}

func TestIMAPCapabilities(t *testing.T) {
	user := imapmemserver.NewUser("alice", "secret")
	for _, name := range []string{"INBOX", "Steuern"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{
		"From: drk@spenden.example\r\nSubject: Spende\r\n\r\nDanke",
		"From: shop@example.org\r\nSubject: Rechnung\r\n\r\nBetrag",
	} {
		raw := []byte(body)
		if _, err := user.Append("INBOX", literal{bytes.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	mb, err := bimap.New(bimap.Config{Addr: ln.Addr().String(), Username: "alice", Password: "secret", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}

	// Search server-side.
	ids, err := mb.Search(t.Context(), "Rechnung")
	if err != nil || len(ids) != 1 {
		t.Errorf("search = %v err %v", ids, err)
	}

	// Folders + scoped instance.
	folders, err := mb.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range folders {
		have[f] = true
	}
	if !have["INBOX"] || !have["Steuern"] {
		t.Errorf("folders = %v", folders)
	}
	if _, err := mb.InFolder(t.Context(), ""); err == nil {
		t.Error("empty folder accepted")
	}
	scoped, err := mb.InFolder(t.Context(), "Steuern")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.ListUnread(t.Context()); err != nil {
		t.Errorf("scoped list: %v", err)
	}

	// Curation: archive one, delete one — both soft (copy + seen).
	all, _ := mb.ListUnread(t.Context())
	if err := mb.Archive(t.Context(), all[0]); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := mb.Delete(t.Context(), all[1]); err != nil {
		t.Fatalf("delete: %v", err)
	}
	remaining, _ := mb.ListUnread(t.Context())
	if len(remaining) != 0 {
		t.Errorf("unread after curation = %v", remaining)
	}
	folders, _ = mb.Folders(t.Context())
	have = map[string]bool{}
	for _, f := range folders {
		have[f] = true
	}
	if !have["Archive"] || !have["Trash"] {
		t.Errorf("Archive/Trash not created: %v", folders)
	}

	// Bad ids on curation.
	if err := mb.Archive(t.Context(), "zero"); err == nil {
		t.Error("bad archive id accepted")
	}
	if err := mb.Delete(t.Context(), "0"); err == nil {
		t.Error("bad delete id accepted")
	}
}
