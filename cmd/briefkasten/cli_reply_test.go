package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliThread is the message the CLI reply tests answer.
const cliThread = "From: Alice <alice@example.com>\r\n" +
	"To: me@x.y, Bob <bob@example.com>\r\n" +
	"Cc: carol@example.com\r\n" +
	"Bcc: hidden@example.com\r\n" +
	"Subject: Q3 planning\r\n" +
	"Message-Id: <orig-1@example.com>\r\n" +
	"\r\nShall we meet?\r\n"

// writeReplyConfig prepares a mailbox holding one message plus a
// dir-sender outbox, and returns the config path and the directory
// delivered mail lands in.
func writeReplyConfig(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	inbox := filepath.Join(root, "in")
	if err := os.MkdirAll(filepath.Join(inbox, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "new", "orig.eml"), []byte(cliThread), 0o644); err != nil {
		t.Fatal(err)
	}
	delivered := filepath.Join(root, "delivered")
	cfgPath := filepath.Join(root, "briefkasten.yaml")
	cfg := "maildir: " + inbox + "\n" +
		"outbox:\n" +
		"  dir: " + filepath.Join(root, "outbox") + "\n" +
		"  from: me@x.y\n" +
		"  deliver_dir: " + delivered + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, filepath.Join(delivered, "new")
}

// onlyDelivered reads the single message the dir sender wrote.
func onlyDelivered(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read delivered dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// `reply --all` derives the whole audience from the original and states
// it before the message goes — the recipients are the one thing the
// operator did not type.
func TestCLIReplyAll(t *testing.T) {
	cfgPath, delivered := writeReplyConfig(t)

	code, out, errOut := runCLI(t, "",
		"reply", "--config", cfgPath, "--all", "--body", "Tuesday works.", "orig.eml")
	if code != 0 {
		t.Fatalf("reply failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "to 3 recipient(s)") {
		t.Errorf("reply did not state the audience: %q", out)
	}
	if !strings.Contains(out, "sent: ") {
		t.Errorf("reply output = %q, want the delivered state", out)
	}

	raw := onlyDelivered(t, delivered)
	for _, want := range []string{
		"Subject: Re: Q3 planning",
		"To: \"Alice\" <alice@example.com>",
		"In-Reply-To: <orig-1@example.com>",
		"References: <orig-1@example.com>",
		"bob@example.com",
		"carol@example.com",
		"Tuesday works.",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("delivered reply is missing %q:\n%s", want, raw)
		}
	}
	// Our own address is not a recipient, and the original's blind copy
	// is not one either.
	if strings.Contains(raw, "To: me@x.y") || strings.Contains(raw, "Cc: me@x.y") {
		t.Errorf("the reply was addressed back to this outbox:\n%s", raw)
	}
	if strings.Contains(raw, "hidden@example.com") {
		t.Errorf("the reply leaked the original's Bcc:\n%s", raw)
	}
}

// `forward` attaches the original whole and goes only where --to said.
func TestCLIForward(t *testing.T) {
	cfgPath, delivered := writeReplyConfig(t)

	code, out, errOut := runCLI(t, "",
		"forward", "--config", cfgPath, "--to", "dave@example.com", "--body", "fyi", "orig.eml")
	if code != 0 {
		t.Fatalf("forward failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "to 1 recipient(s)") || !strings.Contains(out, "dave@example.com") {
		t.Errorf("forward did not state the audience: %q", out)
	}

	raw := onlyDelivered(t, delivered)
	for _, want := range []string{
		"Subject: Fwd: Q3 planning",
		"To: dave@example.com",
		"message/rfc822",
		"forwarded-message.eml",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("delivered forward is missing %q:\n%s", want, raw)
		}
	}
	// The original's audience appears only inside the forwarded header
	// block a human reads — never in the new message's own headers, where
	// it would be a recipient.
	headers, _, _ := strings.Cut(raw, "\r\n\r\n")
	for _, addr := range []string{"carol@example.com", "hidden@example.com", "bob@example.com"} {
		if strings.Contains(headers, addr) {
			t.Errorf("the forward addressed %s, who was on the original:\n%s", addr, headers)
		}
	}
	// And the blind copy is never reproduced in readable text.
	if strings.Contains(raw, "hidden@example.com") {
		t.Errorf("the forward reproduced the original's Bcc in clear text:\n%s", raw)
	}
}

// `send --bcc` puts the address in the envelope's business, never in the
// message. The dir sender writes the rendered bytes, so a Bcc appearing
// there would be a Bcc every recipient could read.
func TestCLISendBccIsNotRendered(t *testing.T) {
	cfgPath, delivered := writeReplyConfig(t)

	code, out, errOut := runCLI(t, "",
		"send", "--config", cfgPath, "--to", "a@b.c", "--cc", "c@d.e",
		"--bcc", "hidden@x.y", "--subject", "S", "--body", "B")
	if code != 0 {
		t.Fatalf("send failed: code=%d out=%q err=%q", code, out, errOut)
	}

	raw := onlyDelivered(t, delivered)
	if !strings.Contains(raw, "Cc: c@d.e") {
		t.Errorf("Cc missing from the rendered message:\n%s", raw)
	}
	if strings.Contains(raw, "hidden@x.y") || strings.Contains(raw, "Bcc:") {
		t.Errorf("the blind copy was rendered into the message:\n%s", raw)
	}
}

// Both commands refuse rather than guess when the arguments they need
// are missing, and both need an outbox.
func TestCLIReplyAndForwardUsage(t *testing.T) {
	cfgPath, _ := writeReplyConfig(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"reply without id", []string{"reply", "--config", cfgPath, "--body", "x"}, "usage: briefkasten reply"},
		{"reply without body", []string{"reply", "--config", cfgPath, "orig.eml"}, "usage: briefkasten reply"},
		{"forward without to", []string{"forward", "--config", cfgPath, "orig.eml"}, "usage: briefkasten forward"},
	} {
		code, _, errOut := runCLI(t, "", tc.args...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s = %d %q, want 2 with %q", tc.name, code, errOut, tc.want)
		}
	}

	// A mailbox with no outbox cannot answer anything.
	plain, _ := writeCLIConfig(t)
	code, _, errOut := runCLI(t, "", "reply", "--config", plain, "--body", "x", "m1.eml")
	if code != 1 || !strings.Contains(errOut, "no outbox configured") {
		t.Errorf("reply without outbox = %d %q, want 1", code, errOut)
	}

	// An unknown id is the mailbox's error, not a silent no-op.
	code, _, errOut = runCLI(t, "", "forward", "--config", cfgPath, "--to", "a@b.c", "nope.eml")
	if code != 1 || errOut == "" {
		t.Errorf("forward of an unknown id = %d %q, want 1 with an error", code, errOut)
	}
}
