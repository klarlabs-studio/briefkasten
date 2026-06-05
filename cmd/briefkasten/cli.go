package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/felixgeelhaar/briefkasten"
)

// run dispatches the CLI. Empty args or "serve" starts the MCP server;
// everything else is a human command over the configured mailbox.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "serve" {
		return serve()
	}

	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	folder := fs.String("folder", "", "folder to operate on (see 'briefkasten folders')")
	account := fs.String("account", "", "named account from the config")
	asJSON := fs.Bool("json", false, "machine-readable output")
	yes := fs.Bool("yes", false, "skip confirmation prompts")
	configPath := fs.String("config", "", "config file (default: $BRIEFKASTEN_CONFIG or ./briefkasten.yaml)")
	to := fs.String("to", "", "recipients, comma-separated (send)")
	subject := fs.String("subject", "", "subject (send)")
	body := fs.String("body", "", "body (send)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	cfg, err := loadConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}

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
		ids, err := svc.ListUnread(*account, *folder)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(strings.Join(ids, "\n"), map[string]any{"ids": ids})

	case "read":
		id := fs.Arg(0)
		if id == "" {
			fmt.Fprintln(stderr, "usage: briefkasten read <id>")
			return 2
		}
		raw, err := svc.Read(*account, *folder, id)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, string(raw))

	case "seen":
		id := fs.Arg(0)
		if id == "" {
			fmt.Fprintln(stderr, "usage: briefkasten seen <id>")
			return 2
		}
		if err := svc.MarkSeen(*account, *folder, id); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit("seen: "+id, map[string]any{"ok": true, "id": id})

	case "search":
		query := fs.Arg(0)
		if query == "" {
			fmt.Fprintln(stderr, "usage: briefkasten search <query>")
			return 2
		}
		ids, err := svc.Search(*account, *folder, query)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(strings.Join(ids, "\n"), map[string]any{"ids": ids})

	case "folders":
		folders, err := svc.Folders(*account)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(strings.Join(folders, "\n"), map[string]any{"folders": folders})

	case "send":
		ob, _, err := cfg.BuildOutbox()
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
			fmt.Fprintln(stderr, "usage: briefkasten send --to a@b.c --subject S --body B")
			return 2
		}
		id, err := ob.Enqueue(briefkasten.OutboundMessage{To: recipients, Subject: *subject, Body: *body})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		// Deliver immediately: the CLI has no background worker.
		if _, err := ob.ProcessOnce(contextTODO()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		msg, _ := ob.Status(id)
		emit(fmt.Sprintf("%s: %s", msg.State, id), map[string]any{"id": id, "state": msg.State})

	case "archive", "delete":
		id := fs.Arg(0)
		if id == "" {
			fmt.Fprintf(stderr, "usage: briefkasten %s <id>\n", cmd)
			return 2
		}
		// HITL stays at the interface; the shared use case runs after
		// the human said yes — exactly like the MCP elicitation gate.
		if !*yes && !confirmPrompt(stdin, stdout, cmd, id) {
			emit("aborted", map[string]any{"ok": false, "aborted": true})
			return 1
		}
		op := svc.Archive
		if cmd == "delete" {
			op = svc.Delete
		}
		if err := op(*account, *folder, id); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		emit(cmd+"d: "+id, map[string]any{"ok": true, "id": id})

	default:
		fmt.Fprintf(stderr, `unknown command %q

usage: briefkasten [serve|list|read|seen|search|folders|send|archive|delete]

Curation is soft: archive files away, delete moves to trash — nothing is
ever expunged. Both prompt for confirmation unless --yes.
`, cmd)
		return 2
	}
	return 0
}

// confirmPrompt asks the human; only an explicit y/yes proceeds.
func confirmPrompt(stdin io.Reader, stdout io.Writer, action, id string) bool {
	fmt.Fprintf(stdout, "%s message %q? The message is moved, never destroyed. [y/N] ", action, id)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
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
	case "send":
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
