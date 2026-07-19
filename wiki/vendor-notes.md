---
updated: 2026-07-19
tags: [vendors, reference]
---
# Vendor and tooling notes

## warden (klarlabs-studio/warden) — commit/push gate
`.warden.yaml`: `lint` pre-commit, `test` (`go test -race ./...`) + `lint`
pre-push. CI job `provenance.yml` runs `warden-verify` against the PR head.

- **Notes are written at push, not commit.** Pre-commit has no SHA to attach one
  to, so a commit made with hooks armed still reads `unverified` until pushed.
  `warden why` misreports this as "made outside the gate or predates adoption" —
  both false. Do not amend or re-init chasing it.
- **The pre-push hook ignores the remote you name and pushes to `origin`.** This
  published a branch to GitHub during testing on 2026-07-19. Never test the gate
  by pushing to a throwaway remote; it is not a sandbox. Also breaks fork-and-PR.
- **Squash-merges lose the note.** `warden reattest` recovers it from the
  tree-identical validated commit — but only if one exists. If the PR branch was
  updated with main before merging (strict branch protection requires this), the
  squash tree matches nothing gated locally and warden correctly refuses.
  Confirmed both ways 2026-07-19: v0.20.0 refused, v0.20.1 succeeded.
- Notes push separately: `git push --no-verify origin refs/notes/warden`
  (`--no-verify` avoids the remote-override bug for a metadata-only ref).

## goreleaser + Homebrew tap
Tag `v*` → `release.yml` → goreleaser. Briefkasten publishes a **cask**; the
tap's other entries (coverctl, mnemos, scout) are **formulas**.

- The tap step is last. A `401 Bad credentials` there marks the whole run failed
  while the GitHub release has already published successfully — deceptive, and it
  hid a stale tap across v0.18.0 and v0.19.0. Check
  `gh run list --workflow release.yml` before tagging.
- `HOMEBREW_TAP_GITHUB_TOKEN` is **repo-level**, not org-level (only `NOX_TOKEN`
  is org-level). Sibling repos use two names: `HOMEBREW_TAP_GITHUB_TOKEN`
  (briefkasten, nomi, scout) vs `HOMEBREW_TAP_TOKEN` (mnemos, roady).
- `git tag | tail` lies about the latest version — v0.10+ sorts before v0.6
  lexically. Use `git tag -l | sort -V` or `gh release list`.

## go-imap v2 (emersion)
The encoder rejects CR/LF/NUL in quoted strings and falls back to a
length-prefixed literal, so folder names and search queries cannot break out of
an IMAP command. Command injection is defended at the library boundary.

## mcp-go (go.klarlabs.de/mcp)
- HTTP transport does **not** forward headers automatically. MCP-layer auth reads
  `protocol.GetRequestMeta(ctx, "Authorization")`, which needs
  `transport.WithRequestContextFn` — briefkasten exposes
  `mcpserver.ForwardAuthorizationHeader`. Pair BasicAuth with it or you get a
  silent 401 despite correct credentials.
- Input schemas auto-generate from handler structs via
  `jsonschema:"required,description=…"` tags. The tag parser splits on comma, so
  **descriptions must be comma-free**.

## auth-go (klarlabs-studio/auth-go)
Web-app auth (WebAuthn/passkeys, magic link, TOTP, sessions, pgx) — **not** mail
OAuth2. Briefkasten's `infrastructure/auth` (XOAUTH2/OAUTHBEARER SASL) is a
different domain. Only `domain.PasswordHash` (argon2id) is reused, for MCP
endpoint basic auth.

## coverctl
Manages `coverage.svg` and history; CI gates per-domain thresholds.
