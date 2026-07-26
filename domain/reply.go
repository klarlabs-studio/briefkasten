package domain

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
)

// ErrNoReplyTarget refuses a reply that would have nobody to go to.
var ErrNoReplyTarget = errors.New("briefkasten: nobody to reply to")

// ErrForwardTooLarge refuses a forward whose original is over the
// attachment ceiling.
var ErrForwardTooLarge = errors.New("briefkasten: message too large to forward")

// MaxQuotedBytes caps how much of an original a reply or forward quotes
// inline.
//
// The attachment ceilings bound what a message carries; this bounds what
// it repeats. Quoting is a courtesy — a forward carries the whole
// original as an attachment regardless — so an original with a
// megabyte-long body is trimmed rather than allowed to double the size
// of every message in a thread.
const MaxQuotedBytes = 64 << 10 // 64 KiB

// Original is the parsed shape of a message a reply or a forward is
// derived from: the header fields the derivation reads, plus the raw
// bytes a forward carries along.
//
// One field is deliberately absent, and its absence is the point: Bcc.
// Two separate reasons converge on it. If we were Bcc'd on the original,
// we cannot see who else was, so there is nothing to derive. And where a
// Bcc list is somehow visible — a server that leaves the header on, a
// message pulled from the sender's own Sent folder — replying to it
// would broadcast a list the sender deliberately hid. Neither case has a
// reading under which copying Bcc into a reply is correct, so the field
// never enters the model in the first place.
type Original struct {
	MessageID  string
	References []string
	Subject    string
	Date       string
	From       []string
	ReplyTo    []string
	To         []string
	Cc         []string
	Raw        []byte
}

// ParseOriginal reads the header fields a reply or forward derives from
// out of a raw RFC 5322 message.
func ParseOriginal(raw []byte) (Original, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Original{}, fmt.Errorf("briefkasten: cannot parse the original message: %w", err)
	}
	return Original{
		MessageID:  strings.TrimSpace(msg.Header.Get("Message-Id")),
		References: strings.Fields(msg.Header.Get("References")),
		Subject:    decodeHeaderText(msg.Header.Get("Subject")),
		Date:       strings.TrimSpace(msg.Header.Get("Date")),
		From:       headerAddresses(msg.Header, "From"),
		ReplyTo:    headerAddresses(msg.Header, "Reply-To"),
		To:         headerAddresses(msg.Header, "To"),
		Cc:         headerAddresses(msg.Header, "Cc"),
		Raw:        raw,
	}, nil
}

// DeriveReply builds the message a reply consists of, bar its body.
//
// The rules it applies, all of them about who the message reaches:
//
//   - To is the original's Reply-To when it set one, else its From. A
//     sender who named a Reply-To asked for answers to go somewhere
//     other than where the mail came from, and ignoring that is how a
//     reply lands in an unattended mailbox.
//   - all additionally copies the original's To and Cc into Cc: everyone
//     who could already see each other, and nobody who could not.
//   - self never appears anywhere. Comparison is on the addr-spec,
//     lowercased, so a display name cannot smuggle our own address back
//     in, and an address in both To and Cc appears exactly once.
//   - every derived address is validated before it is a recipient. It
//     came out of a message body's headers, which makes it attacker-
//     supplied data whatever it looks like.
func DeriveReply(o Original, self string, all bool) (OutboundMessage, error) {
	targets := o.ReplyTo
	if len(targets) == 0 {
		targets = o.From
	}
	if err := ValidateAddresses(targets); err != nil {
		return OutboundMessage{}, err
	}
	set := newAddrSet(self)
	to := set.take(targets)
	if len(to) == 0 {
		return OutboundMessage{}, fmt.Errorf(
			"%w: the original names no sender other than %q — replying would only send the message back to this outbox",
			ErrNoReplyTarget, self)
	}

	msg := OutboundMessage{To: to, Subject: ReplySubject(o.Subject)}
	if all {
		if err := ValidateAddresses(o.To); err != nil {
			return OutboundMessage{}, err
		}
		if err := ValidateAddresses(o.Cc); err != nil {
			return OutboundMessage{}, err
		}
		// One set, taken in order, so an address already in To is not
		// repeated in Cc — and so self is excluded from both by the same
		// membership test rather than two that could disagree.
		msg.Cc = set.take(o.To, o.Cc)
	}
	applyThreading(&msg, o)
	return msg, nil
}

