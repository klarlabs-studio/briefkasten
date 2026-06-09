// Package auth holds authentication infrastructure: OAuth2 token
// sourcing and the XOAUTH2 SASL mechanism for IMAP/SMTP.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/emersion/go-sasl"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// gmailScope is the full IMAP/SMTP access scope Google requires for
// XOAUTH2-authenticated mail (read and send).
const gmailScope = "https://mail.google.com/"

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

	// CredentialsFile points at a downloaded Google credentials JSON —
	// either a service-account key or an OAuth client secret (see
	// LoadCredentials). It saves hand-copying client_id/client_secret/token_url,
	// and a service-account key needs no refresh token at all.
	CredentialsFile string `yaml:"credentials_file"`
	// CredentialsJSON is the raw credentials bytes, an alternative to the file
	// (e.g. read from a secret manager). Not serialized.
	CredentialsJSON []byte `yaml:"-"`

	source oauth2.TokenSource
	loaded bool // LoadCredentials ran (idempotency guard)
}

// LoadCredentials hydrates the settings from a Google credentials file
// (CredentialsFile / CredentialsJSON) when one is configured; otherwise it is a
// no-op. It accepts BOTH kinds Google hands out:
//
//   - a service-account key (its type field reads service_account) —
//     server-to-server: the account impersonates `user` via domain-wide
//     delegation, so no refresh token is needed. Workspace only (a service
//     account cannot act for a consumer @gmail.com account).
//   - a downloaded OAuth client secret ({"web"|"installed":...}) — fills
//     ClientID / ClientSecret / TokenURL from the file; a RefreshToken (from the
//     provider's consent flow) is still required to mint tokens.
//
// `user` is the mailbox address (the IMAP/SMTP username) the tokens act for.
func (o *OAuth2Settings) LoadCredentials(ctx context.Context, user string) error {
	if o.loaded {
		return nil
	}
	raw := o.CredentialsJSON
	if len(raw) == 0 {
		if o.CredentialsFile == "" {
			o.loaded = true
			return nil
		}
		b, err := os.ReadFile(o.CredentialsFile) // #nosec G304 -- path is operator-supplied config
		if err != nil {
			return fmt.Errorf("oauth2: read credentials file: %w", err)
		}
		raw = b
	}
	var probe struct {
		Type      string          `json:"type"`
		Web       json.RawMessage `json:"web"`
		Installed json.RawMessage `json:"installed"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("oauth2: parse credentials file: %w", err)
	}
	switch {
	case probe.Type == "service_account":
		if user == "" {
			return errors.New("oauth2: a service-account credentials file requires a username to impersonate")
		}
		jwtCfg, err := google.JWTConfigFromJSON(raw, gmailScope)
		if err != nil {
			return fmt.Errorf("oauth2: parse service-account key: %w", err)
		}
		jwtCfg.Subject = user // domain-wide delegation impersonation
		o.source = jwtCfg.TokenSource(ctx)
	case len(probe.Web) > 0 || len(probe.Installed) > 0:
		cfg, err := google.ConfigFromJSON(raw, gmailScope)
		if err != nil {
			return fmt.Errorf("oauth2: parse OAuth client secret: %w", err)
		}
		if o.ClientID == "" {
			o.ClientID = cfg.ClientID
		}
		if o.ClientSecret == "" {
			o.ClientSecret = cfg.ClientSecret
		}
		if o.TokenURL == "" {
			o.TokenURL = cfg.Endpoint.TokenURL
		}
	default:
		return errors.New("oauth2: unrecognised credentials file (want a service-account key or an OAuth client secret)")
	}
	o.loaded = true
	return nil
}

// Token returns a current access token, refreshing as needed.
func (o *OAuth2Settings) Token(ctx context.Context) (string, error) {
	// A credentials-file-built source (service account) wins: it needs no
	// refresh token.
	if o.source == nil && o.RefreshToken != "" {
		cfg := &oauth2.Config{
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			Endpoint:     oauth2.Endpoint{TokenURL: o.TokenURL},
		}
		o.source = cfg.TokenSource(context.Background(), &oauth2.Token{RefreshToken: o.RefreshToken})
	}
	if o.source != nil {
		tok, err := o.source.Token()
		if err != nil {
			return "", fmt.Errorf("oauth2: token: %w", err)
		}
		return tok.AccessToken, nil
	}
	if o.AccessToken != "" {
		return o.AccessToken, nil
	}
	return "", errors.New("oauth2: access_token, refresh_token, or a credentials file required")
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
