---
updated: 2026-07-25
tags: [architecture]
---
# Briefkasten architecture

Hexagonal, with the dependency arrow pointing inward at `domain/`.

## Layers

- **`domain/`** — ports and invariants. Imports no infrastructure. `Mailbox` is
  the core port (`ListUnread`, `Fetch`, `MarkSeen`); optional capabilities are
  separate interfaces a backend may implement: `ScopedMailbox`, `Searcher`,
  `ScopedSearcher`, `FolderMailbox`, `Curator`. Outbound lives here too
  (`OutboundMessage`, `Sender`, validation).
- **`application/`** — use cases. The single code path every interface shares:
  the MCP tools and the CLI both call `Service`. Confirmation of destructive
  operations is an *interface* concern; the use cases run after approval.
- **`infrastructure/`** — adapters: `maildir` (local-first), `imap` (go-imap v2),
  `smtp`, `auth` (SASL XOAUTH2/OAUTHBEARER + basic auth), `resilience` (fortify
  timeout/retry/circuit-breaker), `mcpserver` (MCP presentation + embedded Apps UI).
- **root package** — composition: `Config`, `NewConfigServer`, runtime
  reconfiguration tools, re-exported domain types for consumers.

## Load-bearing invariants

- **Nothing is ever destroyed.** Archive and delete are soft moves. IMAP uses
  COPY + `\Seen`, deliberately not MOVE, because MOVE expunges the source. It
  also verifies the UID is present first: servers answer OK to COPY of a UID
  they do not hold, which would report a move that never happened.
- **The destination is asked for, never assumed.** Curation folders resolve
  through config override → RFC 6154 SPECIAL-USE → the personal namespace's
  conventional path → a known localized/legacy name. A mailbox rooted at
  `INBOX.` keeps its trash at `INBOX.Trash`, and such servers routinely
  declare `\Trash` while staying silent about `\Archive` — so the two targets
  often resolve by different routes on one server. The alias step ranks last
  because a long-lived mailbox can hold `Trash`, `Deleted Messages`, and
  `Papierkorb` at once; it exists to avoid creating a fourth, not to guess
  among three. The decision is a pure function (`chooseCurationFolder`)
  precisely because the layouts that matter are the ones an in-memory test
  server cannot reproduce.
- **A destination is knowable before it is used.** `domain.CurationInspector`
  reports where curation would file and by which route, without moving
  anything — surfaced as `folders --curation`, on `email://folders`, and in
  the confirmation prompt. Approving a soft move means little without knowing
  where it goes.
- **Reading never mutates.** IMAP fetches `BODY.PEEK[]`; the maildir backend
  reads files in place. This is what makes `scope=read`/`all` safe — looking at
  processed mail cannot disturb the unread backlog.
- **Read state filters, it does not gate.** `scope` narrows what a listing
  returns; it never decides what an id may be used for. `Fetch`, `MarkSeen`, and
  both `Curator` operations resolve an id across the whole mailbox, so an id from
  a `read` or `all` listing acts exactly like one from the backlog. `MarkSeen` is
  idempotent in service of this — re-acknowledging read mail is a no-op success,
  and only an id in no state at all is an `ErrBadID`.
- **Capabilities degrade loudly, not silently.** A backend that cannot tell read
  from unread errors on a wider scope rather than returning unread ids. See
  `listMailbox` / `searchMailbox` in `application/service.go`.
- **Decorators forward capabilities.** `application.Switchable` (runtime backend
  swap) and `infrastructure/resilience.Mailbox` both re-implement every optional
  interface. Adding a domain capability means updating both, or it silently
  disappears behind a decorator.

## Security model

Message content is untrusted input. It is embedded verbatim into the
`summarize_inbox` and `draft_reply` prompts, so anything reachable from a tool is
reachable from a crafted email.

- Every mutating tool gates on human confirmation via
  `mcpserver.ConfirmAction` — MCP elicitation, or an explicit `confirm=true`.
  That is `email.send`, `email.archive`, `email.delete`, and `config.set`.
  `ConfirmAction` is exported so the root package's runtime tools use the same
  gate rather than a parallel one.
- **Credentials are bound to their endpoint.** `config.set` is a partial update;
  changing `addr` clears inherited credentials. Otherwise a caller who does not
  know the password could choose where it is sent.
- **TLS is one-way at runtime** — `insecure` can be turned off, never on.
- **Profiles vs field-level patches.** A profile may point anywhere; a field-level
  patch is confined to the startup maildir subtree. The asymmetry is deliberate:
  trust comes from *who wrote the destination*, not from where it points.
- **Secrets stay at their source.** `Config.forSave` elides env-sourced passwords
  and OAuth2 values hydrated from a `credentials_file`, tracked at the point they
  were read rather than guessed at save time.

## Interfaces

Two surfaces, one use-case layer: MCP (`infrastructure/mcpserver`) and CLI
(`cmd/briefkasten`). Transport is HTTP or stdio. The MCP Apps UI
(`ui://briefkasten/inbox`) is an embedded self-contained HTML page served as a
resource — all dynamic values reach the DOM through `textContent`. It carries an
unread/read/all selector and per-message archive/delete, and deliberately sends
no `confirm` flag: the host elicits the human, so the UI never answers the gate
on their behalf. Its bridge allows a longer timeout for those calls, because a
person has to answer.
