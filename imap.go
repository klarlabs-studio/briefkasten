package briefkasten

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// IMAPConfig configures an IMAPMailbox.
type IMAPConfig struct {
	// Addr is the IMAP server address (host:port). Required.
	Addr string
	// Username and Password authenticate via LOGIN.
	Username string
	Password string
	// Mailbox is the mailbox to read. Defaults to "INBOX".
	Mailbox string
	// Insecure dials without TLS. For tests and local servers only.
	Insecure bool
	// TLSConfig optionally overrides the TLS client configuration.
	TLSConfig *tls.Config
}

// IMAPMailbox is a Mailbox backed by an IMAP server (go-imap v2).
//
// Ids are message UIDs in the configured mailbox. Each call dials a fresh
// connection and logs out afterwards — no connection state is kept, so the
// mailbox survives server restarts and idle timeouts.
//
// ListUnread issues UID SEARCH UNSEEN, Fetch reads BODY.PEEK[] (the \Seen
// flag is NOT set by fetching), and MarkSeen stores +FLAGS \Seen — seen
// messages simply stop being listed; nothing is ever deleted.
type IMAPMailbox struct {
	cfg IMAPConfig
}

// NewIMAPMailbox validates cfg and returns an IMAPMailbox.
func NewIMAPMailbox(cfg IMAPConfig) (*IMAPMailbox, error) {
	if cfg.Addr == "" {
		return nil, errors.New("imap: Addr is required")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	return &IMAPMailbox{cfg: cfg}, nil
}

// dial connects, logs in, and selects the configured mailbox.
func (m *IMAPMailbox) dial() (*imapclient.Client, error) {
	var (
		c   *imapclient.Client
		err error
	)
	if m.cfg.Insecure {
		c, err = imapclient.DialInsecure(m.cfg.Addr, nil)
	} else {
		c, err = imapclient.DialTLS(m.cfg.Addr, &imapclient.Options{TLSConfig: m.cfg.TLSConfig})
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", m.cfg.Addr, err)
	}
	if err := c.Login(m.cfg.Username, m.cfg.Password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: login: %w", err)
	}
	if _, err := c.Select(m.cfg.Mailbox, nil).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: select %s: %w", m.cfg.Mailbox, err)
	}
	return c, nil
}

func closeClient(c *imapclient.Client) {
	_ = c.Logout().Wait()
	_ = c.Close()
}

// ListUnread returns the UIDs of unseen messages.
func (m *IMAPMailbox) ListUnread() ([]string, error) {
	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer closeClient(c)

	data, err := c.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: search unseen: %w", err)
	}

	uids := data.AllUIDs()
	ids := make([]string, len(uids))
	for i, uid := range uids {
		ids[i] = strconv.FormatUint(uint64(uid), 10)
	}
	return ids, nil
}

// Fetch returns the raw RFC 5322 bytes of the message with the given UID.
// It peeks — fetching does not mark the message seen.
func (m *IMAPMailbox) Fetch(id string) ([]byte, error) {
	uid, err := parseUID(id)
	if err != nil {
		return nil, err
	}

	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer closeClient(c)

	section := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch %s: %w", id, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrBadID, id)
	}
	raw := msgs[0].FindBodySection(section)
	if raw == nil {
		return nil, fmt.Errorf("imap: fetch %s: no body section in response", id)
	}
	return raw, nil
}

// Search returns unseen UIDs matching the query (UID SEARCH UNSEEN TEXT).
func (m *IMAPMailbox) Search(query string) ([]string, error) {
	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer closeClient(c)

	data, err := c.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
		Text:    []string{query},
	}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: search: %w", err)
	}
	uids := data.AllUIDs()
	ids := make([]string, len(uids))
	for i, uid := range uids {
		ids[i] = strconv.FormatUint(uint64(uid), 10)
	}
	return ids, nil
}

var _ Searcher = (*IMAPMailbox)(nil)

// MarkSeen sets the \Seen flag on the message with the given UID.
func (m *IMAPMailbox) MarkSeen(id string) error {
	uid, err := parseUID(id)
	if err != nil {
		return err
	}

	c, err := m.dial()
	if err != nil {
		return err
	}
	defer closeClient(c)

	if err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}, nil).Close(); err != nil {
		return fmt.Errorf("imap: mark seen %s: %w", id, err)
	}
	return nil
}

func parseUID(id string) (imap.UID, error) {
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%w: %s", ErrBadID, id)
	}
	return imap.UID(n), nil
}

var _ Mailbox = (*IMAPMailbox)(nil)
