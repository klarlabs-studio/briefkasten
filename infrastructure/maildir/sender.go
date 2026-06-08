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

// NewSender creates the target new/ directory and binds the From address.
func NewSender(root, from string) (*Sender, error) {
	if from == "" {
		return nil, fmt.Errorf("dirsender: From address is required")
	}
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o700); err != nil {
		return nil, fmt.Errorf("dirsender init: %w", err)
	}
	return &Sender{root: root, from: from}, nil
}

// Send writes the message as RFC 5322 into new/<id>.eml.
func (d *Sender) Send(_ context.Context, msg domain.OutboundMessage) error {
	path := filepath.Join(d.root, "new", msg.ID+".eml")
	if err := os.WriteFile(path, domain.RenderRFC5322(d.from, msg, time.Now()), 0o600); err != nil {
		return fmt.Errorf("dirsender write: %w", err)
	}
	return nil
}

var _ domain.Sender = (*Sender)(nil)
