package main

import "testing"

func TestParseServeFlagsDefaults(t *testing.T) {
	opts, err := parseServeFlags(nil)
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if opts.stdio {
		t.Error("stdio default = true, want false")
	}
	if opts.configPath != "" {
		t.Errorf("configPath = %q, want empty", opts.configPath)
	}
}

func TestParseServeFlagsStdio(t *testing.T) {
	opts, err := parseServeFlags([]string{"--stdio"})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if !opts.stdio {
		t.Error("stdio = false, want true")
	}
}

func TestParseServeFlagsConfig(t *testing.T) {
	opts, err := parseServeFlags([]string{"--config", "/etc/briefkasten.yaml", "--stdio"})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if opts.configPath != "/etc/briefkasten.yaml" {
		t.Errorf("configPath = %q", opts.configPath)
	}
	if !opts.stdio {
		t.Error("stdio = false, want true")
	}
}

func TestParseServeFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseServeFlags([]string{"--wat"}); err == nil {
		t.Error("want error for unknown flag, got nil")
	}
}
