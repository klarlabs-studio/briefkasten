package domain

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
)

// Size limits guard the outbox against unbounded messages. They bound the raw
// (pre-base64) attachment bytes; the encoded wire form is ~33% larger.
const (
	// MaxAttachmentBytes is the largest a single attachment may be.
	MaxAttachmentBytes = 10 << 20 // 10 MiB
	// MaxMessageBytes is the largest the body + all attachments may sum to.
	MaxMessageBytes = 25 << 20 // 25 MiB
)

// Attachment is a file carried by an outbound message. Content is the raw
// bytes; encoding/json marshals it as base64, which is also the wire form the
// MCP email.send tool accepts.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     []byte `json:"content"`
}

// OutboundMessage is one message in the outbox.
type OutboundMessage struct {
	ID      string   `json:"id"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
	// HTMLBody, when set, is sent as an alternative representation alongside
	// the plain-text Body (multipart/alternative).
	HTMLBody string `json:"html_body,omitempty"`
	// Attachments are files delivered with the message (multipart/mixed).
	Attachments []Attachment `json:"attachments,omitempty"`
	// State is the lifecycle state: queued, sending, sent, failed.
	State string `json:"state"`
	// Error holds the last delivery failure, when State is failed.
	Error string `json:"error,omitempty"`
	// Attempts counts delivery attempts.
	Attempts int `json:"attempts"`
}

// ValidateAddress rejects strings that are not a single RFC 5322 address.
// Parsing also rules out CR/LF, closing the header-injection door for
// values that are rendered into From/To headers verbatim.
func ValidateAddress(addr string) error {
	if strings.ContainsAny(addr, "\r\n") {
		return fmt.Errorf("outbox: address %q contains line breaks", addr)
	}
	if _, err := mail.ParseAddress(addr); err != nil {
		return fmt.Errorf("outbox: invalid address %q: %w", addr, err)
	}
	return nil
}

// ValidateContentType rejects attachment content types that are not a
// single well-formed MIME type. The value is written verbatim into a
// part header, so CR/LF would let a caller forge additional headers and
// a whole extra MIME part — the same header-injection door
// ValidateAddress closes for From/To.
func ValidateContentType(ctype string) error {
	if strings.ContainsAny(ctype, "\r\n") {
		return fmt.Errorf("outbox: content type %q contains line breaks", ctype)
	}
	if _, _, err := mime.ParseMediaType(ctype); err != nil {
		return fmt.Errorf("outbox: invalid content type %q: %w", ctype, err)
	}
	return nil
}

// Validate enforces the message invariants.
func (m OutboundMessage) Validate() error {
	if len(m.To) == 0 {
		return errors.New("outbox: message needs at least one recipient")
	}
	for _, to := range m.To {
		if err := ValidateAddress(to); err != nil {
			return err
		}
	}
	total := len(m.Body) + len(m.HTMLBody)
	for i, a := range m.Attachments {
		switch {
		case a.Filename == "":
			return fmt.Errorf("outbox: attachment %d has no filename", i)
		case a.ContentType == "":
			return fmt.Errorf("outbox: attachment %q has no content type", a.Filename)
		case len(a.Content) == 0:
			return fmt.Errorf("outbox: attachment %q is empty", a.Filename)
		case len(a.Content) > MaxAttachmentBytes:
			return fmt.Errorf("outbox: attachment %q is %d bytes, over the %d limit", a.Filename, len(a.Content), MaxAttachmentBytes)
		}
		if err := ValidateContentType(a.ContentType); err != nil {
			return fmt.Errorf("outbox: attachment %q: %w", a.Filename, err)
		}
		total += len(a.Content)
	}
	if total > MaxMessageBytes {
		return fmt.Errorf("outbox: message is %d bytes, over the %d limit", total, MaxMessageBytes)
	}
	return nil
}

// Sender delivers an outbound message — the outbound transport port.
type Sender interface {
	Send(ctx context.Context, msg OutboundMessage) error
}

// OutboxStore is the persistence port for the outbox: messages live in
// exactly one lifecycle state at a time.
type OutboxStore interface {
	// Write persists the message under its current state.
	Write(msg OutboundMessage) error
	// Remove deletes the message's record under the given state (the
	// message itself lives on under its new state after a move).
	Remove(state, id string) error
	// Find returns the message regardless of state.
	Find(id string) (OutboundMessage, error)
	// List returns the ids stored under one state.
	List(state string) ([]string, error)
}
