package domain

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// original builds a raw RFC 5322 message from header lines and a body.
func original(headers map[string]string, body string) []byte {
	var b strings.Builder
	for _, key := range []string{"From", "Reply-To", "To", "Cc", "Bcc", "Subject", "Date", "Message-Id", "References"} {
		if v, ok := headers[key]; ok {
			fmt.Fprintf(&b, "%s: %s\r\n", key, v)
		}
	}
	b.WriteString("\r\n" + body)
	return []byte(b.String())
}

// mustParse parses a raw message, failing the test if it will not.
func mustParse(t *testing.T, raw []byte) Original {
	t.Helper()
	o, err := ParseOriginal(raw)
	if err != nil {
		t.Fatalf("ParseOriginal: %v", err)
	}
	return o
}

// thread is the message the reply tests answer: a small conversation
// with a sender, two other recipients and one cc.
func thread(t *testing.T) Original {
	t.Helper()
	return mustParse(t, original(map[string]string{
		"From":       "Alice <alice@example.com>",
		"To":         "me@ours.example, Bob <bob@example.com>",
		"Cc":         "carol@example.com",
		"Subject":    "Q3 planning",
		"Date":       "Mon, 8 Jun 2026 12:00:00 +0000",
		"Message-Id": "<orig-1@example.com>",
	}, "Shall we meet?\r\n"))
}

// A plain reply goes to the sender and nobody else.
func TestDeriveReplyTargetsTheSender(t *testing.T) {
	msg, err := DeriveReply(thread(t), "me@ours.example", false)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if len(msg.To) != 1 || !strings.Contains(msg.To[0], "alice@example.com") {
		t.Errorf("To = %v, want only alice", msg.To)
	}
	if len(msg.Cc) != 0 {
		t.Errorf("Cc = %v, want none on a plain reply", msg.Cc)
	}
	if msg.Subject != "Re: Q3 planning" {
		t.Errorf("Subject = %q", msg.Subject)
	}
}

// A Reply-To beats From: the sender asked for answers elsewhere, and
// ignoring that lands the reply in a mailbox nobody reads.
func TestDeriveReplyPrefersReplyTo(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":     "noreply@example.com",
		"Reply-To": "support@example.com",
		"To":       "me@ours.example",
		"Subject":  "ticket",
	}, "body"))
	msg, err := DeriveReply(o, "me@ours.example", false)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if len(msg.To) != 1 || msg.To[0] != "support@example.com" {
		t.Errorf("To = %v, want the Reply-To address", msg.To)
	}
}

// Reply-all widens to everyone who could already see each other: the
// original's To and Cc, with our own address gone and no duplicates.
func TestDeriveReplyAllDerivesFromToAndCc(t *testing.T) {
	msg, err := DeriveReply(thread(t), "ME@Ours.Example", true)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if len(msg.To) != 1 || !strings.Contains(msg.To[0], "alice@example.com") {
		t.Fatalf("To = %v, want only alice", msg.To)
	}
	joined := strings.ToLower(strings.Join(msg.Cc, " "))
	for _, want := range []string{"bob@example.com", "carol@example.com"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Cc = %v, want it to include %s", msg.Cc, want)
		}
	}
	// Self is excluded case-insensitively — the exclusion was spelled
	// with different capitalisation than the header.
	if strings.Contains(joined, "me@ours.example") {
		t.Errorf("Cc = %v, want our own address excluded", msg.Cc)
	}
	if len(msg.Cc) != 2 {
		t.Errorf("Cc = %v, want exactly bob and carol", msg.Cc)
	}
}

// An address in both To and Cc is one recipient, and the display name
// attached to it is not what makes it two.
func TestDeriveReplyAllCollapsesDuplicates(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":    "Alice <alice@example.com>",
		"To":      "Bob <bob@example.com>, me@ours.example",
		"Cc":      "\"Bob (mobile)\" <BOB@example.com>, Alice <alice@example.com>",
		"Subject": "dup",
	}, "body"))
	msg, err := DeriveReply(o, "me@ours.example", true)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	all := append(append([]string{}, msg.To...), msg.Cc...)
	if len(all) != 2 {
		t.Fatalf("recipients = %v, want alice (To) and bob (Cc) once each", all)
	}
	// Alice is already in To, so the Cc copy of her must not reappear.
	if len(msg.Cc) != 1 || !strings.Contains(strings.ToLower(msg.Cc[0]), "bob@example.com") {
		t.Errorf("Cc = %v, want only bob", msg.Cc)
	}
}

