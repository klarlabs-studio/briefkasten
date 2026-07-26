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
	// Cc are carbon-copy recipients. They are rendered into the message,
	// so every recipient can see them.
	Cc []string `json:"cc,omitempty"`
	// Bcc are blind carbon-copy recipients. They reach the envelope and
	// nothing else: see Recipients and RenderRFC5322.
	Bcc []string `json:"bcc,omitempty"`
	// HTMLBody, when set, is sent as an alternative representation alongside
	// the plain-text Body (multipart/alternative).
	HTMLBody string `json:"html_body,omitempty"`
	// InReplyTo is the Message-Id of the message this one answers, and
	// References is the thread's id chain ending in that same id. Both
	// are empty on a message that starts a thread — and on a reply whose
	// original carried no Message-Id, which is the one case where the
	// honest answer is no threading rather than an invented parent (see
	// DeriveReply).
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	// Attachments are files delivered with the message (multipart/mixed).
	Attachments []Attachment `json:"attachments,omitempty"`
	// State is the lifecycle state: queued, sending, sent, failed.
	State string `json:"state"`
	// Error holds the last delivery failure, when State is failed.
	Error string `json:"error,omitempty"`
	// Attempts counts delivery attempts.
	Attempts int `json:"attempts"`
}

// Recipients is the envelope recipient list: To, Cc and Bcc together,
// deduplicated on the addr-spec.
//
// This — not the rendered headers — is what a transport hands to RCPT
// TO, and the difference between the two is the whole of what Bcc means.
// A blind copy is one that reaches its recipient without any other
// recipient learning it did, which is only true while the address
// travels in the envelope and nowhere else.
func (m OutboundMessage) Recipients() []string {
	return newAddrSet().take(m.To, m.Cc, m.Bcc)
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

// ValidateAddresses runs a whole recipient group through
// ValidateAddress.
//
// Derived recipients go through it for the same reason caller-supplied
// ones do, and it is worth being explicit about why: an address lifted
// out of a reply-all set is not the operator's data, it is whatever the
// sender of the original message put in a To or Cc header. The
// derivation reads attacker-controllable text, so it validates it.
func ValidateAddresses(addrs []string) error {
	for _, addr := range addrs {
		if err := ValidateAddress(addr); err != nil {
			return err
		}
	}
	return nil
}

// ValidateMessageID rejects a value that could not have come from a real
// Message-Id header.
//
// It is written verbatim into In-Reply-To and References, so CR/LF would
// forge headers exactly as it would in an address — and the value is not
// the outbox's own: it is copied out of a message anyone could have
// sent. A msg-id is one angle-bracketed token, so anything with interior
// whitespace or further brackets is two values wearing one field's
// clothes.
func ValidateMessageID(id string) error {
	if strings.ContainsAny(id, "\r\n") {
		return fmt.Errorf("outbox: message id %q contains line breaks", id)
	}
	if len(id) < 3 || !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") {
		return fmt.Errorf("outbox: message id %q is not angle-bracketed", id)
	}
	if strings.ContainsAny(id[1:len(id)-1], "<> \t") {
		return fmt.Errorf("outbox: message id %q is not a single token", id)
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
	// Cc and Bcc are validated exactly as To is. A Bcc never reaches a
	// rendered header, but it does reach the SMTP envelope, and an
	// address carrying CR/LF is a forged command there just as it is a
	// forged header here.
	for _, group := range [][]string{m.To, m.Cc, m.Bcc} {
		if err := ValidateAddresses(group); err != nil {
			return err
		}
	}
	if m.InReplyTo != "" {
		if err := ValidateMessageID(m.InReplyTo); err != nil {
			return err
		}
	}
	for _, ref := range m.References {
		if err := ValidateMessageID(ref); err != nil {
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

// SelfAddresser is an optional Sender capability: naming the address
// mail leaves from.
//
// It exists because a reply has to know who "we" are before it can work
// out who everyone else is — the one address that must never end up in a
// derived recipient set is our own. The transport already holds that
// value (it is the From: the renderer stamps), so it is asked rather
// than threaded separately through the outbox and every interface, which
// is how the two would drift apart. A transport that cannot name itself
// simply loses the self-exclusion; the cost is a copy of your own reply,
// not a message going somewhere it should not.
type SelfAddresser interface {
	From() string
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
