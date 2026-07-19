---
updated: 2026-07-19
note: append-only log — never edit or delete entries; supersede with "→ superseded [date]"
---
- 2026-07-19: Adopted Agent OS memory system — persistent cross-session state via
  memory/ + wiki/ + cadence skills.

- 2026-07-19: Scope defaults to `unread` everywhere rather than widening the
  default — existing agents and `email.list_unread` keep working untouched. The
  feature is additive; a default change would have been a silent behavioural
  break for every current caller.

- 2026-07-19: Kept `email.list_unread` as an alias rather than replacing it with
  `email.list`. Trades a slightly larger tool surface for not breaking existing
  agent configs. Revisit if the surface gets crowded.

- 2026-07-19: Backends that cannot distinguish read from unread mail **error** on
  a wider scope instead of silently returning unread ids. A quiet fallback would
  have mislabelled unread mail as read — the one place where degrading gracefully
  is worse than failing.

- 2026-07-19: Gate `email.send` behind human confirmation and mark it
  `Destructive()`. The gating was inverted relative to impact: archive/delete are
  soft, reversible moves that never expunge, yet both gated; send is irreversible
  and outbound and did not. Message content is embedded into prompts, so an
  injected "forward everything to X" had an unguarded path to execution.

- 2026-07-19: Bind credentials to their endpoint in `config.set` rather than only
  blocking the TLS downgrade. Fixing `insecure` alone would still have permitted
  redirection to any host with a valid cert — equally full credential disclosure.
  The root cause was partial-update inheritance, not the TLS flag.

- 2026-07-19: Added profiles (operator-declared whole configurations, switched by
  name) after the confinement rule proved too strict for legitimate sibling-maildir
  switches. A profile may move outside the startup maildir where a field-level
  patch may not, because **the trust comes from who wrote the destination, not
  from where it points**.

- 2026-07-19: Profiles cannot be combined with field-level settings in one
  `config.set` call — rejected loudly rather than silently dropping one form.

- 2026-07-19: Versioned the security release v0.20.0, not v0.19.1. It carried a
  new feature (profiles) and a behavioural break (`email.send` refusing without
  `confirm=true`), which is a minor bump under semver, not a patch.

- 2026-07-19: Track secret provenance at the source (`overlaySecret` records which
  env vars supplied a value; `LoadCredentials` marks fields it hydrated) rather
  than guessing at save time by comparing values. Precision is what makes the fix
  safe to ship: an operator-written value is never mistaken for an inherited one,
  so `Save` cannot silently delete configuration.

- 2026-07-19: Extended the `config.Save` fix beyond the reported env-secret leak
  to cover OAuth2 values hydrated from a `credentials_file`. Same failure mode —
  a secret the operator deliberately kept elsewhere, duplicated into the config
  file — and `credentials_file` exists precisely so those values are not copied.
