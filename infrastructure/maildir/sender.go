package maildir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.klarlabs.de/briefkasten/domain"
)

// Sender delivers messages as .eml files into a maildir-style new/
// directory — the outbound twin of DirMailbox. Local-first: point it at
// another briefkasten's maildir and the loop closes without a mail server.
type Sender struct {
	root string
	from string
}

// NewSender creates the target tmp/ and new/ directories and binds the
// From address.
func NewSender(root, from string) (*Sender, error) {
	if from == "" {
		return nil, fmt.Errorf("dirsender: From address is required")
	}
	if err := domain.ValidateAddress(from); err != nil {
		return nil, fmt.Errorf("dirsender: %w", err)
	}
	for _, sub := range []string{"tmp", "new"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return nil, fmt.Errorf("dirsender init: %w", err)
		}
	}
	return &Sender{root: root, from: from}, nil
}

// Send writes the message as RFC 5322 into new/<id>.eml — via tmp/ and an
// atomic rename, per the maildir protocol, so readers and fsnotify
// watchers never observe a partially written message.
func (d *Sender) Send(_ context.Context, msg domain.OutboundMessage) error {
	raw, err := domain.RenderRFC5322(d.from, msg, time.Now())
	if err != nil {
		return fmt.Errorf("dirsender render: %w", err)
	}
	tmp := filepath.Join(d.root, "tmp", msg.ID+".eml")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("dirsender write: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(d.root, "new", msg.ID+".eml")); err != nil {
		return fmt.Errorf("dirsender deliver: %w", err)
	}
	return nil
}

// From reports the bound sender address — the address a reply derived
// here must never send to. See domain.SelfAddresser.
func (d *Sender) From() string { return d.from }

var (
	_ domain.Sender        = (*Sender)(nil)
	_ domain.SelfAddresser = (*Sender)(nil)
)
