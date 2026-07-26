package domain

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

var renderTime = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

// mustRender renders a message at the fixed test time, failing on error.
func mustRender(t *testing.T, msg OutboundMessage) []byte {
	t.Helper()
	raw, err := RenderRFC5322("me@x.y", msg, renderTime)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	return raw
}

// parse turns rendered bytes into a *mail.Message for header + body inspection.
func parse(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("rendered output is not a valid RFC 5322 message: %v\n%s", err, raw)
	}
	return m
}

// mediaType returns the Content-Type media type and params of a parsed message.
func mediaType(t *testing.T, m *mail.Message) (string, map[string]string) {
	t.Helper()
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	return mt, params
}

func TestRenderPlainTextWhenNoHTMLOrAttachments(t *testing.T) {
	msg := OutboundMessage{ID: "1", To: []string{"a@b.c"}, Subject: "hi", Body: "hello"}
	m := parse(t, mustRender(t, msg))

	mt, params := mediaType(t, m)
	if mt != "text/plain" {
		t.Errorf("media type = %q, want text/plain", mt)
	}
	if params["charset"] != "utf-8" {
		t.Errorf("charset = %q, want utf-8", params["charset"])
	}
	body, _ := io.ReadAll(m.Body)
	if !strings.Contains(string(body), "hello") {
		t.Errorf("body %q missing text", body)
	}
}

