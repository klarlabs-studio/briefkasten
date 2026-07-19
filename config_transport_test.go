package briefkasten_test

import (
	"testing"

	"go.klarlabs.de/briefkasten"
)

func TestResolvedTransportDefaultsToHTTP(t *testing.T) {
	cfg, err := briefkasten.LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ResolvedTransport(); got != briefkasten.TransportHTTP {
		t.Errorf("ResolvedTransport() = %q, want %q", got, briefkasten.TransportHTTP)
	}
}

func TestLoadConfigTransportStdio(t *testing.T) {
	path := writeConfig(t, "transport: stdio\n")
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ResolvedTransport(); got != briefkasten.TransportStdio {
		t.Errorf("ResolvedTransport() = %q, want %q", got, briefkasten.TransportStdio)
	}
}

func TestApplyEnvOverridesTransport(t *testing.T) {
	path := writeConfig(t, "transport: http\n")
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	t.Setenv("BRIEFKASTEN_TRANSPORT", "stdio")
	cfg.ApplyEnv()
	if got := cfg.ResolvedTransport(); got != briefkasten.TransportStdio {
		t.Errorf("ResolvedTransport() = %q, want %q", got, briefkasten.TransportStdio)
	}
}

func TestValidateTransportRejectsUnknown(t *testing.T) {
	cfg, err := briefkasten.LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Transport = "carrier-pigeon"
	if err := cfg.ValidateTransport(); err == nil {
		t.Error("want error for unknown transport, got nil")
	}
}

func TestValidateTransportAcceptsKnown(t *testing.T) {
	for _, tr := range []string{"", briefkasten.TransportHTTP, briefkasten.TransportStdio} {
		cfg, err := briefkasten.LoadConfig("")
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		cfg.Transport = tr
		if err := cfg.ValidateTransport(); err != nil {
			t.Errorf("ValidateTransport(%q) = %v, want nil", tr, err)
		}
	}
}

// Stdio speaks JSON-RPC over stdout, so anything else written there
// corrupts the stream. The server must log to stderr in that mode.
func TestLogWriterIsStderrForStdio(t *testing.T) {
	cfg, err := briefkasten.LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Transport = briefkasten.TransportStdio
	if got := cfg.LogWriterName(); got != "stderr" {
		t.Errorf("LogWriterName() = %q, want stderr", got)
	}
	cfg.Transport = briefkasten.TransportHTTP
	if got := cfg.LogWriterName(); got != "stdout" {
		t.Errorf("LogWriterName() = %q, want stdout", got)
	}
}
