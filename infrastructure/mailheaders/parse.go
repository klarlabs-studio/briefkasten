// Package mailheaders parses RFC 5322 fields used for message triage.
package mailheaders

import (
	"bytes"
	"mime"
	"net/mail"

	"go.klarlabs.de/briefkasten/domain"
)

// Parse extracts a compact set of decoded headers from a raw message.
func Parse(raw []byte) domain.MessageHeaders {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return domain.MessageHeaders{}
	}
	decoder := mime.WordDecoder{}
	decode := decoder.DecodeHeader
	value := func(name string) string {
		v := msg.Header.Get(name)
		if decoded, err := decode(v); err == nil {
			return decoded
		}
		return v
	}
	return domain.MessageHeaders{
		From: value("From"), To: value("To"), Subject: value("Subject"),
		Date: msg.Header.Get("Date"), MessageID: msg.Header.Get("Message-Id"),
	}
}