// Bcc must reach the envelope and nothing else. A Bcc header rendered
// into the message would show every hidden recipient to all the others —
// and to each other — which is the exact opposite of what the field
// means. The sender would not notice, because their own copy looks
// right.
func TestBccIsNeverRenderedButIsInTheEnvelope(t *testing.T) {
	msg := OutboundMessage{
		ID: "b1", To: []string{"a@b.c"}, Cc: []string{"c@d.e"},
		Bcc:     []string{"hidden@x.y", "quiet@x.y"},
		Subject: "quiet", Body: "hello",
	}
	raw := mustRender(t, msg)

	m := parse(t, raw)
	if got := m.Header.Get("Bcc"); got != "" {
		t.Errorf("rendered message carries a Bcc header %q — every recipient would see the hidden list", got)
	}
	// Not merely absent as a parsed header: the addresses must not appear
	// anywhere in the bytes that go on the wire.
	for _, hidden := range []string{"hidden@x.y", "quiet@x.y"} {
		if strings.Contains(string(raw), hidden) {
			t.Errorf("blind recipient %s appears in the rendered message:\n%s", hidden, raw)
		}
	}
	// Cc, by contrast, is meant to be seen.
	if got := m.Header.Get("Cc"); got != "c@d.e" {
		t.Errorf("Cc = %q, want it rendered", got)
	}

	// The envelope is where they travel — that is the only reason they
	// are delivered at all.
	got := msg.Recipients()
	want := []string{"a@b.c", "c@d.e", "hidden@x.y", "quiet@x.y"}
	if len(got) != len(want) {
		t.Fatalf("Recipients() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Recipients()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The envelope is a set: an address listed in two fields is one RCPT TO,
// and the comparison ignores the display name.
func TestRecipientsDeduplicateAcrossFields(t *testing.T) {
	msg := OutboundMessage{
		To:  []string{"Alice <a@b.c>"},
		Cc:  []string{"A@B.C", "c@d.e"},
		Bcc: []string{"c@d.e"},
	}
	if got := msg.Recipients(); len(got) != 2 {
		t.Errorf("Recipients() = %v, want alice and c@d.e once each", got)
	}
}

// Threading headers render when they exist, and References folds rather
// than running past the line length RFC 5322 permits.
func TestRenderThreadingHeaders(t *testing.T) {
	refs := make([]string, 12)
	for i := range refs {
		refs[i] = fmt.Sprintf("<message-%02d-of-a-long-thread@example.com>", i)
	}
	msg := OutboundMessage{
		ID: "t1", To: []string{"a@b.c"}, Subject: "Re: long", Body: "x",
		InReplyTo: refs[len(refs)-1], References: refs,
	}
	raw := mustRender(t, msg)

	m := parse(t, raw)
	if got := m.Header.Get("In-Reply-To"); got != refs[len(refs)-1] {
		t.Errorf("In-Reply-To = %q", got)
	}
	got := strings.Fields(m.Header.Get("References"))
	if len(got) != len(refs) {
		t.Fatalf("References has %d ids, want %d", len(got), len(refs))
	}
	for i := range refs {
		if got[i] != refs[i] {
			t.Errorf("References[%d] = %q, want %q", i, got[i], refs[i])
		}
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line exceeds the RFC 5322 limit: %d chars", len(line))
		}
	}
}

func TestRenderHTMLProducesMultipartAlternative(t *testing.T) {
	msg := OutboundMessage{
		ID: "2", To: []string{"a@b.c"}, Subject: "hi",
		Body:     "plain version",
		HTMLBody: "<p>html version</p>",
	}
	m := parse(t, mustRender(t, msg))

	mt, params := mediaType(t, m)
	if mt != "multipart/alternative" {
		t.Fatalf("media type = %q, want multipart/alternative", mt)
	}

	parts := readParts(t, m.Body, params["boundary"])
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (text + html)", len(parts))
	}
	if got := decodePart(t, parts[0]); got["ct"] != "text/plain" || !strings.Contains(got["body"], "plain version") {
		t.Errorf("part 0 = %+v, want text/plain with plain version", got)
	}
	if got := decodePart(t, parts[1]); got["ct"] != "text/html" || !strings.Contains(got["body"], "html version") {
		t.Errorf("part 1 = %+v, want text/html with html version", got)
	}
}

func TestRenderAttachmentProducesMultipartMixed(t *testing.T) {
	msg := OutboundMessage{
		ID: "3", To: []string{"a@b.c"}, Subject: "filing", Body: "see attached",
		Attachments: []Attachment{{
			Filename:    "filing.pdf",
			ContentType: "application/pdf",
			Content:     []byte("%PDF-1.4 fake pdf bytes"),
		}},
	}
	m := parse(t, mustRender(t, msg))

	mt, params := mediaType(t, m)
	if mt != "multipart/mixed" {
		t.Fatalf("media type = %q, want multipart/mixed", mt)
	}
	parts := readParts(t, m.Body, params["boundary"])
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (body + attachment)", len(parts))
	}
	// first part: the text body
	if got := decodePart(t, parts[0]); got["ct"] != "text/plain" || !strings.Contains(got["body"], "see attached") {
		t.Errorf("body part = %+v", got)
	}
	// second part: the attachment, base64, with disposition + filename
	att := parts[1]
	disp, dparams, err := mime.ParseMediaType(att.Header.Get("Content-Disposition"))
	if err != nil || disp != "attachment" || dparams["filename"] != "filing.pdf" {
		t.Errorf("disposition = %q %v err=%v, want attachment filing.pdf", disp, dparams, err)
	}
	got := decodePart(t, att)
	if got["ct"] != "application/pdf" {
		t.Errorf("attachment content-type = %q", got["ct"])
	}
	if got["body"] != "%PDF-1.4 fake pdf bytes" {
		t.Errorf("attachment bytes round-trip = %q", got["body"])
	}
}

func TestRenderHTMLAndAttachmentNestsAlternativeInMixed(t *testing.T) {
	msg := OutboundMessage{
		ID: "4", To: []string{"a@b.c"}, Subject: "both", Body: "plain", HTMLBody: "<b>rich</b>",
		Attachments: []Attachment{{Filename: "x.txt", ContentType: "text/plain", Content: []byte("data")}},
	}
	m := parse(t, mustRender(t, msg))
	mt, params := mediaType(t, m)
	if mt != "multipart/mixed" {
		t.Fatalf("top media type = %q, want multipart/mixed", mt)
	}
	parts := readParts(t, m.Body, params["boundary"])
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (alternative + attachment)", len(parts))
	}
	innerMT, innerParams, err := mime.ParseMediaType(parts[0].Header.Get("Content-Type"))
	if err != nil || innerMT != "multipart/alternative" {
		t.Fatalf("first part = %q err=%v, want multipart/alternative", innerMT, err)
	}
	inner := readParts(t, bytes.NewReader(parts[0].Body), innerParams["boundary"])
	if len(inner) != 2 {
		t.Fatalf("alternative has %d parts, want 2", len(inner))
	}
}

