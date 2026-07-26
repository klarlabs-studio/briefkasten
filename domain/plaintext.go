package domain

import (
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
)

// maxPlainTextDepth bounds how far readTextBody descends into nested
// multiparts. Real mail nests two levels (mixed wrapping alternative);
// anything deeper is either exotic or a decompression bomb wearing MIME,
// and the quoted text is a courtesy either way.
const maxPlainTextDepth = 4

// readTextBody returns the first text/plain content it can find,
// descending into multiparts and decoding transfer encodings.
//
// It never reports an error. Every caller is quoting, and a body that
// cannot be extracted means a reply without a quoted original — a
// cosmetic loss the caller cannot act on, whereas an error here would
// block a reply the user can perfectly well write themselves.
func readTextBody(contentType, encoding string, r io.Reader, depth int) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || contentType == "" {
		// No usable Content-Type means RFC 5322's default: US-ASCII text.
		return decodeBody(encoding, r)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		if mediaType == "text/plain" {
			return decodeBody(encoding, r)
		}
		return ""
	}
	if depth >= maxPlainTextDepth || params["boundary"] == "" {
		return ""
	}
	mr := multipart.NewReader(r, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		text := readTextBody(part.Header.Get("Content-Type"),
			part.Header.Get("Content-Transfer-Encoding"), part, depth+1)
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
}

// decodeBody reads a leaf part, undoing its transfer encoding and
// stopping at MaxQuotedBytes.
func decodeBody(encoding string, r io.Reader) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	// One byte past the cap, so a body sitting exactly on it is not
	// reported as truncated.
	raw, err := io.ReadAll(io.LimitReader(r, MaxQuotedBytes+1))
	if err != nil && len(raw) == 0 {
		return ""
	}
	if len(raw) > MaxQuotedBytes {
		return string(raw[:MaxQuotedBytes]) + "\n[... quoted original truncated ...]"
	}
	return string(raw)
}
