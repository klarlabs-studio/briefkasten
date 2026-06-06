package smtp

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"go.klarlabs.de/briefkasten/domain"

	"github.com/emersion/go-smtp"
)

// memSMTP captures delivered messages.
type memSMTP struct {
	mu       sync.Mutex
	from     string
	to       []string
	data     string
	failNext bool
}

type memSMTPSession struct{ s *memSMTP }

func (m *memSMTPSession) Reset()        {}
func (m *memSMTPSession) Logout() error { return nil }

func (m *memSMTPSession) Mail(from string, _ *smtp.MailOptions) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if m.s.failNext {
		m.s.failNext = false
		return &smtp.SMTPError{Code: 451, Message: "transient failure"}
	}
	m.s.from = from
	return nil
}

func (m *memSMTPSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	m.s.to = append(m.s.to, to)
	return nil
}

func (m *memSMTPSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	m.s.data = string(raw)
	return nil
}

func startSMTPServer(t *testing.T, backend *memSMTP) string {
	t.Helper()
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &memSMTPSession{s: backend}, nil
	}))
	srv.Domain = "localhost"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestSMTPSenderDelivers(t *testing.T) {
	backend := &memSMTP{}
	addr := startSMTPServer(t, backend)

	sender, err := NewSender(Config{
		Addr:     addr,
		From:     "nexa@local.example",
		Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), domain.OutboundMessage{
		ID:      "m-7",
		To:      []string{"steuerberater@kanzlei.example"},
		Subject: "Belege 2025",
		Body:    "Anbei die Belege.",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.from != "nexa@local.example" {
		t.Errorf("from = %q", backend.from)
	}
	if len(backend.to) != 1 || backend.to[0] != "steuerberater@kanzlei.example" {
		t.Errorf("to = %v", backend.to)
	}
	for _, want := range []string{"Subject: ", "Anbei die Belege."} {
		if !strings.Contains(backend.data, want) {
			t.Errorf("data missing %q:\n%s", want, backend.data)
		}
	}
}

func TestSMTPSenderRetriesTransientFailure(t *testing.T) {
	backend := &memSMTP{failNext: true}
	addr := startSMTPServer(t, backend)

	sender, err := NewSender(Config{Addr: addr, From: "nexa@local.example", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}

	// First MAIL FROM fails transiently; fortify retry should recover.
	err = sender.Send(context.Background(), domain.OutboundMessage{
		ID: "m-8", To: []string{"a@b.c"}, Subject: "x", Body: "y",
	})
	if err != nil {
		t.Fatalf("Send after transient failure: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.data == "" {
		t.Error("message not delivered after retry")
	}
}

func TestSMTPSenderConfigValidation(t *testing.T) {
	if _, err := NewSender(Config{From: "x@y.z"}); err == nil {
		t.Error("missing addr accepted")
	}
	if _, err := NewSender(Config{Addr: "h:25"}); err == nil {
		t.Error("missing from accepted")
	}
}

func TestSMTPSenderUnreachableServerFails(t *testing.T) {
	sender, err := NewSender(Config{Addr: "127.0.0.1:1", From: "x@y.z", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), domain.OutboundMessage{
		ID: "m-9", To: []string{"a@b.c"}, Subject: "x", Body: "y",
	}); err == nil {
		t.Error("unreachable server: want error")
	}
}