// --- helpers ---

// capturedPart holds a part's headers and its already-read body. Bodies must be
// read during iteration: multipart.Part is a streaming reader that advancing to
// the next part invalidates.
type capturedPart struct {
	Header textproto.MIMEHeader
	Body   []byte
}

func readParts(t *testing.T, r io.Reader, boundary string) []capturedPart {
	t.Helper()
	if boundary == "" {
		t.Fatal("no boundary param")
	}
	mr := multipart.NewReader(r, boundary)
	var parts []capturedPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		// multipart.Part.Read transparently decodes quoted-printable and
		// base64 Content-Transfer-Encoding.
		body, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part body: %v", err)
		}
		parts = append(parts, capturedPart{Header: p.Header, Body: body})
	}
	return parts
}

// decodePart returns the part's base media type and its decoded body.
// multipart.Part auto-decodes quoted-printable but not base64, so base64 is
// decoded here.
func decodePart(t *testing.T, p capturedPart) map[string]string {
	t.Helper()
	ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
	body := p.Body
	if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "base64") {
		clean := strings.NewReplacer("\r", "", "\n", "").Replace(string(body))
		dec, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			t.Fatalf("base64 decode part: %v", err)
		}
		body = dec
	}
	return map[string]string{"ct": ct, "body": strings.TrimRight(string(body), "\r\n")}
}

func TestAttachmentValidation(t *testing.T) {
	base := OutboundMessage{To: []string{"a@b.c"}, Body: "x"}

	missingName := base
	missingName.Attachments = []Attachment{{ContentType: "text/plain", Content: []byte("x")}}
	if err := missingName.Validate(); err == nil {
		t.Error("attachment without filename accepted")
	}

	missingType := base
	missingType.Attachments = []Attachment{{Filename: "x", Content: []byte("x")}}
	if err := missingType.Validate(); err == nil {
		t.Error("attachment without content-type accepted")
	}

	empty := base
	empty.Attachments = []Attachment{{Filename: "x", ContentType: "text/plain"}}
	if err := empty.Validate(); err == nil {
		t.Error("empty attachment accepted")
	}

	tooBig := base
	tooBig.Attachments = []Attachment{{Filename: "big", ContentType: "application/octet-stream", Content: make([]byte, MaxAttachmentBytes+1)}}
	if err := tooBig.Validate(); err == nil {
		t.Error("oversized attachment accepted")
	}

	ok := base
	ok.Attachments = []Attachment{{Filename: "ok.pdf", ContentType: "application/pdf", Content: []byte("data")}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid attachment rejected: %v", err)
	}
}

func TestRenderLargeAttachmentWrapsBase64(t *testing.T) {
	// 600 bytes → ~800 base64 chars, forcing several 76-char wrap lines.
	content := make([]byte, 600)
	for i := range content {
		content[i] = byte('A' + i%26)
	}
	msg := OutboundMessage{
		ID: "5", To: []string{"a@b.c"}, Subject: "big", Body: "x",
		Attachments: []Attachment{{Filename: "big.bin", ContentType: "application/octet-stream", Content: content}},
	}
	raw := mustRender(t, msg)

	// Every base64 line in the encoded part must be at most 76 chars.
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 line exceeds 76 chars: %d", len(line))
		}
	}

	m := parse(t, raw)
	_, params := mediaType(t, m)
	parts := readParts(t, m.Body, params["boundary"])
	if got := decodePart(t, parts[1]); got["body"] != string(content) {
		t.Errorf("large attachment did not round-trip (len got %d, want %d)", len(got["body"]), len(content))
	}
}