// The Bcc of an original is never a source of recipients, even when the
// header is sitting right there in the message — which happens on mail
// read out of the sender's own Sent folder. Replying to it would show
// everyone a list its sender deliberately hid.
func TestDeriveReplyNeverDerivesFromBcc(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":    "Alice <alice@example.com>",
		"To":      "me@ours.example",
		"Bcc":     "hidden@example.com, secret@example.com",
		"Subject": "quiet",
	}, "body"))
	msg, err := DeriveReply(o, "me@ours.example", true)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	everyone := strings.ToLower(strings.Join(msg.Recipients(), " "))
	for _, hidden := range []string{"hidden@example.com", "secret@example.com"} {
		if strings.Contains(everyone, hidden) {
			t.Errorf("recipients %v leak the original's Bcc %s", msg.Recipients(), hidden)
		}
	}
	if len(msg.Bcc) != 0 {
		t.Errorf("Bcc = %v, want a derived reply to carry none", msg.Bcc)
	}
}

// A forward drops the original's audience entirely — Cc as well as Bcc.
// The new recipients are the caller's alone.
func TestDeriveForwardDropsOriginalAudience(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":    "Alice <alice@example.com>",
		"To":      "me@ours.example",
		"Cc":      "carol@example.com",
		"Bcc":     "hidden@example.com",
		"Subject": "notes",
	}, "body"))
	msg, err := DeriveForward(o, []string{"dave@example.com"})
	if err != nil {
		t.Fatalf("DeriveForward: %v", err)
	}
	if len(msg.Recipients()) != 1 || msg.Recipients()[0] != "dave@example.com" {
		t.Errorf("recipients = %v, want only dave", msg.Recipients())
	}
	if len(msg.Cc)+len(msg.Bcc) != 0 {
		t.Errorf("forward carried Cc %v / Bcc %v from the original", msg.Cc, msg.Bcc)
	}
}

// Replying to mail this outbox sent itself has nowhere to go, and is
// refused rather than answered back to ourselves.
func TestDeriveReplyRefusesWhenOnlySelfRemains(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":    "me@ours.example",
		"To":      "me@ours.example",
		"Subject": "note to self",
	}, "body"))
	_, err := DeriveReply(o, "me@ours.example", false)
	if !errors.Is(err, ErrNoReplyTarget) {
		t.Errorf("err = %v, want ErrNoReplyTarget", err)
	}
}

// Threading chains: the reply points at the original, and a reply to
// that reply carries both ids in order.
func TestThreadingChainsTwoDeep(t *testing.T) {
	first, err := DeriveReply(thread(t), "me@ours.example", false)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if first.InReplyTo != "<orig-1@example.com>" {
		t.Errorf("In-Reply-To = %q", first.InReplyTo)
	}
	if len(first.References) != 1 || first.References[0] != "<orig-1@example.com>" {
		t.Errorf("References = %v, want just the original", first.References)
	}

	// Alice answers our reply; we answer hers.
	second := mustParse(t, original(map[string]string{
		"From":       "Alice <alice@example.com>",
		"To":         "me@ours.example",
		"Subject":    "Re: Q3 planning",
		"Message-Id": "<reply-2@example.com>",
		"References": "<orig-1@example.com>",
	}, "sure"))
	third, err := DeriveReply(second, "me@ours.example", false)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if third.InReplyTo != "<reply-2@example.com>" {
		t.Errorf("In-Reply-To = %q, want the message being answered", third.InReplyTo)
	}
	want := []string{"<orig-1@example.com>", "<reply-2@example.com>"}
	if len(third.References) != len(want) {
		t.Fatalf("References = %v, want %v", third.References, want)
	}
	for i, ref := range want {
		if third.References[i] != ref {
			t.Errorf("References[%d] = %q, want %q", i, third.References[i], ref)
		}
	}
}

