package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	authdomain "github.com/klarlabs-studio/auth-go/domain"

	"go.klarlabs.de/briefkasten"
)

// run dispatches the CLI. Empty args or "serve" starts the MCP server;
// everything else is a human command over the configured mailbox.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return serve(nil)
	}
	// "What is this binary?" is answered before any config is read or
	// mailbox built — the question matters most on a machine where
	// nothing is set up yet, which is exactly when someone is filing a
	// bug report.
	if isVersionRequest(args[0]) {
		printVersion(stdout, wantsJSON(args[1:]))
		return 0
	}
	if args[0] == "serve" {
		return serve(args[1:])
	}

	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	folder := fs.String("folder", "", "folder to operate on (see 'briefkasten folders')")
	scope := fs.String("scope", "unread", "which messages to list/search: unread, read, or all")
	account := fs.String("account", "", "named account from the config")
	asJSON := fs.Bool("json", false, "machine-readable output")
	curation := fs.Bool("curation", false, "with 'folders': show where archive and delete would file, and why")
	yes := fs.Bool("yes", false, "skip confirmation prompts")
	configPath := fs.String("config", "", "config file (default: $BRIEFKASTEN_CONFIG or ./briefkasten.yaml)")
	to := fs.String("to", "", "recipients, comma-separated (send)")
	subject := fs.String("subject", "", "subject (send)")
	body := fs.String("body", "", "body (send)")
	htmlBody := fs.String("html", "", "HTML alternative body (send)")
	var attach stringList
	fs.Var(&attach, "attach", "file to attach; repeatable (send)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	cfg, err := loadConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}

	// One context for the whole command, handed to every mailbox call:
	// the ports take one, and the CLI is not the place to invent a
	// deadline the operator did not ask for.
	ctx := commandContext()

	// The CLI calls the same application service the MCP tools call —
	// one use-case layer, two interfaces.
	svc, err := buildService(cfg)
	if err != nil && needsMailbox(cmd) {
		fmt.Fprintln(stderr, err)
		return 1
	}

	emit := func(human string, machine any) {
		if *asJSON {
			raw, _ := json.MarshalIndent(machine, "", "  ")
			fmt.Fprintln(stdout, string(raw))
			return
		}
		fmt.Fprintln(stdout, human)
	}

	switch cmd {
	case "list":
		ids, err := svc.List(ctx, *account, *folder, briefkasten.Scope(*scope))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(strings.Join(ids, "\n"), map[string]any{"ids": ids})

	case "read":
		ids := fs.Args()
		if len(ids) == 0 {
			fmt.Fprintln(stderr, "usage: briefkasten read <id> [id ...]")
			return 2
		}
		if len(ids) == 1 && !*asJSON {
			// The single-id form is what pipelines already parse: the
			// message, and nothing around it.
			raw, err := svc.Read(ctx, *account, *folder, ids[0])
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintln(stdout, string(raw))
			break
		}
		res, err := svc.ReadMany(ctx, *account, *folder, ids)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return reportFetch(stdout, stderr, *asJSON, res)

	case "seen":
		ids := fs.Args()
		if len(ids) == 0 {
			fmt.Fprintln(stderr, "usage: briefkasten seen <id> [id ...]")
			return 2
		}
		if len(ids) == 1 {
			if err := svc.MarkSeen(ctx, *account, *folder, ids[0]); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			emit("seen: "+ids[0], map[string]any{"ok": true, "id": ids[0]})
			break
		}
		res, err := svc.MarkSeenMany(ctx, *account, *folder, ids)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return reportBulk(stdout, stderr, *asJSON, "seen", res)

	case "search":
		query := fs.Arg(0)
		if query == "" {
			fmt.Fprintln(stderr, "usage: briefkasten search <query>")
			return 2
		}
		ids, err := svc.SearchScope(ctx, *account, *folder, query, briefkasten.Scope(*scope))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(strings.Join(ids, "\n"), map[string]any{"ids": ids})

	case "profiles":
		names := cfg.ProfileNames()
		if len(names) == 0 {
			fmt.Fprintln(stderr, "profiles: none declared (add a 'profiles:' block to the config file)")
			return 1
		}
		active := cfg.ResolvedBackend()
		emit(strings.Join(names, "\n"), map[string]any{"profiles": names, "active_backend": active})

	case "folders":
		// --curation answers "where would archive and delete file?"
		// before anything moves, so a wrong destination is caught by
		// reading rather than by finding mail somewhere unexpected.
		if *curation {
			plan, err := svc.CurationPlan(ctx, *account, *folder)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			emit(
				fmt.Sprintf("archive  %s  (%s)\ndelete   %s  (%s)",
					plan.Archive.Folder, plan.Archive.Route,
					plan.Trash.Folder, plan.Trash.Route),
				plan)
			break
		}
		folders, err := svc.Folders(ctx, *account)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(strings.Join(folders, "\n"), map[string]any{"folders": folders})

	case "send":
		ob, _, err := cfg.BuildClientOutbox()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if ob == nil {
			fmt.Fprintln(stderr, "send: no outbox configured (set outbox.dir)")
			return 1
		}
		recipients := splitList(*to)
		if len(recipients) == 0 || *subject == "" || *body == "" {
			fmt.Fprintln(stderr, "usage: briefkasten send --to a@b.c --subject S --body B [--html H] [--attach FILE ...]")
			return 2
		}
		attachments, err := loadAttachments(attach)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		id, err := ob.Enqueue(briefkasten.OutboundMessage{
			To:          recipients,
			Subject:     *subject,
			Body:        *body,
			HTMLBody:    *htmlBody,
			Attachments: attachments,
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		// Deliver immediately: the CLI has no background worker.
		if _, err := ob.ProcessOnce(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		msg, _ := ob.Status(id)
		emit(fmt.Sprintf("%s: %s", msg.State, id), map[string]any{"id": id, "state": msg.State})

	case "retry":
		id := fs.Arg(0)
		if id == "" {
			fmt.Fprintln(stderr, "usage: briefkasten retry <id>")
			return 2
		}
		ob, _, err := cfg.BuildClientOutbox()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if ob == nil {
			fmt.Fprintln(stderr, "retry: no outbox configured (set outbox.dir)")
			return 1
		}
		// A message stranded in sending by a dead server has to reach
		// failed before it can be re-queued, so retry repairs first.
		repairOutbox(ob, stderr)
		if err := ob.Retry(id); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, err := ob.ProcessOnce(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		msg, _ := ob.Status(id)
		human := fmt.Sprintf("%s: %s", msg.State, id)
		machine := map[string]any{"id": id, "state": msg.State}
		if msg.Error != "" {
			human += " (" + msg.Error + ")"
			machine["error"] = msg.Error
		}
		emit(human, machine)

	case "outbox":
		ob, _, err := cfg.BuildClientOutbox()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if ob == nil {
			fmt.Fprintln(stderr, "outbox: no outbox configured (set outbox.dir)")
			return 1
		}
		// Repair before reporting: a record listed as sending that no live
		// process is sending is a lie the operator would act on.
		repairOutbox(ob, stderr)
		summary, err := ob.Summary()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		var lines []string
		for _, state := range []string{"queued", "sending", "sent", "failed"} {
			for _, id := range summary[state] {
				lines = append(lines, state+": "+id)
			}
		}
		emit(strings.Join(lines, "\n"), summary)

	case "hashpw":
		// Read from stdin, not argv — a password argument would land in
		// shell history.
		fmt.Fprint(stdout, "password: ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			if err != nil && err != io.EOF {
				fmt.Fprintln(stderr, err)
			} else {
				fmt.Fprintln(stderr, "hashpw: empty password")
			}
			return 1
		}
		hash, err := authdomain.HashPassword(pw, authdomain.DefaultArgon2idParams())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(hash.String(), map[string]any{"password_hash": hash.String()})

	case "archive", "delete":
		ids := fs.Args()
		if len(ids) == 0 {
			fmt.Fprintf(stderr, "usage: briefkasten %s <id> [id ...]\n", cmd)
			return 2
		}
		// HITL stays at the interface; the shared use case runs after
		// the human said yes — exactly like the MCP elicitation gate.
		// One prompt covers the batch, so it has to state what the batch
		// is: how many messages and where they are about to go.
		if !*yes && !confirmPrompt(stdin, stdout, cmd, ids, curationDest(ctx, svc, *account, *folder, cmd)) {
			emit("aborted", map[string]any{"ok": false, "aborted": true})
			return 1
		}
		if len(ids) == 1 {
			op := svc.Archive
			if cmd == "delete" {
				op = svc.Delete
			}
			if err := op(ctx, *account, *folder, ids[0]); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			emit(cmd+"d: "+ids[0], map[string]any{"ok": true, "id": ids[0]})
			break
		}
		op := svc.ArchiveMany
		if cmd == "delete" {
			op = svc.DeleteMany
		}
		res, err := op(ctx, *account, *folder, ids)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return reportBulk(stdout, stderr, *asJSON, cmd+"d", res)

	default:
		fmt.Fprintf(stderr, `unknown command %q

usage: briefkasten [serve|list|read|seen|search|folders|profiles|send|retry|outbox|archive|delete|hashpw|version]

serve [--stdio] [--config FILE]   MCP server; --stdio serves over stdin/stdout
                                  instead of HTTP, for hosts that spawn it.

--version (or version) prints the build metadata; add --json for a
machine-readable form that also reports the toolchain and platform.

folders --curation shows where archive and delete would file and how each
destination was decided (override, declared, convention, alias, create) —
without moving anything.

list and search take --scope unread (default), read, or all. Listing and
reading never change a message's state.

read, seen, archive, and delete take an id from any scope: a message that
was processed long ago acts exactly like one still in the backlog. seen
is idempotent — re-acknowledging read mail is not an error.

read, seen, archive, and delete take several ids at once (max %d), and
report per id: what succeeded is listed, what did not is named with its
reason on stderr, and nothing is rolled back. A batch that partly failed
exits non-zero.

read with one id prints the message and nothing else, as it always has.
With several it length-prefixes each one — a header line "id <id>
<bytes>", then exactly that many raw bytes, then a newline — because no
marker line can safely delimit mail that may contain any byte sequence.
--json gives the structured form instead, with raw base64-encoded. A
batch of messages totalling over %d MiB is refused before anything is
read, rather than truncated.

Curation is soft: archive files away, delete moves to trash — nothing is
ever expunged. Both prompt once for confirmation unless --yes; for a
batch the prompt names the count and the destination folder.
`, cmd, briefkasten.MaxBulkIDs, briefkasten.MaxFetchBytes>>20)
		return 2
	}
	return 0
}

// repairOutbox recovers the outbox for the client commands that need a
// truthful picture of it. Recovery is deliberately not part of building a
// client outbox: `send` has no business repairing old mail, and the
// scenario that cost a duplicate delivery was exactly a send-time recovery
// filing a live server's in-flight message as failed.
//
// Recover claims the outbox without waiting, so a live owner is left
// strictly alone. Damage found on the way is reported and never blocks the
// command — one unreadable record must not make the others unreachable.
func repairOutbox(ob *briefkasten.Outbox, stderr io.Writer) {
	if err := ob.Recover(); err != nil && !errors.Is(err, briefkasten.ErrOutboxBusy) {
		fmt.Fprintln(stderr, err)
	}
}

// isVersionRequest recognises the three spellings people actually try.
// The bare `version` verb shipped first, so it keeps working; the flag
// forms exist because every other tool has them and reaching for
// `--version` should not return "unknown command".
func isVersionRequest(arg string) bool {
	switch arg {
	case "version", "--version", "-version":
		return true
	}
	return false
}

// wantsJSON reports whether a --json flag trails the version request.
// Version reporting runs before flag parsing — it must not depend on a
// flag set built around a mailbox command — so the scan is explicit.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "-json" {
			return true
		}
	}
	return false
}

