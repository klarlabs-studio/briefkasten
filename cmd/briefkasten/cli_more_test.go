package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmPrompt(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"Y\n", true},
		{"n\n", false},
		{"nope\n", false},
		{"", false}, // EOF without an answer never proceeds
	}
	for _, tc := range cases {
		var out bytes.Buffer
		got := confirmPrompt(strings.NewReader(tc.input), &out, "delete", []string{"m1.eml"}, "")
		if got != tc.want {
			t.Errorf("confirmPrompt(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !strings.Contains(out.String(), "m1.eml") {
			t.Errorf("prompt for %q does not name the message: %q", tc.input, out.String())
		}
	}
}

func TestNeedsMailbox(t *testing.T) {
	for _, cmd := range []string{"send", "retry", "outbox"} {
		if needsMailbox(cmd) {
			t.Errorf("needsMailbox(%q) = true, want false", cmd)
		}
	}
	for _, cmd := range []string{"list", "read", "archive"} {
		if !needsMailbox(cmd) {
			t.Errorf("needsMailbox(%q) = false, want true", cmd)
		}
	}
}

func TestLoadAttachments(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.txt")
	if _, err := loadAttachments([]string{missing}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing file err = %v, want the path named", err)
	}

	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("hallo welt"), 0o644); err != nil {
		t.Fatal(err)
	}
	atts, err := loadAttachments([]string{path})
	if err != nil {
		t.Fatalf("loadAttachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if atts[0].Filename != "note.txt" {
		t.Errorf("filename = %q", atts[0].Filename)
	}
	if atts[0].ContentType == "" {
		t.Error("content type empty")
	}
	if string(atts[0].Content) != "hallo welt" {
		t.Errorf("content = %q", atts[0].Content)
	}

	atts, err = loadAttachments(nil)
	if atts != nil || err != nil {
		t.Errorf("empty paths = %v, %v, want nil, nil", atts, err)
	}
}

// writeOutboxConfig prepares a config with a dir-sender outbox.
func writeOutboxConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "briefkasten.yaml")
	cfg := "maildir: " + filepath.Join(root, "in") + "\n" +
		"outbox:\n" +
		"  dir: " + filepath.Join(root, "outbox") + "\n" +
		"  from: me@x.y\n" +
		"  deliver_dir: " + filepath.Join(root, "delivered") + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestCLIOutboxListsSent(t *testing.T) {
	cfgPath := writeOutboxConfig(t)

	code, out, errOut := runCLI(t, "",
		"send", "--config", cfgPath,
		"--to", "advisor@x.y", "--subject", "Filing", "--body", "see inbox")
	if code != 0 {
		t.Fatalf("send failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "sent") {
		t.Fatalf("send output = %q, want sent state", out)
	}

	code, out, errOut = runCLI(t, "", "outbox", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("outbox failed: code=%d err=%q", code, errOut)
	}
	if !strings.Contains(out, "sent: ") {
		t.Errorf("outbox output = %q, want the delivered id under sent", out)
	}
	if strings.Contains(out, "queued: ") || strings.Contains(out, "failed: ") {
		t.Errorf("outbox output = %q, want nothing queued or failed", out)
	}
}

func TestCLIRetryUnknownID(t *testing.T) {
	cfgPath := writeOutboxConfig(t)

	code, _, errOut := runCLI(t, "", "retry", "--config", cfgPath, "doesnotexist")
	if code != 1 {
		t.Fatalf("retry unknown id = %d, want 1", code)
	}
	if errOut == "" {
		t.Error("retry unknown id printed no error")
	}
}

func TestCLIOutboxCommandsWithoutOutbox(t *testing.T) {
	cfg, _ := writeCLIConfig(t)

	code, _, errOut := runCLI(t, "", "outbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "no outbox configured") {
		t.Errorf("outbox = %d %q, want 1 with no-outbox error", code, errOut)
	}

	code, _, errOut = runCLI(t, "", "retry", "--config", cfg, "some-id")
	if code != 1 || !strings.Contains(errOut, "no outbox configured") {
		t.Errorf("retry = %d %q, want 1 with no-outbox error", code, errOut)
	}
}
