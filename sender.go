package briefkasten

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DirSender delivers messages as .eml files into a maildir-style new/
// directory — the outbound twin of DirMailbox. Local-first: point it at
// another briefkasten's maildir and the loop closes without a mail server.
type DirSender struct {
	root string
	from string
}

// NewDirSender creates the target new/ directory and binds the From address.
func NewDirSender(root, from string) (*DirSender, error) {
	if from == "" {
		return nil, fmt.Errorf("dirsender: From address is required")
	}
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		return nil, fmt.Errorf("dirsender init: %w", err)
	}
	return &DirSender{root: root, from: from}, nil
}

// Send writes the message as RFC 5322 into new/<id>.eml.
func (d *DirSender) Send(_ context.Context, msg OutboundMessage) error {
	path := filepath.Join(d.root, "new", msg.ID+".eml")
	if err := os.WriteFile(path, buildRFC5322(d.from, msg, time.Now()), 0o644); err != nil {
		return fmt.Errorf("dirsender write: %w", err)
	}
	return nil
}

// buildRFC5322 renders an outbound message as a simple text/plain email.
func buildRFC5322(from string, msg OutboundMessage, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(msg.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: <%s@briefkasten>\r\n", msg.ID)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

var _ Sender = (*DirSender)(nil)
