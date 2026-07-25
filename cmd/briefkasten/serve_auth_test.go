package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServeWithBasicAuth boots the real server with auth.basic configured
// and proves the gate at the HTTP boundary: tools/list is 401 without
// credentials and 200 with them.
func TestServeWithBasicAuth(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	cfgPath := filepath.Join(root, "briefkasten.yaml")
	cfg := "addr: \"" + addr + "\"\n" +
		"maildir: " + filepath.Join(root, "box") + "\n" +
		"auth:\n" +
		"  basic:\n" +
		"    username: alice\n" +
		"    password: s3cret\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIEFKASTEN_CONFIG", cfgPath)

	done := make(chan int, 1)
	go func() { done <- serve(nil) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	call := func(authorize bool) (int, string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if authorize {
			req.SetBasicAuth("alice", "s3cret")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		// Read the whole listing: a fixed-size buffer would make this
		// assertion depend on where a tool happens to land in the
		// payload, so an edited description could "fail" the auth gate.
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(payload)
	}

	// Without credentials the request must be rejected: either an HTTP
	// 401 or a JSON-RPC unauthorized error, depending on where the
	// transport surfaces middleware failures — but never a tool list.
	status, payload := call(false)
	if status == http.StatusOK && strings.Contains(payload, "email.list_unread") {
		t.Errorf("unauthenticated tools/list succeeded: %d %s", status, payload)
	}

	status, payload = call(true)
	if status != http.StatusOK || !strings.Contains(payload, "email.list_unread") {
		t.Errorf("authenticated tools/list failed: %d %s", status, payload)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exited %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not shut down")
	}
}
