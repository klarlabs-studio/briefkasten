package mcpserver

import (
	"regexp"
	"strings"
	"testing"
)

// readUI serves the bundled page the way a host would.
func readUI(t *testing.T) string {
	t.Helper()
	client, _ := newClient(t)
	page, err := client.ReadResource(InboxUIResourceURI)
	if err != nil {
		t.Fatalf("read UI resource: %v", err)
	}
	return page
}

// The bundled UI is the human surface for the same tools the model gets,
// so answering mail has to be reachable from it — not only reading and
// curating. Reply carries the all flag because reply-all is a different
// audience, not a different rendering of the same one.
func TestInboxUIAnswersMail(t *testing.T) {
	page := readUI(t)

	for _, want := range []struct{ snippet, why string }{
		{"'email.reply'", "replying is unreachable"},
		{"args.all = true", "reply-all cannot widen the audience"},
		{"'email.forward'", "forwarding is unreachable"},
		{"Forward to (comma-separated)", "forward has no recipient field, and its recipients are caller-supplied"},
	} {
		if !strings.Contains(page, want.snippet) {
			t.Errorf("inbox UI does not contain %q — %s", want.snippet, want.why)
		}
	}
}

// A reply's audience is the server's to derive from the original. The UI
// must not offer to edit it: a recipient field on a reply hands the
// audience back to the caller, which is the mistake email.reply exists
// to prevent. Forward keeps its field — those recipients are new.
func TestInboxUIDoesNotEditReplyRecipients(t *testing.T) {
	page := readUI(t)

	// The only recipient inputs on the page belong to compose (to/cc/bcc)
	// and to forward. Any further one would be a reply's.
	if got := strings.Count(page, "placeholder=\"Forward to"); got != 1 {
		t.Errorf("forward recipient fields = %d, want exactly 1", got)
	}
	if strings.Contains(page, "'email.reply'") && strings.Contains(page, "callTool('email.reply', { id, to") {
		t.Error("the reply call carries recipients — the server derives them from the original")
	}
}

// email.send grew cc and bcc; a compose form that cannot reach them
// makes the UI a worse client than the model's own tool call.
func TestInboxUIComposesWithCarbonCopies(t *testing.T) {
	page := readUI(t)

	for _, want := range []string{
		`id="cc"`,      // the carbon-copy field
		`id="bcc"`,     // ... and the blind one
		"args.cc = cc", // both reach email.send
		"args.bcc = bcc",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("inbox UI does not contain %q — compose cannot copy anyone", want)
		}
	}
}

// Searching and folder navigation are how a mailbox larger than one
// screen is read at all. Folders are read from the resource rather than
// typed, so the human picks from what exists.
func TestInboxUISearchesAndSwitchesFolders(t *testing.T) {
	page := readUI(t)

	for _, want := range []struct{ snippet, why string }{
		{"'email.search'", "there is no way to search"},
		{`id="query"`, "there is no query box"},
		{"Back to listing", "search results cannot be dismissed"},
		{`id="folder"`, "there is no folder selector"},
		{"'email://folders'", "folders are not read from the resource that lists them"},
		{"resources/read", "the bridge cannot read a resource at all"},
		{"args.folder = folder", "the chosen folder does not reach the tool calls"},
	} {
		if !strings.Contains(page, want.snippet) {
			t.Errorf("inbox UI does not contain %q — %s", want.snippet, want.why)
		}
	}
}

// The one that matters most. Every gated tool — send, reply, forward,
// archive, delete — takes a confirm flag that means "a human already
// approved this". The UI is not that human: it renders inside the host,
// and the host is what elicits. A UI that sets the flag turns every gate
// on the page into a formality, silently, and nothing else here would
// notice. The page may name the flag in prose; it must never set it.
func TestInboxUINeverConfirmsOnTheHumansBehalf(t *testing.T) {
	page := readUI(t)

	for _, pattern := range []string{
		`confirm\s*[:=]`,  // confirm: true, confirm=true, "confirm": true
		`['"]confirm['"]`, // args['confirm'] = true, { "confirm": ... }
		`\bconfirm\b\s*,`, // { id, body, confirm }
	} {
		if match := regexp.MustCompile(pattern).FindString(page); match != "" {
			t.Errorf("inbox UI sets a confirm flag (%s matched %q) — the host elicits the human, the UI must not answer for them", pattern, match)
		}
	}
}
