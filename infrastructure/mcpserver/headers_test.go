package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

const headersEML = "From: =?utf-8?q?Bj=C3=B6rn?= <b@example.org>\r\n" +
	"To: you@example.org\r\n" +
	"Subject: =?utf-8?q?Gru=C3=9F?=\r\n" +
	"Date: Mon, 01 Jun 2026 10:00:00 +0200\r\n" +
	"Message-Id: <abc@example.org>\r\n" +
	"\r\n" +
	"Hallo!\r\n"

func TestInboxHeadersResource(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "m1.eml", headersEML)

	text, err := client.ReadResource("email://inbox/m1.eml/headers")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("headers payload: %v", err)
	}
	if !strings.Contains(got["from"], "Björn") {
		t.Errorf("from not RFC 2047-decoded: %q", got["from"])
	}
	if got["subject"] != "Gruß" {
		t.Errorf("subject = %q, want %q", got["subject"], "Gruß")
	}
	if got["to"] != "you@example.org" || got["message_id"] != "<abc@example.org>" {
		t.Errorf("to/message_id wrong: %q / %q", got["to"], got["message_id"])
	}
	if got["date"] == "" {
		t.Error("date missing")
	}
}

func TestInboxHeadersResourceUnknownID(t *testing.T) {
	client, _ := newClient(t)
	if _, err := client.ReadResource("email://inbox/nope.eml/headers"); err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func TestParseHeadersGarbage(t *testing.T) {
	got := parseHeaders([]byte("not a message at all"))
	if got["from"] != "" || got["subject"] != "" {
		t.Errorf("garbage should yield empty fields, got %v", got)
	}
}
