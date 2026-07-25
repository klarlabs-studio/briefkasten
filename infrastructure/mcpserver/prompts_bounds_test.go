package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/briefkasten/domain"
)

// fillInbox drops n unread messages, lexicographically ordered so the
// embedded slice is predictable.
func fillInbox(t *testing.T, root string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		drop(t, root, fmt.Sprintf("m%04d.eml", i),
			fmt.Sprintf("From: a@b.c\r\nSubject: Post %d\r\n\r\nhallo", i))
	}
}

// embedded counts the messages a rendered prompt actually quotes.
func embedded(text string) int { return strings.Count(text, "--- Message") }

// TestSummarizeInboxClampsToCap asks for far more than the cap on an inbox
// that holds more than the cap: the prompt embeds the cap, says it was
// clamped, and does not tell the caller to try again with a higher count.
func TestSummarizeInboxClampsToCap(t *testing.T) {
	client, root := newClient(t)
	over := domain.MaxSummaryMessages + 5
	fillInbox(t, root, over)

	text := promptText(t, client, "summarize_inbox", map[string]string{"count": "5000"})
	if got := embedded(text); got != domain.MaxSummaryMessages {
		t.Errorf("embedded messages = %d, want the %d-message cap", got, domain.MaxSummaryMessages)
	}
	if !strings.Contains(text, fmt.Sprintf("'count' asked for 5000 messages and was clamped to %d", domain.MaxSummaryMessages)) {
		t.Errorf("prompt silent about the clamp: %s", text)
	}
	// The "more not shown" line must be accurate at the cap: it names the
	// real remainder and no longer promises a higher 'count' would help.
	if !strings.Contains(text, fmt.Sprintf("%d more unread messages not shown", over-domain.MaxSummaryMessages)) {
		t.Errorf("prompt miscounts the hidden messages: %s", text)
	}
	if !strings.Contains(text, "a higher 'count' will not show more") {
		t.Errorf("prompt at the cap still advertises a higher 'count': %s", text)
	}
	if strings.Contains(text, "re-run with a higher 'count'") {
		t.Errorf("prompt at the cap keeps the pre-cap wording: %s", text)
	}
}

// TestSummarizeInboxSensibleCountsUnchanged pins the behaviour below the
// cap: the default of 20, the original hidden-message wording, and no
// clamp notice for a count nobody needs to clamp.
func TestSummarizeInboxSensibleCountsUnchanged(t *testing.T) {
	client, root := newClient(t)
	fillInbox(t, root, 30)

	// No argument: the documented default, still 20.
	text := promptText(t, client, "summarize_inbox", nil)
	if got := embedded(text); got != defaultSummarizeCount {
		t.Errorf("default embedded messages = %d, want %d", got, defaultSummarizeCount)
	}
	want := fmt.Sprintf("(%d more unread messages not shown — re-run with a higher 'count' or list them with email.list_unread.)",
		30-defaultSummarizeCount)
	if !strings.Contains(text, want) {
		t.Errorf("default prompt lost the pre-cap wording: %s", text)
	}
	if strings.Contains(text, "clamped") {
		t.Errorf("default prompt claims a clamp: %s", text)
	}

	// An explicit count under the cap is honoured exactly.
	text = promptText(t, client, "summarize_inbox", map[string]string{"count": "25"})
	if got := embedded(text); got != 25 {
		t.Errorf("count=25 embedded %d messages", got)
	}
	if strings.Contains(text, "clamped") || strings.Contains(text, "will not show more") {
		t.Errorf("count=25 treated as over the cap: %s", text)
	}

	// A count above the backlog still embeds everything and hides nothing,
	// clamp or no clamp.
	text = promptText(t, client, "summarize_inbox", map[string]string{"count": "5000"})
	if got := embedded(text); got != 30 {
		t.Errorf("count over the backlog embedded %d messages, want all 30", got)
	}
	if strings.Contains(text, "not shown") {
		t.Errorf("nothing was hidden, but the prompt says otherwise: %s", text)
	}
}

// TestSummarizeInboxClampBoundsPromptSize is the resource half of the cap:
// a clamped request still truncates every message it embeds, so the whole
// prompt stays inside cap × maxEmbeddedMessageBytes.
func TestSummarizeInboxClampBoundsPromptSize(t *testing.T) {
	client, root := newClient(t)
	fillInbox(t, root, domain.MaxSummaryMessages+5)
	// Sorts ahead of the m0001.eml… batch, so it lands inside the cap.
	drop(t, root, "big0000.eml",
		"From: a@b.c\r\nSubject: Riesig\r\n\r\n"+strings.Repeat("x", maxEmbeddedMessageBytes+1024))

	text := promptText(t, client, "summarize_inbox", map[string]string{"count": "5000"})
	if !strings.Contains(text, "[... truncated ...]") {
		t.Error("clamped prompt embeds an oversized message untruncated")
	}
	if limit := domain.MaxSummaryMessages * maxEmbeddedMessageBytes; len(text) > limit {
		t.Errorf("clamped prompt is %d bytes, over the %d-byte worst case", len(text), limit)
	}
}

// TestSummarizeInboxCapIsNotAnError keeps the clamp a clamp: an absurd
// count is answered, not refused, while a non-positive one still fails.
func TestSummarizeInboxCapIsNotAnError(t *testing.T) {
	client, root := newClient(t)
	fillInbox(t, root, 3)

	for _, count := range []string{"101", "5000", "999999999"} {
		if _, err := client.GetPrompt("summarize_inbox", map[string]string{"count": count}); err != nil {
			t.Errorf("count=%s refused: %v", count, err)
		}
	}
	for _, count := range []string{"0", "-1", "abc", "1e6"} {
		if _, err := client.GetPrompt("summarize_inbox", map[string]string{"count": count}); err == nil {
			t.Errorf("count=%s accepted", count)
		}
	}
}
