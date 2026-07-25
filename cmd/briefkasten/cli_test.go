package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCLIConfig prepares a maildir with one unread message and returns
// the config path.
func writeCLIConfig(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new", "m1.eml"),
		[]byte("From: a@b.c\r\nSubject: CLI\r\n\r\nhallo"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(cfgPath, []byte("maildir: "+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestCLIListAndRead(t *testing.T) {
	cfg, _ := writeCLIConfig(t)

	code, out, _ := runCLI(t, "", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "m1.eml") {
		t.Fatalf("list = %d %q", code, out)
	}

	code, out, _ = runCLI(t, "", "read", "--config", cfg, "m1.eml")
	if code != 0 || !strings.Contains(out, "Subject: CLI") {
		t.Fatalf("read = %d %q", code, out)
	}

	code, out, _ = runCLI(t, "", "search", "--config", cfg, "hallo")
	if code != 0 || !strings.Contains(out, "m1.eml") {
		t.Fatalf("search = %d %q", code, out)
	}

	code, out, _ = runCLI(t, "", "list", "--config", cfg, "--json")
	if code != 0 || !strings.Contains(out, `"ids"`) {
		t.Fatalf("json list = %d %q", code, out)
	}
}

func TestCLIDeletePromptsAndAborts(t *testing.T) {
	cfg, root := writeCLIConfig(t)

	// "n" aborts; the message survives.
	code, out, _ := runCLI(t, "n\n", "delete", "--config", cfg, "m1.eml")
	if code == 0 {
		t.Fatalf("aborted delete exited 0: %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "m1.eml")); err != nil {
		t.Fatal("message gone despite abort")
	}

	// "y" proceeds to trash.
	code, _, errOut := runCLI(t, "y\n", "delete", "--config", cfg, "m1.eml")
	if code != 0 {
		t.Fatalf("confirmed delete failed: %s", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "new", "m1.eml")); err != nil {
		t.Errorf("not in trash: %v", err)
	}
}

func TestCLIArchiveWithYes(t *testing.T) {
	cfg, root := writeCLIConfig(t)
	code, _, errOut := runCLI(t, "", "archive", "--config", cfg, "--yes", "m1.eml")
	if code != 0 {
		t.Fatalf("archive --yes failed: %s", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".archive", "new", "m1.eml")); err != nil {
		t.Errorf("not archived: %v", err)
	}
}

func TestCLIUnknownCommand(t *testing.T) {
	code, _, errOut := runCLI(t, "", "frobnicate")
	if code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("unknown = %d %q", code, errOut)
	}
}

func TestCLISendWithAttachment(t *testing.T) {
	root := t.TempDir()
	outboxDir := filepath.Join(root, "outbox")
	deliverDir := filepath.Join(root, "delivered")
	cfgPath := filepath.Join(root, "briefkasten.yaml")
	cfg := "maildir: " + filepath.Join(root, "in") + "\n" +
		"outbox:\n" +
		"  dir: " + outboxDir + "\n" +
		"  from: me@x.y\n" +
		"  deliver_dir: " + deliverDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	attPath := filepath.Join(root, "filing.pdf")
	if err := os.WriteFile(attPath, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "",
		"send", "--config", cfgPath,
		"--to", "advisor@x.y", "--subject", "Filing", "--body", "see attached",
		"--attach", attPath)
	if code != 0 {
		t.Fatalf("send failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "sent") {
		t.Errorf("send output = %q, want sent state", out)
	}

	// DirSender drops the rendered message as .eml in deliver_dir/new/.
	entries, err := os.ReadDir(filepath.Join(deliverDir, "new"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no delivered message: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(deliverDir, "new", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "multipart/mixed") {
		t.Errorf("delivered message is not multipart/mixed:\n%s", got)
	}
	if !strings.Contains(got, "filename=filing.pdf") && !strings.Contains(got, `filename="filing.pdf"`) {
		t.Errorf("delivered message missing attachment filename:\n%s", got)
	}
	if !strings.Contains(got, "application/pdf") {
		t.Errorf("delivered message missing attachment content type:\n%s", got)
	}
}

// Version reporting runs before config loading and mailbox construction:
// asking a binary what it is must work on a machine with no mailbox
// configured at all, which is exactly when someone is filing a bug.
func TestCLIVersionFlagAndSubcommand(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "version"} {
		code, out, errOut := runCLI(t, "", arg)
		if code != 0 {
			t.Errorf("%s = exit %d, stderr %q", arg, code, errOut)
		}
		if !strings.HasPrefix(out, "briefkasten ") {
			t.Errorf("%s printed %q, want it to lead with the binary name", arg, out)
		}
		for _, want := range []string{version, "commit:", commit, "built:", date} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output %q missing %q", arg, out, want)
			}
		}
	}
}

// The machine-readable form exists for bug reports and support scripts.
func TestCLIVersionJSON(t *testing.T) {
	code, out, _ := runCLI(t, "", "--version", "--json")
	if code != 0 {
		t.Fatalf("--version --json = exit %d", code)
	}
	var got struct {
		Version  string `json:"version"`
		Commit   string `json:"commit"`
		Date     string `json:"date"`
		Go       string `json:"go"`
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v (%q)", err, out)
	}
	if got.Version != version || got.Commit != commit || got.Date != date {
		t.Errorf("json = %+v, want the build vars", got)
	}
	if got.Go == "" || got.Platform == "" {
		t.Errorf("json = %+v, want go and platform filled for bug reports", got)
	}
}

// A version request must not be mistaken for a mailbox command, even
// though every other verb needs a working backend.
func TestCLIVersionNeedsNoConfig(t *testing.T) {
	t.Setenv("BRIEFKASTEN_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	code, out, errOut := runCLI(t, "", "--version")
	if code != 0 {
		t.Fatalf("--version with a missing config = exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, version) {
		t.Errorf("out = %q, want the version", out)
	}
}