// DeriveForward builds the message a forward consists of, bar its body.
//
// The original travels as a message/rfc822 attachment rather than being
// re-rendered inline. That is what keeps its own attachments intact:
// re-encoding a message means decoding every part and encoding it again,
// and anything the renderer does not model — an inline image, a
// signature, a part whose transfer encoding matters — is lost in the
// round trip. Attached whole, the bytes the recipient receives are the
// bytes that arrived.
//
// The recipients are the caller's, not the message's: a forward goes
// where a human said, so there is no derived set here to exclude self
// from, and forwarding something to yourself is a normal thing to want.
// Bcc is dropped with the rest of the original's audience — see
// Original. That is about recipients: nobody on the original receives
// the forward. The attached bytes are a separate matter and are not
// edited, because "byte for byte" and "except for the headers we
// dislike" cannot both be true. A stored message carrying a Bcc header
// is one from a Sent folder — a copy of something this mailbox sent —
// and forwarding it passes on what is in it, exactly as attaching any
// other file does.
func DeriveForward(o Original, to []string) (OutboundMessage, error) {
	if err := ValidateAddresses(to); err != nil {
		return OutboundMessage{}, err
	}
	dest := newAddrSet().take(to)
	if len(dest) == 0 {
		return OutboundMessage{}, fmt.Errorf("%w: a forward needs at least one recipient", ErrNoReplyTarget)
	}
	if err := CheckForwardBudget(len(o.Raw)); err != nil {
		return OutboundMessage{}, err
	}
	return OutboundMessage{
		To:      dest,
		Subject: ForwardSubject(o.Subject),
		Attachments: []Attachment{{
			// A fixed filename, not one built from the subject: the value
			// is written into a Content-Disposition parameter, and a
			// subject is untrusted text.
			Filename:    "forwarded-message.eml",
			ContentType: "message/rfc822",
			Content:     o.Raw,
		}},
		// Deliberately no In-Reply-To or References. A forward is not an
		// answer to the original, and threading it as one files it into a
		// conversation the new recipient was never part of and cannot see.
	}, nil
}

// CheckForwardBudget refuses a forward whose original is over
// MaxAttachmentBytes.
//
// The original is carried whole, so the message's size is the original's
// size, and the ceiling that applies is the one every other attachment
// obeys. The refusal states the measurement and the budget — the two
// numbers a caller needs to understand that no retry will help — in the
// shape CheckFetchBudget uses for the same reason.
func CheckForwardBudget(size int) error {
	if size > MaxAttachmentBytes {
		return fmt.Errorf(
			"%w: the original measures %d bytes, over the %d-byte (%d MiB) ceiling for one attachment"+
				" — a forward carries the original whole, so it cannot be split; send a link or a summary instead",
			ErrForwardTooLarge, size, MaxAttachmentBytes, MaxAttachmentBytes>>20)
	}
	return nil
}

// replyPrefixes and forwardPrefixes are the spellings that already mark
// a subject as an answer or a pass-along.
var (
	replyPrefixes   = []string{"re:"}
	forwardPrefixes = []string{"fwd:", "fw:"}
)

// ReplySubject prefixes a subject with "Re:" unless it carries one
// already.
func ReplySubject(subject string) string { return prefixSubject(subject, "Re:", replyPrefixes) }

// ForwardSubject prefixes a subject with "Fwd:" unless it carries one
// already, in any of the spellings mail clients use.
func ForwardSubject(subject string) string { return prefixSubject(subject, "Fwd:", forwardPrefixes) }

// prefixSubject adds the prefix only when none of the equivalents is
// already there.
//
// The match is case-insensitive and the existing prefix is left exactly
// as written. Both halves matter: without the first, a thread
// accumulates "Re: RE: Re: Re:" one round trip at a time; without the
// second, briefkasten would rewrite a correspondent's "AW:" or "SV:"
// into its own house style, changing the subject line of a thread it
// merely joined.
func prefixSubject(subject, prefix string, existing []string) string {
	trimmed := strings.TrimSpace(subject)
	lower := strings.ToLower(trimmed)
	for _, p := range existing {
		if strings.HasPrefix(lower, p) {
			return trimmed
		}
	}
	return strings.TrimSpace(prefix + " " + trimmed)
}

// applyThreading chains a reply onto the original's thread.
//
// A message with no Message-Id gets no threading headers at all, and
// that is deliberate. Inventing an id would be worse than omitting them:
// In-Reply-To pointing at an id no message ever carried threads the
// reply onto nothing, and every client that walks References would file
// the conversation under a parent that does not exist. An unthreaded
// reply is merely a reply that shows up on its own; a fabricated parent
// is a lie the whole thread then inherits.
func applyThreading(msg *OutboundMessage, o Original) {
	id := strings.TrimSpace(o.MessageID)
	if id == "" {
		return
	}
	if !strings.HasPrefix(id, "<") {
		id = "<" + id + ">"
	}
	if err := ValidateMessageID(id); err != nil {
		// An id that cannot be rendered safely is one we do not have.
		return
	}
	msg.InReplyTo = id
	msg.References = make([]string, 0, len(o.References)+1)
	for _, ref := range o.References {
		if ValidateMessageID(ref) == nil {
			msg.References = append(msg.References, ref)
		}
	}
	msg.References = append(msg.References, id)
}

