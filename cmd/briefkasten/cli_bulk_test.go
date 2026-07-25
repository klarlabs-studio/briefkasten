package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten"
)

// writeCLIBulkConfig prepares a maildir holding the named unread
// messages and returns the config path and the maildir root.
func writeCLIBulkConfig(t *testing.T, names ...string) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, "new", name),
			[]byte("From: a@b.c\r\nSubject: "+name+"\r\n\r\nhallo"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(t.TempDir(), "briefkasten.yaml")
	if err := os.WriteFile(cfgPath, []byte("maildir: "+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

// One prompt authorises the whole batch, so it has to say how big the
// batch is and where the mail is going before the human answers.
func TestCLIBulkDeletePromptNamesCountAndDestination(t *testing.T) {
	cfg, root := writeCLIBulkConfig(t, "a.eml", "b.eml", "c.eml")

	code, out, _ := runCLI(t, "n\n", "delete", "--config", cfg, "a.eml", "b.eml", "c.eml")
	if code == 0 {
		t.Fatalf("aborted bulk delete exited 0: %q", out)
	}
	for _, want := range []string{"3 messages", ".trash", "a.eml, b.eml, c.eml", "never destroyed"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt = %q, want it to contain %q", out, want)
		}
	}
	for _, name := range []string{"a.eml", "b.eml", "c.eml"} {
		if _, err := os.Stat(filepath.Join(root, "new", name)); err != nil {
			t.Errorf("%s moved despite the abort: %v", name, err)
		}
	}

	// Answering yes once moves the whole batch.
	code, out, errOut := runCLI(t, "y\n", "delete", "--config", cfg, "a.eml", "b.eml", "c.eml")
	if code != 0 {
		t.Fatalf("confirmed bulk delete = %d, %q / %q", code, out, errOut)
	}
	for _, name := range []string{"a.eml", "b.eml", "c.eml"} {
		if _, err := os.Stat(filepath.Join(root, ".trash", "new", name)); err != nil {
			t.Errorf("%s not in trash: %v", name, err)
		}
	}
}

// A batch that partly failed reports both halves and exits non-zero: a
// script must not read "some of it worked" as success.
func TestCLIBulkArchivePartialFailure(t *testing.T) {
	cfg, root := writeCLIBulkConfig(t, "a.eml", "b.eml")

	code, out, errOut := runCLI(t, "", "archive", "--config", cfg, "--yes", "a.eml", "ghost.eml", "b.eml")
	if code == 0 {
		t.Errorf("partly failed batch exited 0")
	}
	for _, want := range []string{"archived: a.eml", "archived: b.eml"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if !strings.Contains(errOut, "ghost.eml") {
		t.Errorf("stderr = %q, want the failed id named", errOut)
	}
	// The good ones really moved; nothing was rolled back.
	for _, name := range []string{"a.eml", "b.eml"} {
		if _, err := os.Stat(filepath.Join(root, ".archive", "new", name)); err != nil {
			t.Errorf("%s not archived: %v", name, err)
		}
	}
}

// The machine-readable form carries the same per-id verdict.
func TestCLIBulkSeenJSONReportsPerID(t *testing.T) {
	cfg, _ := writeCLIBulkConfig(t, "a.eml", "b.eml")

	code, out, _ := runCLI(t, "", "seen", "--config", cfg, "--json", "a.eml", "ghost.eml", "b.eml")
	if code == 0 {
		t.Errorf("partly failed batch exited 0")
	}
	var payload struct {
		Seen   []string `json:"seen"`
		Failed []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"failed"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out)
	}
	if len(payload.Seen) != 2 || payload.Total != 3 {
		t.Errorf("payload = %+v, want two marked out of three", payload)
	}
	if len(payload.Failed) != 1 || payload.Failed[0].ID != "ghost.eml" || payload.Failed[0].Error == "" {
		t.Errorf("failed = %+v, want ghost.eml with a reason", payload.Failed)
	}
}

// The cap is refused before anything moves, with the number named.
func TestCLIBulkCapRefused(t *testing.T) {
	cfg, root := writeCLIBulkConfig(t, "a.eml")

	ids := make([]string, briefkasten.MaxBulkIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d.eml", i)
	}
	args := append([]string{"delete", "--config", cfg, "--yes"}, ids...)

	code, _, errOut := runCLI(t, "", args...)
	if code == 0 || !strings.Contains(errOut, "100") {
		t.Errorf("over-cap delete = %d %q, want a refusal naming the cap", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "a.eml")); err != nil {
		t.Errorf("message disturbed by a refused batch: %v", err)
	}
}
