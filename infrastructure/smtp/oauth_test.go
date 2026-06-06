package smtp

import (
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"go.klarlabs.de/briefkasten/domain"
	briefauth "go.klarlabs.de/briefkasten/infrastructure/auth"
)

// oauthSMTPSession accepts OAUTHBEARER with one specific token.
type oauthSMTPSession struct {
	memSMTPSession
	token string
}

func (s *oauthSMTPSession) AuthMechanisms() []string { return []string{"OAUTHBEARER"} }

func (s *oauthSMTPSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		if opts.Token != s.token {
			return &sasl.OAuthBearerError{Status: "invalid_token"}
		}
		return nil
	}), nil
}

func TestSMTPSenderOAuthBearer(t *testing.T) {
	backend := &memSMTP{}
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &oauthSMTPSession{memSMTPSession: memSMTPSession{s: backend}, token: "tok-987"}, nil
	}))
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	sender, err := NewSender(Config{
		Addr:     ln.Addr().String(),
		From:     "alice@example.org",
		Username: "alice@example.org",
		Insecure: true,
		OAuth2:   &briefauth.OAuth2Settings{AccessToken: "tok-987", Mechanism: "oauthbearer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(t.Context(), domain.OutboundMessage{
		ID: "o1", To: []string{"x@y.z"}, Subject: "OAuth", Body: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !strings.Contains(backend.data, "OAuth") {
		t.Errorf("not delivered: %q", backend.data)
	}

	// Wrong token fails.
	bad, _ := NewSender(Config{
		Addr: ln.Addr().String(), From: "alice@example.org", Username: "alice@example.org",
		Insecure: true,
		OAuth2:   &briefauth.OAuth2Settings{AccessToken: "wrong", Mechanism: "oauthbearer"},
	})
	if err := bad.Send(t.Context(), domain.OutboundMessage{ID: "o2", To: []string{"x@y.z"}, Subject: "x", Body: "y"}); err == nil {
		t.Error("wrong token accepted")
	}
}
