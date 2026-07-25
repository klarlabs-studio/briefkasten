package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bodyOf is the content writeCLIBulkConfig gives a named message.
func bodyOf(name string) string {
	return "From: a@b.c\r\nSubject: " + name + "\r\n\r\nhallo"
}

// writeMessage drops an unread message of arbitrary content into the
// maildir, for the sizes writeCLIBulkConfig's fixed body cannot reach.
func writeMessage(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "new", name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readRecords parses the length-prefixed stream `briefkasten read`
// produces for several ids: a header line "id <id> <bytes>", then
// exactly that many bytes, then a newline. Parsing it here is the point
// — the format is only useful if a consumer can walk it without
// guessing where a message ends.
func readRecords(t *testing.T, out string) map[string]string {
	t.Helper()
	r := bufio.NewReader(strings.NewReader(out))
	msgs := map[string]string{}
	for {
		header, err := r.ReadString('\n')
		if err == io.EOF && strings.TrimSpace(header) == "" {
			return msgs
		}
		if err != nil && err != io.EOF {
			t.Fatalf("reading header: %v", err)
		}
		fields := strings.Fields(strings.TrimSpace(header))
		if len(fields) != 3 || fields[0] != "id" {
			t.Fatalf("header = %q, want `id <id> <bytes>`", header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("header %q: byte count is not a number: %v", header, err)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			t.Fatalf("message %s: want %d bytes, got %v", fields[1], size, err)
		}
		msgs[fields[1]] = string(body)
		if _, err := r.ReadString('\n'); err != nil && err != io.EOF {
			t.Fatalf("message %s: no separator after the body: %v", fields[1], err)
		}
	}
}

// One id prints the message and nothing else — the form every existing
// pipeline already parses.
func TestCLIReadSingleIDUnchanged(t *testing.T) {
	cfg, _ := writeCLIBulkConfig(t, "a.eml")

	code, out, errOut := runCLI(t, "", "read", "--config", cfg, "a.eml")
	if code != 0 {
		t.Fatalf("read = %d, %q / %q", code, out, errOut)
	}
	if out != bodyOf("a.eml")+"\n" {
		t.Errorf("read = %q, want the bare message", out)
	}
}

// Several ids are length-prefixed, because no marker line can safely
// delimit mail: any delimiter can occur inside a message, and escaping
// mail is how a consumer ends up parsing something that is no longer the
// message.
func TestCLIReadManyIsLengthPrefixed(t *testing.T) {
	cfg, _ := writeCLIBulkConfig(t, "a.eml", "b.eml")

	code, out, errOut := runCLI(t, "", "read", "--config", cfg, "a.eml", "b.eml")
	if code != 0 {
		t.Fatalf("read = %d, %q / %q", code, out, errOut)
	}
	msgs := readRecords(t, out)
	if len(msgs) != 2 || msgs["a.eml"] != bodyOf("a.eml") || msgs["b.eml"] != bodyOf("b.eml") {
		t.Errorf("parsed = %v, want both messages whole", msgs)
	}
}

// A batch that partly failed names the failure and exits non-zero: a
// script must not read "some of it worked" as success.
func TestCLIReadManyReportsFailuresAndExitsNonZero(t *testing.T) {
	cfg, _ := writeCLIBulkConfig(t, "a.eml")

	code, out, errOut := runCLI(t, "", "read", "--config", cfg, "a.eml", "ghost.eml")
	if code == 0 {
		t.Errorf("partly failed read exited 0")
	}
	if !strings.Contains(errOut, "ghost.eml") {
		t.Errorf("stderr = %q, want the unreadable id named", errOut)
	}
	// The good message is still delivered, and still parses.
	if msgs := readRecords(t, out); len(msgs) != 1 || msgs["a.eml"] != bodyOf("a.eml") {
		t.Errorf("stdout = %v, want the readable message", msgs)
	}
}

// --json gives the structured per-id form, with raw base64-encoded.
func TestCLIReadManyJSON(t *testing.T) {
	cfg, _ := writeCLIBulkConfig(t, "a.eml", "b.eml")

	code, out, errOut := runCLI(t, "", "read", "--config", cfg, "--json", "a.eml", "ghost.eml", "b.eml")
	if code == 0 {
		t.Errorf("partly failed read exited 0 (%q)", errOut)
	}
	var got struct {
		Fetched []struct {
			ID  string `json:"id"`
			Raw []byte `json:"raw"`
		} `json:"fetched"`
		Failed []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"failed"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v (%q)", err, out)
	}
	if len(got.Fetched) != 2 || got.Total != 3 {
		t.Fatalf("fetched %d of total %d, want 2 of 3", len(got.Fetched), got.Total)
	}
	for _, m := range got.Fetched {
		if string(m.Raw) != bodyOf(m.ID) {
			t.Errorf("%s = %q, want the message", m.ID, m.Raw)
		}
	}
	if len(got.Failed) != 1 || got.Failed[0].ID != "ghost.eml" || got.Failed[0].Error == "" {
		t.Errorf("failed = %v, want ghost.eml with a reason", got.Failed)
	}

	// A single id with --json takes the same structured path, so a script
	// that always passes --json gets one shape whatever the id count.
	_, out, _ = runCLI(t, "", "read", "--config", cfg, "--json", "a.eml")
	if !strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(bodyOf("a.eml")))) {
		t.Errorf("single --json read = %q, want the base64 message", out)
	}
}

// The size budget refuses the call before anything is read, and says
// enough for the operator to split the batch.
func TestCLIReadManyRefusesOversizedBatch(t *testing.T) {
	cfg, root := writeCLIBulkConfig(t)
	const each = 8 << 20 // four of these is 32 MiB, over the 25 MiB budget
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = "big" + strconv.Itoa(i) + ".eml"
		writeMessage(t, root, ids[i], "From: a@b.c\r\n\r\n"+strings.Repeat("x", each))
	}

	code, out, errOut := runCLI(t, "", append([]string{"read", "--config", cfg}, ids...)...)
	if code == 0 {
		t.Fatalf("oversized read exited 0")
	}
	if out != "" {
		t.Errorf("refused read still wrote %d bytes to stdout", len(out))
	}
	for _, want := range []string{"25 MiB", "4 ids", "fewer ids"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("refusal = %q, want it to name %q", errOut, want)
		}
	}
}