// Quote renders the original as a quoted block for a reply body: an
// attribution line, then the plain-text body prefixed with "> ".
func Quote(o Original) string {
	var b strings.Builder
	b.WriteString(attribution(o))
	body := PlainText(o.Raw)
	if body == "" {
		return b.String()
	}
	b.WriteString("\n")
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		b.WriteString("> " + line + "\n")
	}
	return b.String()
}

// ForwardIntro renders the header block a forwarded message is
// traditionally introduced by, followed by the original's text. The
// bytes themselves travel as the attachment; this is what a human reads
// without opening it.
func ForwardIntro(o Original) string {
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	for _, field := range []struct{ name, value string }{
		{"From", strings.Join(o.From, ", ")},
		{"Date", o.Date},
		{"Subject", o.Subject},
		{"To", strings.Join(o.To, ", ")},
		{"Cc", strings.Join(o.Cc, ", ")},
	} {
		if field.value != "" {
			fmt.Fprintf(&b, "%s: %s\n", field.name, field.value)
		}
	}
	if body := PlainText(o.Raw); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	return b.String()
}

// attribution is the "On <date>, <sender> wrote:" line a quote opens
// with, degrading gracefully when the original named neither.
func attribution(o Original) string {
	who := strings.Join(o.From, ", ")
	switch {
	case who != "" && o.Date != "":
		return fmt.Sprintf("On %s, %s wrote:\n", o.Date, who)
	case who != "":
		return fmt.Sprintf("%s wrote:\n", who)
	default:
		return "The original message read:\n"
	}
}

// PlainText extracts a message's text/plain body, walking one level of
// multipart when it has to and decoding the part's transfer encoding.
//
// It is best-effort by design: it answers "what would a human read?" for
// quoting, and quoting nothing is a cosmetic loss. Nothing downstream
// depends on it — a forward carries the original's exact bytes as an
// attachment whatever this returns — so it never fails, it just returns
// what it could find. The result is capped at MaxQuotedBytes.
func PlainText(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return strings.TrimRight(readTextBody(msg.Header.Get("Content-Type"),
		msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0), "\r\n")
}

// headerAddresses reads one address header as a list of addresses.
//
// The parsed form is preferred and re-rendered canonically. When the
// whole header will not parse, the raw comma-separated tokens are kept
// instead of the header being dropped: a recipient silently missing from
// a reply-all is a worse failure than one that trips validation, and
// every token is validated before it can become a recipient.
func headerAddresses(h mail.Header, key string) []string {
	raw := h.Get(key)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if list, err := h.AddressList(key); err == nil {
		out := make([]string, 0, len(list))
		for _, a := range list {
			// A bare address stays bare. Address.String() would wrap it in
			// angle brackets, which is legal but turns every recipient in
			// a confirmation prompt into something the human has to read
			// past to find the address.
			if a.Name == "" {
				out = append(out, a.Address)
				continue
			}
			out = append(out, a.String())
		}
		return out
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// decodeHeaderText decodes RFC 2047 encoded words, falling back to the
// raw value when the encoding is not one Go knows.
func decodeHeaderText(v string) string {
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(v)
	if err != nil {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(decoded)
}

// addrSet collects recipients while enforcing the two rules every
// derived set obeys: an excluded address never appears, and no address
// appears twice.
//
// Both comparisons are on the addr-spec, lowercased. "Alice
// <a@b.c>" and "a@b.c" are one recipient, and "Not Alice <a@b.c>" is
// still that same recipient — a display name is chosen by whoever wrote
// the header, so letting it distinguish two addresses would let it
// defeat both rules at once.
type addrSet struct{ seen map[string]struct{} }

// newAddrSet starts a set with the given addresses already claimed, so
// they can never be taken into it.
func newAddrSet(exclude ...string) *addrSet {
	s := &addrSet{seen: make(map[string]struct{}, len(exclude))}
	for _, addr := range exclude {
		if key := addrSpec(addr); key != "" {
			s.seen[key] = struct{}{}
		}
	}
	return s
}

// take appends, in order, the candidates the set has not seen yet.
// Groups are taken as one sequence so an address in two of them lands in
// the first only.
func (s *addrSet) take(groups ...[]string) []string {
	var out []string
	for _, group := range groups {
		for _, addr := range group {
			addr = strings.TrimSpace(addr)
			key := addrSpec(addr)
			if key == "" {
				continue
			}
			if _, dup := s.seen[key]; dup {
				continue
			}
			s.seen[key] = struct{}{}
			out = append(out, addr)
		}
	}
	return out
}

// addrSpec reduces an address to its comparable form: the addr-spec,
// lowercased, with the display name discarded.
func addrSpec(addr string) string {
	addr = strings.TrimSpace(addr)
	if a, err := mail.ParseAddress(addr); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(addr)
}
