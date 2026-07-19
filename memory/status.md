---
updated: 2026-07-19
---
## Current State
Briefkasten is at v0.20.1, released and clean — GitHub release published, Homebrew
cask current, release commit carrying warden provenance. Every finding from
today's security review is closed and shipped. The MCP surface now gates all
three mutating tools (`email.send`, `email.archive`, `email.delete`) plus
`config.set` behind human confirmation, credentials are bound to their endpoint,
and secrets kept in the environment or an OAuth2 credentials file are no longer
copied into the config file. Working tree clean, main synced, no open PRs.

## Last Session Summary
Started with "read read mails too" → shipped scoped listing (v0.19.0), then a
security review found four more issues → shipped XSS fix, `email.send` gate, MIME
injection fix, and the `config.set` credential-redirection fix plus profiles
(v0.20.0), then the config-save secrets fix (v0.20.1). v0.19.0 went out carrying a
known XSS because open PRs were never checked before tagging.

## Next Session Should
Decide whether to relax `checkMaildirConfinement` (runtime.go) from strict
startup-subtree to parent-directory scope. Strict confinement currently refuses
legitimate sibling switches like `/var/mail/alice` → `/var/mail/bob`; profiles are
the sanctioned workaround, but if that proves awkward in practice the parent-dir
rule still blocks `/etc` while permitting siblings.

## Blocked / Waiting
- Warden's pre-push remote-override bug reported upstream, unfixed — pushing to
  any remote still pushes to `origin`. Do not test the gate against a scratch
  remote; it is not a sandbox.
- `felixgeelhaar/roady` has a `HOMEBREW_TAP_TOKEN` secret but no release
  workflow — likely orphaned, needs a decision.
