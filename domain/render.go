package domain

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"
)

// RenderRFC5322 renders an outbound message as an RFC 5322 email. The structure
// adapts to the message content:
//
//   - body only                     → text/plain
//   - body + HTMLBody               → multipart/alternative (text, html)
//   - body + attachments            → multipart/mixed (text, attachment…)
//   - body + HTMLBody + attachments → multipart/mixed (alternative, attachment…)
//
// Bcc is never written. A blind copy that appears in the message is not
// blind: the header travels with the message to every recipient, so
// rendering it would show the whole hidden list to exactly the people it
// was hidden from — and would do so silently, since the sender sees
// their own copy looking correct. The addresses reach the transport
// through OutboundMessage.Recipients instead, which is the envelope and
// only the envelope.
func RenderRFC5322(from string, msg OutboundMessage, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(msg.To, ", "))
	if len(msg.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(msg.Cc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: <%s@briefkasten>\r\n", msg.ID)
	if msg.InReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", msg.InReplyTo)
	}
	writeReferences(&b, msg.References)
	b.WriteString("MIME-Version: 1.0\r\n")

	var err error
	switch {
	case msg.HTMLBody == "" && len(msg.Attachments) == 0:
		err = renderPlain(&b, msg)
	case len(msg.Attachments) == 0: // HTML, no attachments → top-level alternative
		err = renderAlternative(&b, msg)
	default: // attachments present → top-level mixed
		err = renderMixed(&b, msg)
	}
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeReferences writes the References header, folded at whitespace.
//
// A thread's id chain grows by one msg-id per round trip, so on a long
// conversation it runs past the 998-character line RFC 5322 permits —
// and an over-long header is not a cosmetic problem: it is a message
// some servers refuse and some clients truncate. Folding is legal
// anywhere between ids, since the header body is a whitespace-separated
// list.
func writeReferences(w io.Writer, refs []string) {
	if len(refs) == 0 {
		return
	}
	const limit = 76
	line := "References:"
	for _, ref := range refs {
		if line != "" && len(line)+1+len(ref) > limit {
			fmt.Fprintf(w, "%s\r\n", line)
			line = "" // the continuation's leading space comes from the append below
		}
		line += " " + ref
	}
	fmt.Fprintf(w, "%s\r\n", line)
}

// renderPlain writes a single text/plain body.
func renderPlain(w io.Writer, msg OutboundMessage) error {
	fmt.Fprint(w, "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	return writeQuotedPrintable(w, msg.Body)
}

// renderAlternative writes a top-level multipart/alternative (text + html).
func renderAlternative(w io.Writer, msg OutboundMessage) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary("alt", msg.ID)); err != nil {
		return err
	}
	fmt.Fprintf(w, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", mw.Boundary())
	if err := writeAlternativeParts(mw, msg.Body, msg.HTMLBody); err != nil {
		return err
	}
	return mw.Close()
}

// renderMixed writes a top-level multipart/mixed: a body part (alternative when
// HTML is present, else plain text) followed by one part per attachment.
func renderMixed(w io.Writer, msg OutboundMessage) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary("mixed", msg.ID)); err != nil {
		return err
	}
	fmt.Fprintf(w, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mw.Boundary())
	if msg.HTMLBody != "" {
		if err := writeAlternativePart(mw, msg); err != nil {
			return err
		}
	} else if err := writeTextPart(mw, "text/plain; charset=utf-8", msg.Body); err != nil {
		return err
	}
	for _, a := range msg.Attachments {
		if err := writeAttachmentPart(mw, a); err != nil {
			return err
		}
	}
	return mw.Close()
}

// writeAlternativePart writes a nested multipart/alternative part (text + html)
// into a parent multipart writer.
func writeAlternativePart(parent *multipart.Writer, msg OutboundMessage) error {
	altBoundary := boundary("alt", msg.ID)
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", altBoundary))
	pw, err := parent.CreatePart(h)
	if err != nil {
		return err
	}
	nested := multipart.NewWriter(pw)
	if err := nested.SetBoundary(altBoundary); err != nil {
		return err
	}
	if err := writeAlternativeParts(nested, msg.Body, msg.HTMLBody); err != nil {
		return err
	}
	return nested.Close()
}

// writeAlternativeParts writes the text and html leaf parts into an
// alternative multipart writer.
func writeAlternativeParts(mw *multipart.Writer, text, html string) error {
	if err := writeTextPart(mw, "text/plain; charset=utf-8", text); err != nil {
		return err
	}
	return writeTextPart(mw, "text/html; charset=utf-8", html)
}

// writeTextPart writes a quoted-printable text part with the given content type.
func writeTextPart(mw *multipart.Writer, contentType, content string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	return writeQuotedPrintable(pw, content)
}

// writeAttachmentPart writes a base64-encoded attachment part.
func writeAttachmentPart(mw *multipart.Writer, a Attachment) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", a.ContentType)
	h.Set("Content-Transfer-Encoding", "base64")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}))
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	return writeBase64Wrapped(pw, a.Content)
}

func writeQuotedPrintable(w io.Writer, s string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := io.WriteString(qp, s); err != nil {
		return err
	}
	return qp.Close()
}

// writeBase64Wrapped writes content as base64 with CRLF every 76 characters,
// per RFC 2045.
func writeBase64Wrapped(w io.Writer, content []byte) error {
	const lineLen = 76
	enc := base64.StdEncoding.EncodeToString(content)
	for len(enc) > lineLen {
		if _, err := io.WriteString(w, enc[:lineLen]+"\r\n"); err != nil {
			return err
		}
		enc = enc[lineLen:]
	}
	_, err := io.WriteString(w, enc+"\r\n")
	return err
}

// boundary derives a deterministic, RFC-valid MIME boundary from a tag and the
// message id, so a message renders identically across calls.
func boundary(tag, id string) string {
	var safe strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			safe.WriteRune(r)
		}
	}
	s := safe.String()
	if s == "" {
		s = "0"
	}
	b := fmt.Sprintf("bk-%s-%s", tag, s)
	if len(b) > 70 {
		b = b[:70]
	}
	return b
}
