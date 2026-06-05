// Package auth holds authentication infrastructure: OAuth2 token
// sourcing and the XOAUTH2 SASL mechanism for IMAP/SMTP.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/emersion/go-sasl"
	"golang.org/x/oauth2"
)

// OAuth2Settings configures token-based IMAP/SMTP authentication —
// Gmail and Outlook are phasing out app passwords; XOAUTH2/OAUTHBEARER
// is the supported path.
//
// Provide either a static AccessToken (tests, short-lived use) or the
// refresh-token triple (ClientID, ClientSecret, RefreshToken, TokenURL)
// — tokens are then minted and refreshed automatically.
type OAuth2Settings struct {
	// ClientID / ClientSecret identify the OAuth app.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// RefreshToken is the long-lived grant obtained once via the
	// provider's consent flow.
	RefreshToken string `yaml:"refresh_token"`
	// TokenURL is the provider's token endpoint
	// (e.g. https://oauth2.googleapis.com/token).
	TokenURL string `yaml:"token_url"`
	// AccessToken short-circuits the refresh flow with a static token.
	AccessToken string `yaml:"access_token"`
	// Mechanism selects the SASL mechanism: "xoauth2" (default — Gmail,
	// Outlook) or "oauthbearer" (RFC 7628).
	Mechanism string `yaml:"mechanism"`

	source oauth2.TokenSource
}

// Token returns a current access token, refreshing as needed.
func (o *OAuth2Settings) Token(ctx context.Context) (string, error) {
	if o.AccessToken != "" && o.RefreshToken == "" {
		return o.AccessToken, nil
	}
	if o.RefreshToken == "" {
		return "", errors.New("oauth2: access_token or refresh_token required")
	}
	if o.source == nil {
		cfg := &oauth2.Config{
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			Endpoint:     oauth2.Endpoint{TokenURL: o.TokenURL},
		}
		o.source = cfg.TokenSource(context.Background(), &oauth2.Token{RefreshToken: o.RefreshToken})
	}
	tok, err := o.source.Token()
	if err != nil {
		return "", fmt.Errorf("oauth2: token: %w", err)
	}
	return tok.AccessToken, nil
}

// SASLClient builds the SASL client for the configured mechanism.
func (o *OAuth2Settings) SASLClient(ctx context.Context, username, host string, port int) (sasl.Client, error) {
	tok, err := o.Token(ctx)
	if err != nil {
		return nil, err
	}
	switch o.Mechanism {
	case "", "xoauth2":
		return NewXOAuth2Client(username, tok), nil
	case "oauthbearer":
		return sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: username,
			Token:    tok,
			Host:     host,
			Port:     port,
		}), nil
	default:
		return nil, fmt.Errorf("oauth2: unknown mechanism %q (want xoauth2 or oauthbearer)", o.Mechanism)
	}
}

// SplitHostPort splits addr into host and port, with a default port.
func SplitHostPort(addr string, defaultPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, defaultPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defaultPort
	}
	return host, port
}

// xoauth2Client implements the XOAUTH2 SASL mechanism (Gmail, Outlook):
// a single initial response "user=<u>\x01auth=Bearer <t>\x01\x01"; on an
// error challenge the client answers empty so the server can fail the
// exchange cleanly.
type xoauth2Client struct {
	username string
	token    string
	failed   bool
}

// NewXOAuth2Client builds an XOAUTH2 SASL client for the user and token.
func NewXOAuth2Client(username, token string) sasl.Client {
	return &xoauth2Client{username: username, token: token}
}

// Start begins the exchange with the single XOAUTH2 initial response.
func (c *xoauth2Client) Start() (string, []byte, error) {
	ir := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)
	return "XOAUTH2", []byte(ir), nil
}

// Next answers an error challenge with an empty response, then fails.
func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	if c.failed {
		return nil, errors.New("xoauth2: authentication failed: " + string(challenge))
	}
	c.failed = true
	return []byte{}, nil
}