// An original with no Message-Id gets no threading headers at all. A
// fabricated parent would thread the whole conversation onto a message
// that never existed.
func TestNoMessageIDMeansNoThreadingHeaders(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":    "alice@example.com",
		"To":      "me@ours.example",
		"Subject": "no id",
	}, "body"))
	msg, err := DeriveReply(o, "me@ours.example", false)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if msg.InReplyTo != "" || len(msg.References) != 0 {
		t.Errorf("In-Reply-To = %q, References = %v; want neither invented",
			msg.InReplyTo, msg.References)
	}
	raw := mustRender(t, msg)
	for _, header := range []string{"In-Reply-To:", "References:"} {
		if strings.Contains(string(raw), header) {
			t.Errorf("rendered message carries %s despite the original having no Message-Id:\n%s", header, raw)
		}
	}
}

// Prefixes do not stack, whatever case the existing one is written in,
// and the existing spelling is preserved rather than normalised.
func TestSubjectPrefixesDoNotStack(t *testing.T) {
	cases := []struct{ in, reply, forward string }{
		{"Q3 planning", "Re: Q3 planning", "Fwd: Q3 planning"},
		{"Re: Q3 planning", "Re: Q3 planning", "Fwd: Re: Q3 planning"},
		{"RE: Q3 planning", "RE: Q3 planning", "Fwd: RE: Q3 planning"},
		{"re: Q3 planning", "re: Q3 planning", "Fwd: re: Q3 planning"},
		{"Fwd: Q3 planning", "Re: Fwd: Q3 planning", "Fwd: Q3 planning"},
		{"FW: Q3 planning", "Re: FW: Q3 planning", "FW: Q3 planning"},
		{"", "Re:", "Fwd:"},
	}
	for _, tc := range cases {
		if got := ReplySubject(tc.in); got != tc.reply {
			t.Errorf("ReplySubject(%q) = %q, want %q", tc.in, got, tc.reply)
		}
		if got := ForwardSubject(tc.in); got != tc.forward {
			t.Errorf("ForwardSubject(%q) = %q, want %q", tc.in, got, tc.forward)
		}
	}
	// Three round trips must not grow three prefixes.
	s := "Q3 planning"
	for range 3 {
		s = ReplySubject(s)
	}
	if s != "Re: Q3 planning" {
		t.Errorf("three replies produced %q", s)
	}
}

// A derived recipient is attacker-supplied data: it comes out of the
// headers of a message anyone could have sent. One carrying CR/LF must
// be refused, not written into a To header where it would forge the rest
// of the message.
func TestDerivedRecipientRejectsHeaderInjection(t *testing.T) {
	injected := "evil@example.com>\r\nBcc: everyone@example.com"

	// In the From, so a plain reply would carry it.
	_, err := DeriveReply(Original{
		From:    []string{injected},
		Subject: "x",
	}, "me@ours.example", false)
	if err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Errorf("injected From = %v, want a line-break refusal", err)
	}

	// In the Cc, which only reply-all reads.
	_, err = DeriveReply(Original{
		From:    []string{"alice@example.com"},
		To:      []string{"me@ours.example"},
		Cc:      []string{injected},
		Subject: "x",
	}, "me@ours.example", true)
	if err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Errorf("injected Cc = %v, want a line-break refusal", err)
	}

	// And in a forward's caller-supplied recipients.
	_, err = DeriveForward(Original{Subject: "x", Raw: []byte("From: a@b.c\r\n\r\nx")}, []string{injected})
	if err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Errorf("injected forward recipient = %v, want a line-break refusal", err)
	}
}

// A Message-Id that would forge headers is dropped rather than rendered:
// the reply loses its threading, which is a cosmetic loss, instead of
// gaining an attacker's headers, which is not.
func TestInjectedMessageIDIsNotThreaded(t *testing.T) {
	msg, err := DeriveReply(Original{
		From:      []string{"alice@example.com"},
		Subject:   "x",
		MessageID: "<a@b.c>\r\nBcc: everyone@example.com",
	}, "me@ours.example", false)
	if err != nil {
		t.Fatalf("DeriveReply: %v", err)
	}
	if msg.InReplyTo != "" || len(msg.References) != 0 {
		t.Errorf("In-Reply-To = %q, References = %v; want the unsafe id dropped",
			msg.InReplyTo, msg.References)
	}
	if err := msg.Validate(); err != nil {
		t.Errorf("derived message does not validate: %v", err)
	}
}

