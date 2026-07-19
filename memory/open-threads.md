---
updated: 2026-07-19
---
## [OPEN]
- Maildir confinement scope. `checkMaildirConfinement` (runtime.go) restricts
  runtime maildir changes to the startup subtree, which refuses legitimate
  sibling switches (`/var/mail/alice` → `/var/mail/bob`). Profiles are the
  sanctioned path. Relaxing to parent-directory scope would permit siblings while
  still blocking `/etc`. Undecided.
- Low-severity review findings not yet addressed: `postMessage` origin/source
  validation, `*PathError` path disclosure to MCP clients, forgeable prompt
  delimiters, `email.list` annotated `ReadOnly()` while creating directories,
  unbounded inbound message reads.
- Homebrew tap secret naming differs across klarlabs repos
  (`HOMEBREW_TAP_GITHUB_TOKEN` on briefkasten/nomi/scout vs `HOMEBREW_TAP_TOKEN`
  on mnemos/roady; mnemos bridges them in its workflow). Secret is repo-level in
  five places, not org-level — rotation means five updates.

## [BLOCKED]
- Warden pre-push ignores the remote you name and pushes to `origin`. Reported to
  the maintainer as feedback; unfixed upstream. Consequence: never test the gate
  against a scratch remote, it is not a sandbox. Also breaks fork-and-PR
  workflows.
- Warden `reattest` cannot restore provenance when the PR branch had to be updated
  with main before merging — the squash tree then matches no locally gated commit
  and warden correctly refuses. Confirmed both ways today: v0.20.0 refused (branch
  was updated), v0.20.1 succeeded (main had not moved). No workaround short of
  gating after the branch is current with main.

## [WAITING]
- `felixgeelhaar/roady` holds a `HOMEBREW_TAP_TOKEN` secret but has no release
  workflow (only ci, nox-remediate, security, website). Appears orphaned — needs
  a decision on whether to delete it.
