package main

import (
	"strings"
	"testing"
)

// After 'seen', the message must leave the default listing but stay
// reachable through --scope read / all — and 'read' must still print it.
func TestCLIListScope(t *testing.T) {
	cfg, _ := writeCLIConfig(t)

	if code, _, errOut := runCLI(t, "", "seen", "--config", cfg, "m1.eml"); code != 0 {
		t.Fatalf("seen = %d %q", code, errOut)
	}

	code, out, _ := runCLI(t, "", "list", "--config", cfg)
	if code != 0 || strings.Contains(out, "m1.eml") {
		t.Fatalf("default list = %d %q, want it empty", code, out)
	}

	code, out, _ = runCLI(t, "", "list", "--config", cfg, "--scope", "read")
	if code != 0 || !strings.Contains(out, "m1.eml") {
		t.Fatalf("list --scope read = %d %q", code, out)
	}

	code, out, _ = runCLI(t, "", "list", "--config", cfg, "--scope", "all")
	if code != 0 || !strings.Contains(out, "m1.eml") {
		t.Fatalf("list --scope all = %d %q", code, out)
	}

	code, out, _ = runCLI(t, "", "search", "--config", cfg, "--scope", "read", "hallo")
	if code != 0 || !strings.Contains(out, "m1.eml") {
		t.Fatalf("search --scope read = %d %q", code, out)
	}

	// Reading a seen message works and leaves it seen.
	code, out, _ = runCLI(t, "", "read", "--config", cfg, "m1.eml")
	if code != 0 || !strings.Contains(out, "Subject: CLI") {
		t.Fatalf("read seen message = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "list", "--config", cfg)
	if code != 0 || strings.Contains(out, "m1.eml") {
		t.Fatalf("list after reading seen mail = %d %q, want it empty", code, out)
	}
}

func TestCLIListBadScope(t *testing.T) {
	cfg, _ := writeCLIConfig(t)

	code, _, errOut := runCLI(t, "", "list", "--config", cfg, "--scope", "nonsense")
	if code == 0 {
		t.Fatal("list --scope nonsense exited 0, want a failure")
	}
	if !strings.Contains(errOut, "scope") {
		t.Errorf("stderr = %q, want it to mention the scope", errOut)
	}
}
