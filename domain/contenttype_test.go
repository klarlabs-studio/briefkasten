package domain_test

import (
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// The content type is written verbatim into a MIME part header, so CRLF
// would let a caller forge extra headers and a whole extra part.
func TestValidateContentTypeRejectsInjection(t *testing.T) {
	for _, bad := range []string{
		"text/plain\r\nX-Injected: yes",
		"text/plain\nX-Injected: yes",
		"text/plain\r\nX-Injected: yes\r\n\r\nSMUGGLED BODY",
		"\r\n",
		"not a media type",
		"text/",
	} {
		if err := domain.ValidateContentType(bad); err == nil {
			t.Errorf("ValidateContentType(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateContentTypeAcceptsRealTypes(t *testing.T) {
	for _, good := range []string{
		"text/plain",
		"application/pdf",
		"text/plain; charset=utf-8",
		"application/octet-stream",
	} {
		if err := domain.ValidateContentType(good); err != nil {
			t.Errorf("ValidateContentType(%q) = %v, want nil", good, err)
		}
	}
}

// The guard must be reached through the message invariants, not only by
// calling it directly — Enqueue validates before anything is persisted.
func TestValidateRejectsAttachmentContentTypeInjection(t *testing.T) {
	msg := domain.OutboundMessage{
		To:      []string{"a@b.c"},
		Subject: "s",
		Body:    "b",
		Attachments: []domain.Attachment{{
			Filename:    "ok.txt",
			ContentType: "text/plain\r\nX-Injected: yes\r\n\r\nSMUGGLED",
			Content:     []byte("A"),
		}},
	}
	err := msg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an attachment content type carrying CRLF")
	}
	if !strings.Contains(err.Error(), "ok.txt") {
		t.Errorf("error = %v, want it to name the offending attachment", err)
	}
}

func TestValidateAcceptsNormalAttachment(t *testing.T) {
	msg := domain.OutboundMessage{
		To:      []string{"a@b.c"},
		Subject: "s",
		Body:    "b",
		Attachments: []domain.Attachment{{
			Filename:    "ok.txt",
			ContentType: "text/plain; charset=utf-8",
			Content:     []byte("A"),
		}},
	}
	if err := msg.Validate(); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}
