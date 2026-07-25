package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
	// rcptErr, when set, is the reply to every RCPT TO.
	rcptErr *smtp.SMTPError
	// attempts counts MAIL FROM commands, i.e. delivery attempts that
	// reached the server.
	attempts int
	// delivered counts messages accepted at end-of-DATA.
	delivered int
}

type memSMTPSession struct{ s *memSMTP }

func (m *memSMTPSession) Reset()        {}
func (m *memSMTPSession) Logout() error { return nil }

func (m *memSMTPSession) Mail(from string, _ *smtp.MailOptions) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	m.s.attempts++
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
	if m.s.rcptErr != nil {
		return m.s.rcptErr
	}
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
	m.s.delivered++
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

// hangUpAfterDataServer speaks just enough SMTP to accept one message and
// then drops the connection instead of answering QUIT. go-smtp's server
// always completes QUIT, so reproducing a connection lost during teardown
// needs a scripted one.
type hangUpAfterDataServer struct {
	addr      string
	mu        sync.Mutex
	delivered int
}

func startHangUpAfterDataServer(t *testing.T) *hangUpAfterDataServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &hangUpAfterDataServer{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *hangUpAfterDataServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	reply := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }

	reply("220 localhost ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "DATA") {
			// EHLO/HELO/MAIL/RCPT all get a bare 250; advertising no
			// extensions keeps the client on the plain command path.
			reply("250 localhost")
			continue
		}
		reply("354 End data with <CR><LF>.<CR><LF>")
		for {
			body, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimRight(body, "\r\n") == "." {
				break
			}
		}
		s.mu.Lock()
		s.delivered++
		s.mu.Unlock()
		reply("250 2.0.0 Ok: queued")
		// Accepted — now vanish, so the client's QUIT fails.
		return
	}
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
	if backend.attempts != 2 {
		t.Errorf("attempts = %d, want 2 (4xx must be retried once)", backend.attempts)
	}
	if backend.delivered != 1 {
		t.Errorf("delivered = %d, want 1", backend.delivered)
	}
}

func TestSMTPSenderDoesNotRetryPermanentRejection(t *testing.T) {
	backend := &memSMTP{rcptErr: &smtp.SMTPError{Code: 550, Message: "no such user"}}
	addr := startSMTPServer(t, backend)

	sender, err := NewSender(Config{Addr: addr, From: "nexa@local.example", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(t.Context(), domain.OutboundMessage{
		ID: "m-10", To: []string{"ghost@kanzlei.example"}, Subject: "x", Body: "y",
	})
	if err == nil {
		t.Fatal("permanent rejection: want error")
	}
	if !errors.Is(err, errPermanent) {
		t.Errorf("error not classified permanent: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.attempts != 1 {
		t.Errorf("attempts = %d, want 1 (5xx must not be retried)", backend.attempts)
	}
}

func TestSMTPSenderQuitFailureDoesNotResend(t *testing.T) {
	srv := startHangUpAfterDataServer(t)

	sender, err := NewSender(Config{Addr: srv.addr, From: "nexa@local.example", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}

	// The server accepted DATA and then vanished, so QUIT fails. The mail is
	// already the server's responsibility: the send must succeed and must not
	// be attempted again.
	if err := sender.Send(t.Context(), domain.OutboundMessage{
		ID: "m-11", To: []string{"a@b.c"}, Subject: "x", Body: "y",
	}); err != nil {
		t.Errorf("Send with broken QUIT: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.delivered != 1 {
		t.Errorf("delivered = %d, want 1 (QUIT failure must not resend)", srv.delivered)
	}
}

func TestClassifyReplyCodes(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"5xx reply", &smtp.SMTPError{Code: 550, Message: "mailbox unavailable"}, true},
		{"4xx reply", &smtp.SMTPError{Code: 451, Message: "try later"}, false},
		{"wrapped 5xx", fmt.Errorf("smtp send: %w", &smtp.SMTPError{Code: 552, Message: "too big"}), true},
		{"transport error", io.ErrUnexpectedEOF, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(classify(tc.err), errPermanent); got != tc.permanent {
				t.Errorf("permanent = %v, want %v", got, tc.permanent)
			}
			// Whatever the verdict, the original error stays inspectable.
			if !errors.Is(classify(tc.err), tc.err) {
				t.Error("classify dropped the underlying error")
			}
		})
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
