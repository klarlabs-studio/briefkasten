---
updated: 2026-07-19
---
## Now
- Decide on `checkMaildirConfinement` scope: strict startup-subtree (current) vs
  parent-directory. Strict blocks legitimate sibling-maildir switches.

## Next
- Harden the remaining low-severity review findings:
  - `postMessage` bridge in `ui/inbox.html` validates neither `ev.origin` nor
    `ev.source`, and posts with `targetOrigin: '*'`.
  - Absolute filesystem paths leak to MCP clients via unwrapped `*PathError`
    (`maildir.go`, `outboxstore.go`) — wrap with `ErrBadID` on `ErrNotExist`.
  - Prompt delimiters (`--- Message %s ---`) are forgeable plain text; a
    per-invocation nonce would harden the injection boundary.
- `email.list` is annotated `ReadOnly()` but creates directories via `MkdirAll`
  from the `folder` argument — the false annotation is the real issue, since
  hosts use it to skip approval.
- Unbounded message reads: no inbound analogue of the outbound `MaxMessageBytes`
  guard; `SearchScope` fetches every message in scope and lowercases a full copy.

## Later
- Consider whether `config.set`'s field-level patch arm should exist at all now
  that profiles cover the safe cases. Removing it would delete a whole class of
  guard logic (credential binding, TLS one-way, maildir confinement).
- Standardise the Homebrew tap secret name across klarlabs repos
  (`HOMEBREW_TAP_GITHUB_TOKEN` vs `HOMEBREW_TAP_TOKEN`) and consider promoting it
  to an org-level secret so rotation is one update, not five.

## Done
- v0.19.0 — scoped mail listing (`unread` / `read` / `all`) across domain, both
  backends, service, decorators, MCP tools, CLI.
- v0.20.0 — DOM XSS fix (external, @heiderich); `email.send` confirmation gate;
  attachment `content_type` MIME-injection fix; `config.set` credential binding,
  TLS one-way, maildir confinement; mailbox profiles.
- v0.20.1 — `config.Save` no longer persists env-sourced secrets or OAuth2 values
  hydrated from a credentials file.
