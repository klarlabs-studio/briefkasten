package mcpserver

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// fetchedPairs pulls {id: raw} out of a batched fetch response, decoding
// the base64 so the test asserts on the message rather than its encoding.
func fetchedPairs(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	raw, ok := out["fetched"].([]map[string]any)
	if !ok {
		t.Fatalf("response has no fetched list: %v", out)
	}
	pairs := make(map[string]string, len(raw))
	for _, m := range raw {
		decoded, err := base64.StdEncoding.DecodeString(m["raw"].(string))
		if err != nil {
			t.Fatalf("fetched %v: raw is not base64: %v", m["id"], err)
		}
		pairs[m["id"].(string)] = string(decoded)
	}
	return pairs
}

// A batch is answered per id: the messages that were read come back with
// their bytes, the ids that could not are named with a reason, and there
// is no blanket success for the caller to misread.
func TestBulkFetchReportsPerID(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")
	drop(t, root, "b.eml", "From: x@y\r\nSubject: B\r\n\r\nb")

	out := callMap(t, client, "email.fetch", map[string]any{
		"ids": []string{"a.eml", "ghost.eml", "b.eml"},
	})
	got := fetchedPairs(t, out)
	if len(got) != 2 || got["a.eml"] != "From: x@y\r\nSubject: A\r\n\r\na" ||
		got["b.eml"] != "From: x@y\r\nSubject: B\r\n\r\nb" {
		t.Errorf("fetched = %v, want the two real messages whole", got)
	}
	if failed := failureIDs(t, out); !slices.Equal(failed, []string{"ghost.eml"}) {
		t.Errorf("failed = %v, want only ghost.eml", failed)
	}
	if _, claimed := out["ok"]; claimed {
		t.Error("a partly failed batch reported ok — the whole point is that it cannot")
	}
	if _, single := out["raw"]; single {
		t.Error("a batch answered in the single-message shape")
	}
	if out["total"] != 3 {
		t.Errorf("total = %v, want 3", out["total"])
	}
	// Fetching never marks anything seen, in bulk as singly.
	for _, name := range []string{"a.eml", "b.eml"} {
		if _, err := os.Stat(filepath.Join(root, "new", name)); err != nil {
			t.Errorf("%s left the unread backlog after a fetch: %v", name, err)
		}
	}
}

// One id keeps the response shape it always had, so a client that never
// heard of batches sees no change at all.
func TestSingleFetchFormUnchanged(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")

	out := callMap(t, client, "email.fetch", map[string]any{"id": "a.eml"})
	raw, ok := out["raw"].(string)
	if !ok {
		t.Fatalf("single fetch = %v, want a raw field", out)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || string(decoded) != "From: x@y\r\nSubject: A\r\n\r\na" {
		t.Errorf("raw = %q (err %v), want the base64 message", decoded, err)
	}
	for _, extra := range []string{"fetched", "failed", "total"} {
		if _, present := out[extra]; present {
			t.Errorf("single fetch grew a %q field: %v", extra, out)
		}
	}
}

// Exactly one of id and ids, exactly as the mutating tools require it.
func TestFetchIDAndIDsAreExclusive(t *testing.T) {
	client, root := newClient(t)
	drop(t, root, "a.eml", "From: x@y\r\nSubject: A\r\n\r\na")

	_, err := client.CallToolRaw("email.fetch", map[string]any{"id": "a.eml", "ids": []string{"a.eml"}})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("fetch with id and ids = %v, want a both-supplied refusal", err)
	}
	_, err = client.CallToolRaw("email.fetch", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no message named") {
		t.Errorf("fetch with neither = %v, want a no-target refusal", err)
	}
}

// The id cap bounds one call here as everywhere, and is refused with the
// number named rather than trimmed to the first hundred.
func TestFetchCapIsEnforcedAtTheTool(t *testing.T) {
	client, _ := newClient(t)

	_, err := client.CallToolRaw("email.fetch", map[string]any{"ids": bulkIDs(domain.MaxBulkIDs + 1)})
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("over-cap fetch = %v, want a refusal naming the cap", err)
	}
	_, err = client.CallToolRaw("email.fetch", map[string]any{"ids": []string{"a.eml", "a.eml"}})
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("duplicate ids = %v, want a refusal", err)
	}
}

// The id cap does not bound a fetch — a hundred messages with
// attachments is hundreds of megabytes — so the size budget does, and it
// refuses the call rather than returning a truncated answer.
func TestFetchRefusesOversizedBatch(t *testing.T) {
	client, root := newClient(t)
	const each = 8 << 20 // four of these is 32 MiB, over the 25 MiB budget
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = "big" + strconv.Itoa(i) + ".eml"
		drop(t, root, ids[i], "From: x@y\r\n\r\n"+strings.Repeat("x", each))
	}

	_, err := client.CallToolRaw("email.fetch", map[string]any{"ids": ids})
	if err == nil {
		t.Fatal("oversized batch accepted; want a refusal")
	}
	// Actionable: the budget, the measured total, and the id count, so
	// the caller can split the batch without guessing.
	for _, want := range []string{"25 MiB", "4 ids", "fewer ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}

	// Fetching them one at a time is unaffected: the budget bounds a
	// batch, it does not make a message unreachable.
	out := callMap(t, client, "email.fetch", map[string]any{"id": ids[0]})
	decoded, decErr := base64.StdEncoding.DecodeString(out["raw"].(string))
	if decErr != nil || len(decoded) < each {
		t.Errorf("single fetch of a large message = %d bytes (err %v), want it whole", len(decoded), decErr)
	}
}