// printVersion reports the build metadata goreleaser injects. The human
// line is byte-for-byte what the `version` verb has always printed, so
// anything parsing it keeps working; the JSON form adds the toolchain
// and platform, which is what a bug report actually needs.
func printVersion(stdout io.Writer, asJSON bool) {
	if asJSON {
		raw, _ := json.MarshalIndent(map[string]string{
			"version":  version,
			"commit":   commit,
			"date":     date,
			"go":       runtime.Version(),
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
		}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
		return
	}
	fmt.Fprintf(stdout, "briefkasten %s (commit: %s, built: %s)\n", version, commit, date)
}

// confirmPrompt asks the human; only an explicit y/yes proceeds. A batch
// is authorised by that single answer, so the question names the blast
// radius it covers: the count, the destination, and the ids themselves.
func confirmPrompt(stdin io.Reader, stdout io.Writer, action string, ids []string, destination string) bool {
	if len(ids) == 1 {
		fmt.Fprintf(stdout, "%s message %q? %s [y/N] ", action, ids[0], movedWhere(destination, false))
	} else {
		fmt.Fprintf(stdout, "%s %d messages (%s)? %s [y/N] ",
			action, len(ids), strings.Join(ids, ", "), movedWhere(destination, true))
	}
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// movedWhere phrases the reassurance the prompt ends on, naming the
// destination when the backend could report one.
func movedWhere(destination string, plural bool) string {
	subject, verb := "The message is", "It will"
	if plural {
		subject, verb = "The messages are", "They will"
	}
	if destination == "" {
		return subject + " moved, never destroyed."
	}
	return fmt.Sprintf("%s be filed into %q — moved, never destroyed.", verb, destination)
}

// curationDest reports where this curation would file, for the prompt.
// Best-effort: a backend that cannot answer must not block the command,
// and the prompt simply falls back to the destination-less wording.
func curationDest(ctx context.Context, svc *briefkasten.Service, account, folder, action string) string {
	plan, err := svc.CurationPlan(ctx, account, folder)
	if err != nil {
		return ""
	}
	if action == "archive" {
		return plan.Archive.Folder
	}
	return plan.Trash.Folder
}

// reportBulk prints a per-id outcome and sets the exit status from it. A
// batch that partly failed exits non-zero: the successes are named, but
// a script must not read "some of it worked" as success.
func reportBulk(stdout, stderr io.Writer, asJSON bool, verb string, res briefkasten.BulkResult) int {
	if asJSON {
		raw, _ := json.MarshalIndent(map[string]any{
			verb:     res.Succeeded,
			"failed": res.Failed,
			"total":  len(res.Succeeded) + len(res.Failed),
		}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
	} else {
		for _, id := range res.Succeeded {
			fmt.Fprintf(stdout, "%s: %s\n", verb, id)
		}
		for _, f := range res.Failed {
			fmt.Fprintf(stderr, "failed: %s (%v)\n", f.ID, f.Err)
		}
	}
	if len(res.Failed) > 0 {
		return 1
	}
	return 0
}

// reportFetch prints a batch of raw messages and sets the exit status
// from the per-id outcome, exactly as reportBulk does.
//
// The plain-text form is length-prefixed, because raw RFC 5322 messages
// cannot be separated by a marker line: any delimiter that could appear
// in a header or a MIME body — and every printable one can — would need
// escaping, and escaping mail is how a consumer ends up parsing
// something that is no longer the message. So each message is announced
// by a header line
//
//	id <id> <bytes>
//
// followed by exactly that many bytes and a newline. A reader takes the
// header, reads the count, and consumes the message byte for byte with
// nothing to unescape. --json gives the same information as one object,
// with raw base64-encoded.
func reportFetch(stdout, stderr io.Writer, asJSON bool, res briefkasten.FetchResult) int {
	if asJSON {
		raw, _ := json.MarshalIndent(map[string]any{
			"fetched": res.Fetched,
			"failed":  res.Failed,
			"total":   len(res.Fetched) + len(res.Failed),
		}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
	} else {
		for _, msg := range res.Fetched {
			fmt.Fprintf(stdout, "id %s %d\n", msg.ID, len(msg.Raw))
			_, _ = stdout.Write(msg.Raw)
			fmt.Fprintln(stdout)
		}
		for _, f := range res.Failed {
			fmt.Fprintf(stderr, "failed: %s (%v)\n", f.ID, f.Err)
		}
	}
	if len(res.Failed) > 0 {
		return 1
	}
	return 0
}

func loadConfigPath(explicit string) (*briefkasten.Config, error) {
	path := explicit
	if path == "" {
		path = os.Getenv("BRIEFKASTEN_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat("briefkasten.yaml"); err == nil {
			path = "briefkasten.yaml"
		}
	}
	cfg, err := briefkasten.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv()
	return cfg, nil
}

func needsMailbox(cmd string) bool {
	switch cmd {
	case "send", "retry", "outbox", "hashpw", "profiles":
		return false
	default:
		return true
	}
}

// splitList parses a comma-separated list, trimming blanks.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// stringList is a repeatable string flag (e.g. --attach a --attach b).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// loadAttachments reads each file path into an Attachment, inferring the
// content type from the extension (falling back to content sniffing).
func loadAttachments(paths []string) ([]briefkasten.Attachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	atts := make([]briefkasten.Attachment, 0, len(paths))
	for _, p := range paths {
		content, err := os.ReadFile(p) // #nosec G304 -- attachment path is supplied by the operator running the CLI
		if err != nil {
			return nil, fmt.Errorf("attach %q: %w", p, err)
		}
		ctype := mime.TypeByExtension(filepath.Ext(p))
		if ctype == "" {
			ctype = http.DetectContentType(content)
		}
		atts = append(atts, briefkasten.Attachment{
			Filename:    filepath.Base(p),
			ContentType: ctype,
			Content:     content,
		})
	}
	return atts, nil
}

// buildService composes the shared application service from the config —
// the identical wiring NewConfigServer uses for the MCP surface.
func buildService(cfg *briefkasten.Config) (*briefkasten.Service, error) {
	box, _, err := cfg.BuildMailbox()
	if err != nil {
		return nil, err
	}
	accounts, err := cfg.BuildAccounts()
	if err != nil {
		return nil, err
	}
	return briefkasten.NewService(box, accounts), nil
}
