# AGENTS.md — last updated: 2026-07-25
# Keep under 400 lines. Split overflow to memory/ files.

## Working Style
Output format: prose with structure — headings and tables where they earn their
place, not bulleted fragments. Lead with the finding, not the process.
Decision style: recommend directly, with the reasoning visible so it can be
argued with. Use AskUserQuestion when the options lead to materially different
work (e.g. patch-the-primitive vs replace-it).
When stuck: make the call and flag it. Do not stall on approval for
non-destructive steps.
Review mode: critique hard. Verify claims against the code before repeating them,
including claims from subagents and from memory.

## Project Context
Company: klarlabs — open-source Go tooling.
What we're building: briefkasten, a mailbox served over MCP so agents can read,
search, curate, and send mail through one contract instead of binding to IMAP.
Phase: maintenance / hardening. Released and versioned; v0.23.0 current.
Stack: Go, hexagonal architecture, go-imap v2, go.klarlabs.de/mcp, goreleaser,
warden commit gate, coverctl coverage gates.

## Constraints
# Testable formulations only.
Never: tag or merge without first running `gh pr list` and
`gh run list --workflow release.yml`. Both have failed silently in this repo.
Never: push to a scratch or fork remote to "test" anything — warden's pre-push
hook ignores the named remote and pushes to `origin`.
Never: add a capability to `domain/` without also forwarding it in
`application.Switchable` and `infrastructure/resilience.Mailbox`, or it vanishes
behind a decorator.
Never: let a backend silently substitute unread mail when a wider scope is
unsupported — error instead.
Never: let read state gate an action. `scope` filters what a listing returns; it
never decides what an id may be used for. Fetch, mark-seen, archive, and delete
all resolve an id across the whole mailbox.
Never: report a curation success a backend did not perform — IMAP answers OK to
COPY of a UID it does not hold, so verify presence before claiming a move.
Never: hardcode an IMAP folder name. Servers root folders differently
(`INBOX.Trash` vs `Trash`) and declare SPECIAL-USE inconsistently; resolve via
override → SPECIAL-USE → namespace path, and create only inside the namespace.
Always: gate a new mutating MCP tool through `mcpserver.ConfirmAction` and mark
it `Destructive()`. Message content reaches every tool.
Always: assert security fixes against observable output (bytes on disk, the tool
response), not against the struct in memory.
Always: run `go test -race ./...` and `golangci-lint run ./...` before pushing —
warden's pre-push gate runs exactly these, so failing locally is faster.

## Known Failure Modes
# Session note → pattern → constraint.
- Tends to treat "approved" as authorising the whole chain → merged and tagged
  v0.19.0 in one uninterrupted run, shipping binaries containing a known XSS that
  had a one-line fix sitting in an open PR. Correct by re-checking repo state
  between merge and publish; approval of a step is not approval of the sequence.
- Tends to assume a tool respects its arguments → tested warden's pre-push hook
  against a `/tmp` bare repo believing it was sandboxed; the hook discarded the
  remote and pushed to GitHub. Correct by reading what a hook does before
  triggering it, especially when the thing under test is itself a push.
- Tends to report a bug from a single observation → nearly filed a warden
  exit-code bug that was an artifact of piping through `head`. Correct by
  re-running the check in isolation before attributing a fault.
- Tends to branch off a stale main → merge blocked as `BEHIND` (branch protection
  requires up-to-date branches). Correct by branching off fresh `origin/main`, or
  rebase + force-push before merge. Note the provenance cost: updating the branch
  after the local gate ran means `warden reattest` can no longer restore the note.
- Tends to trust recalled memory over the repo → stored notes claimed briefkasten
  did not use warden while `.warden.yaml` sat in the root. Correct by verifying
  memory claims against current files before acting on them.

## Decision Summary
# 3–5 most consequential decisions. Full log in memory/decisions.md
- 2026-07-19: Gate `email.send` behind confirmation — gating was inverted relative
  to impact (reversible curation gated, irreversible send not).
- 2026-07-19: Bind credentials to their endpoint in `config.set` — the root cause
  was partial-update inheritance, not the TLS flag; fixing `insecure` alone left
  redirection to any valid-cert host.
- 2026-07-19: Add profiles rather than loosen maildir confinement — trust comes
  from who wrote the destination, not from where it points.
- 2026-07-19: Scope defaults to `unread` everywhere — the feature is additive, so
  no current caller changes behaviour.
- 2026-07-19: Track secret provenance at the source rather than comparing values
  at save time — precision is what stops the fix deleting real configuration.
- 2026-07-25: Read state filters, it does not gate — every action resolves an id
  across the whole mailbox, so the only difference between curating fresh and
  processed mail is which listing surfaced it. The confirmation gate is what
  restrains destructive work, not the read flag.
- 2026-07-25: Discover curation folders rather than naming them. Hardcoded
  `Archive`/`Trash` had never worked on an `INBOX.`-rooted server; asking the
  server (SPECIAL-USE, then NAMESPACE) fixes the whole class, and the config
  override exists for what neither answers.

## Active Patterns
- "brief me" → /brief (reads ./memory/status.md)
- "capture" → /capture (writes session log, updates status)
- "/mem-compact" → digest sessions older than 30 days