// A forward carries the original whole, so it cannot be split; one over
// the attachment ceiling is refused with what it measured.
func TestOversizedForwardIsRefusedWithTheSize(t *testing.T) {
	big := make([]byte, MaxAttachmentBytes+17)
	copy(big, "From: a@b.c\r\n\r\n")
	_, err := DeriveForward(Original{Subject: "huge", Raw: big}, []string{"dave@example.com"})
	if !errors.Is(err, ErrForwardTooLarge) {
		t.Fatalf("err = %v, want ErrForwardTooLarge", err)
	}
	for _, want := range []string{
		fmt.Sprint(len(big)),           // the measured size
		fmt.Sprint(MaxAttachmentBytes), // the budget
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}

// The original rides along as message/rfc822, byte for byte, so its own
// attachments arrive as they were sent rather than re-encoded.
func TestForwardPreservesTheOriginalBytes(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"To: me@ours.example\r\n" +
		"Subject: with attachment\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"sep\"\r\n\r\n" +
		"--sep\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--sep\r\nContent-Type: application/pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"filing.pdf\"\r\n\r\n" +
		"JVBERi0xLjQK\r\n--sep--\r\n")

	msg, err := DeriveForward(mustParse(t, raw), []string{"dave@example.com"})
	if err != nil {
		t.Fatalf("DeriveForward: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want the original attached once", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.ContentType != "message/rfc822" {
		t.Errorf("content type = %q, want message/rfc822", att.ContentType)
	}
	if !bytes.Equal(att.Content, raw) {
		t.Errorf("forwarded original was not preserved byte for byte:\ngot  %q\nwant %q", att.Content, raw)
	}

	// And it survives the render: base64 round-trips, so the recipient's
	// client sees the same nested attachment.
	msg.Body = "fyi"
	rendered := mustRender(t, msg)
	if !strings.Contains(string(rendered), "message/rfc822") {
		t.Errorf("rendered forward does not carry the message/rfc822 part:\n%s", rendered)
	}
	m := parse(t, rendered)
	_, params := mediaType(t, m)
	parts := readParts(t, m.Body, params["boundary"])
	if len(parts) != 2 {
		t.Fatalf("rendered forward has %d parts, want body + original", len(parts))
	}
	if got := decodePart(t, parts[1]); got["body"] != strings.TrimRight(string(raw), "\r\n") {
		t.Errorf("attached original did not round-trip through the renderer")
	}
}

// A header the address parser chokes on is not silently dropped: the
// tokens are kept and fail validation loudly, because a recipient
// missing from a reply-all is a worse surprise than one that errors.
func TestUnparseableAddressHeaderIsNotSilentlyDropped(t *testing.T) {
	o := mustParse(t, original(map[string]string{
		"From":    "alice@example.com",
		"To":      "me@ours.example, not an address at all",
		"Subject": "odd",
	}, "body"))
	if len(o.To) == 0 {
		t.Fatal("the unparseable To header was dropped entirely")
	}
	if _, err := DeriveReply(o, "me@ours.example", true); err == nil {
		t.Error("reply-all over an unparseable recipient succeeded silently")
	}
}

// Quoting is a courtesy, but it must find the text of a real multipart
// message rather than quoting the MIME scaffolding.
func TestQuoteFindsThePlainTextPart(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"Date: Mon, 8 Jun 2026 12:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"sep\"\r\n\r\n" +
		"--sep\r\nContent-Type: text/plain\r\n\r\nShall we meet?\r\n" +
		"--sep\r\nContent-Type: text/html\r\n\r\n<p>Shall we meet?</p>\r\n--sep--\r\n")
	quoted := Quote(mustParse(t, raw))
	if !strings.Contains(quoted, "alice@example.com wrote:") {
		t.Errorf("quote has no attribution line:\n%s", quoted)
	}
	if !strings.Contains(quoted, "> Shall we meet?") {
		t.Errorf("quote does not carry the plain-text body:\n%s", quoted)
	}
	if strings.Contains(quoted, "<p>") {
		t.Errorf("quote picked up the HTML alternative:\n%s", quoted)
	}
}

// A message that will not parse at all is refused where it is read,
// rather than producing a reply to nobody.
func TestParseOriginalRejectsGarbage(t *testing.T) {
	if _, err := ParseOriginal([]byte("this is not a message")); err == nil {
		t.Error("unparseable bytes accepted as an original")
	}
}

// The forwarded header block names what a human needs to place the
// message without opening the attachment, and names nothing that was not
// in the original.
func TestForwardIntroNamesTheOriginalsHeaders(t *testing.T) {
	intro := ForwardIntro(mustParse(t, original(map[string]string{
		"From":    "Alice <alice@example.com>",
		"To":      "me@ours.example",
		"Cc":      "carol@example.com",
		"Subject": "Q3 planning",
		"Date":    "Mon, 8 Jun 2026 12:00:00 +0000",
	}, "Shall we meet?\r\n")))

	for _, want := range []string{
		"---------- Forwarded message ----------",
		"From: \"Alice\" <alice@example.com>",
		"Date: Mon, 8 Jun 2026 12:00:00 +0000",
		"Subject: Q3 planning",
		"To: me@ours.example",
		"Cc: carol@example.com",
		"Shall we meet?",
	} {
		if !strings.Contains(intro, want) {
			t.Errorf("forward intro is missing %q:\n%s", want, intro)
		}
	}

	// Fields the original did not carry are omitted rather than rendered
	// empty.
	bare := ForwardIntro(mustParse(t, original(map[string]string{"Subject": "bare"}, "x")))
	for _, absent := range []string{"From:", "Date:", "Cc:", "To:"} {
		if strings.Contains(bare, absent) {
			t.Errorf("forward intro of a bare message renders %q:\n%s", absent, bare)
		}
	}
}

// A quote degrades rather than fails when the original names no sender
// or no date.
func TestQuoteAttributionDegrades(t *testing.T) {
	noDate := Quote(Original{From: []string{"alice@example.com"}, Raw: []byte("From: a@b.c\r\n\r\nhi\r\n")})
	if !strings.HasPrefix(noDate, "alice@example.com wrote:") {
		t.Errorf("attribution without a date = %q", noDate)
	}
	anonymous := Quote(Original{Raw: []byte("From: a@b.c\r\n\r\nhi\r\n")})
	if !strings.HasPrefix(anonymous, "The original message read:") {
		t.Errorf("attribution without a sender = %q", anonymous)
	}
	// Nothing quotable at all still yields just the attribution.
	empty := Quote(Original{From: []string{"alice@example.com"}})
	if strings.TrimSpace(empty) != "alice@example.com wrote:" {
		t.Errorf("quote of an unreadable original = %q", empty)
	}
}

// A body that dwarfs the message is trimmed rather than repeated whole
// into every reply in the thread.
func TestQuotedOriginalIsCapped(t *testing.T) {
	body := strings.Repeat("x", MaxQuotedBytes+4096)
	quoted := Quote(Original{
		From: []string{"alice@example.com"},
		Raw:  []byte("From: alice@example.com\r\n\r\n" + body),
	})
	if len(quoted) > MaxQuotedBytes+1024 {
		t.Errorf("quote is %d bytes, want it capped near %d", len(quoted), MaxQuotedBytes)
	}
	if !strings.Contains(quoted, "truncated") {
		t.Errorf("a trimmed quote does not say so")
	}
}

// Transfer encodings are undone before quoting, or the reply would quote
// base64 at the reader.
func TestPlainTextDecodesTransferEncodings(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"base64", "Content-Transfer-Encoding: base64\r\nContent-Type: text/plain\r\n\r\naGVsbG8gdGhlcmU=\r\n"},
		{"quoted-printable", "Content-Transfer-Encoding: quoted-printable\r\nContent-Type: text/plain\r\n\r\nhello=20there\r\n"},
		{"no content type", "\r\nhello there\r\n"},
	} {
		if got := PlainText([]byte("From: a@b.c\r\n" + tc.raw)); !strings.Contains(got, "hello there") {
			t.Errorf("%s: PlainText = %q", tc.name, got)
		}
	}
	// A message with no text part at all quotes nothing rather than
	// quoting the scaffolding.
	only := "From: a@b.c\r\nMIME-Version: 1.0\r\nContent-Type: image/png\r\n\r\nbinary\r\n"
	if got := PlainText([]byte(only)); got != "" {
		t.Errorf("PlainText of a bodiless message = %q, want empty", got)
	}
}