func TestBoundaryEdgeCases(t *testing.T) {
	// Non-alphanumeric id collapses to the "0" placeholder.
	if got := boundary("mixed", "@@@"); got != "bk-mixed-0" {
		t.Errorf("boundary(non-alnum id) = %q, want bk-mixed-0", got)
	}
	// Alphanumerics survive; other runes are stripped.
	if got := boundary("alt", "a1-b2.c3"); got != "bk-alt-a1b2c3" {
		t.Errorf("boundary(mixed id) = %q, want bk-alt-a1b2c3", got)
	}
	// Over-long ids truncate to the 70-char RFC limit.
	long := strings.Repeat("x", 200)
	if got := boundary("mixed", long); len(got) != 70 {
		t.Errorf("boundary(long id) len = %d, want 70", len(got))
	}
}

func TestRenderWithAwkwardIDStaysValid(t *testing.T) {
	// An id with no alphanumerics still yields a parseable multipart message.
	msg := OutboundMessage{
		ID: "@@@", To: []string{"a@b.c"}, Subject: "x", Body: "b",
		Attachments: []Attachment{{Filename: "f", ContentType: "text/plain", Content: []byte("d")}},
	}
	m := parse(t, mustRender(t, msg))
	if mt, _ := mediaType(t, m); mt != "multipart/mixed" {
		t.Errorf("media type = %q, want multipart/mixed", mt)
	}
}

// failWriter fails every Write, to exercise the render error paths that an
// in-memory buffer never triggers.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRenderErrorPropagation(t *testing.T) {
	msg := OutboundMessage{
		ID: "e", To: []string{"a@b.c"}, Subject: "s", Body: "b", HTMLBody: "<b>h</b>",
		Attachments: []Attachment{{Filename: "f", ContentType: "text/plain", Content: []byte("data that is long enough to wrap once base64 encoded across lines")}},
	}
	fw := failWriter{}

	cases := []struct {
		name string
		fn   func() error
	}{
		{"renderPlain", func() error { return renderPlain(fw, msg) }},
		{"renderAlternative", func() error { return renderAlternative(fw, msg) }},
		{"renderMixed", func() error { return renderMixed(fw, msg) }},
		{"writeQuotedPrintable", func() error { return writeQuotedPrintable(fw, "body") }},
		{"writeBase64Wrapped", func() error { return writeBase64Wrapped(fw, msg.Attachments[0].Content) }},
		{"writeTextPart", func() error { return writeTextPart(multipart.NewWriter(fw), "text/plain", "x") }},
		{"writeAttachmentPart", func() error { return writeAttachmentPart(multipart.NewWriter(fw), msg.Attachments[0]) }},
		{"writeAlternativeParts", func() error { return writeAlternativeParts(multipart.NewWriter(fw), "t", "h") }},
		{"writeAlternativePart", func() error { return writeAlternativePart(multipart.NewWriter(fw), msg) }},
	}
	for _, tc := range cases {
		if err := tc.fn(); err == nil {
			t.Errorf("%s: expected error from failing writer, got nil", tc.name)
		}
	}
}

func TestTotalMessageSizeLimit(t *testing.T) {
	// Each attachment is under MaxAttachmentBytes, but together they exceed
	// MaxMessageBytes — this must trip the total check, not the per-item one.
	each := MaxAttachmentBytes - 1
	n := MaxMessageBytes/each + 2
	atts := make([]Attachment, n)
	for i := range atts {
		atts[i] = Attachment{Filename: "a", ContentType: "application/octet-stream", Content: make([]byte, each)}
	}
	msg := OutboundMessage{To: []string{"a@b.c"}, Body: "x", Attachments: atts}
	if err := msg.Validate(); err == nil {
		t.Error("message exceeding total size limit accepted")
	}
}
