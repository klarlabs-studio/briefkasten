package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLICreateAndDeleteFolder(t *testing.T) {
	cfg, root := writeCLIConfig(t)

	code, out, errOut := runCLI(t, "", "folders", "--config", cfg, "--create", "Work")
	if code != 0 || !strings.Contains(out, "created: Work") {
		t.Fatalf("folders --create = %d %q, stderr %q", code, out, errOut)
	}
	for _, sub := range []string{"new", "cur", "tmp"} {
		if _, err := os.Stat(filepath.Join(root, "Work", sub)); err != nil {
			t.Errorf("Work/%s: %v", sub, err)
		}
	}
	code, out, _ = runCLI(t, "", "folders", "--config", cfg)
	if code != 0 || !strings.Contains(out, "Work") {
		t.Fatalf("folders = %d %q, want Work listed", code, out)
	}

	// Idempotent, so a script that re-runs is not punished for it.
	if code, _, errOut := runCLI(t, "", "folders", "--config", cfg, "--create", "Work"); code != 0 {
		t.Fatalf("second --create = %d, stderr %q", code, errOut)
	}

	code, out, errOut = runCLI(t, "y\n", "folders", "--config", cfg, "--delete", "Work")
	if code != 0 || !strings.Contains(out, "deleted: Work") {
		t.Fatalf("folders --delete = %d %q, stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "Work")); !os.IsNotExist(err) {
		t.Errorf("Work still on disk: %v", err)
	}
}

// The prompt is the CLI's half of the same gate MCP elicits, so only an
// explicit yes proceeds.
func TestCLIDeleteFolderPromptsAndAborts(t *testing.T) {
	cfg, root := writeCLIConfig(t)
	if code, _, errOut := runCLI(t, "", "folders", "--config", cfg, "--create", "Work"); code != 0 {
		t.Fatalf("--create = %d, stderr %q", code, errOut)
	}

	code, out, _ := runCLI(t, "n\n", "folders", "--config", cfg, "--delete", "Work")
	if code == 0 {
		t.Fatalf("aborted delete exited 0: %q", out)
	}
	if !strings.Contains(out, "no mail is destroyed") {
		t.Errorf("prompt = %q, want it to say what the delete cannot do", out)
	}
	if _, err := os.Stat(filepath.Join(root, "Work")); err != nil {
		t.Errorf("folder gone after an aborted delete: %v", err)
	}

	// --yes skips the prompt, as it does for archive and delete.
	if code, _, errOut := runCLI(t, "", "folders", "--config", cfg, "--delete", "Work", "--yes"); code != 0 {
		t.Fatalf("--delete --yes = %d, stderr %q", code, errOut)
	}
}

// The refusal has to reach the operator with the count in it: the exit
// status alone does not tell them what to do next.
func TestCLIDeleteFolderRefusesAFolderHoldingMail(t *testing.T) {
	cfg, root := writeCLIConfig(t)
	if code, _, errOut := runCLI(t, "", "folders", "--config", cfg, "--create", "Work"); code != 0 {
		t.Fatalf("--create = %d, stderr %q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(root, "Work", "new", "a.eml"), []byte("From: a\r\n\r\nx"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI(t, "y\n", "folders", "--config", cfg, "--delete", "Work")
	if code == 0 {
		t.Fatal("delete of a folder holding mail exited 0")
	}
	for _, want := range []string{"not empty", "1 message", "archive or delete them first"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q missing %q", errOut, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Work", "new", "a.eml")); err != nil {
		t.Errorf("message gone after a refused delete: %v", err)
	}
}

func TestCLIFolderFlagsAreExclusive(t *testing.T) {
	cfg, _ := writeCLIConfig(t)

	code, _, errOut := runCLI(t, "", "folders", "--config", cfg, "--create", "A", "--delete", "B")
	if code != 2 || !strings.Contains(errOut, "not both") {
		t.Errorf("both flags = %d %q, want a usage error", code, errOut)
	}
}
