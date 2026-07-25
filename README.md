# briefkasten

**A mailbox as an MCP server.**

Briefkasten (German: *letterbox*) exposes any message store through three
[Model Context Protocol](https://modelcontextprotocol.io) tools, so agent
runtimes and ingestion pipelines pull mail through a stable, language-agnostic
contract instead of binding to IMAP libraries:

| Tool | Does |
|---|---|
| `email.list` | `{"scope?": "unread\|read\|all", "limit?"}` → `{"ids": ["..."], "total": N, "scope": "unread"}` — `scope` defaults to `unread` |
| `email.list_unread` | `{"limit?"}` → `{"ids": ["..."], "total": N}` — `email.list` with `scope=unread` |
| `email.fetch` | `{"id": "..."}` → `{"raw": "<base64 RFC 5322>"}` — read or unread; never sets `\Seen` |
| `email.mark_seen` | `{"id"}` → `{"ok": true}`, or `{"ids": [...]}` → `{"marked": [...], "failed": [...], "total": N}` — message won't be listed again; idempotent, so re-acknowledging read mail is not an error |
| `email.send`* | `{"to": [...], "subject", "body", "html_body?", "attachments?": [{"filename", "content_type", "content": "<base64>"}]}` → `{"id", "state": "queued"}` — attachments ≤ 10 MiB each, ≤ 25 MiB per message |
| `email.send_status`* | `{"id"}` → `{"state": "queued\|sending\|sent\|failed", "attempts", "error?"}` |
| `email.retry`* | `{"id"}` → `{"id", "state": "queued"}` — re-queue a failed send |
| `email.search` | `{"query", "scope?", "folder?", "account?", "limit?"}` → `{"ids": [...], "total": N, "scope": "unread"}` — case-insensitive; IMAP searches server-side |
| `email.archive` | `{"id" \| "ids", "confirm?"}` → `{"ok": true}` / `{"archived": [...], "failed": [...], "total": N}` — **human-confirmed** (elicitation or confirm flag); soft: filed to Archive, never destroyed; read or unread |
| `email.delete` | `{"id" \| "ids", "confirm?"}` → `{"ok": true}` / `{"deleted": [...], "failed": [...], "total": N}` — **human-confirmed**; soft delete to Trash, never expunged; read or unread |

Every mailbox tool — `email.list`, `email.list_unread`, `email.fetch`,
`email.mark_seen`, `email.search`, `email.archive`, and `email.delete` —
accepts optional `folder` (see `email://folders`) and `account` (see
`email://accounts`) arguments. `limit` caps the ids returned; `total`
always reports the full count.

### Bulk: many ids, one call

`email.mark_seen`, `email.archive`, and `email.delete` take either `id`
(one message) or `ids` (a batch of up to **100**) — exactly one of the
two; both, or neither, is refused. On IMAP a batch costs what one
message costs: the curation folder is resolved once, and one `UID FETCH`,
one `UID COPY` and one `UID STORE` carry the whole set. Fifty deletes go
from **301 commands to 7**. Backends that gain nothing from batching
(the dir backend: local renames, no round trips) loop through a shared
fallback and behave identically to the caller.

Three properties are deliberate:

- **Explicit ids only.** There is no predicate or "all matching" form.
  Message content reaches every tool, so a bulk-by-query delete would let
  one injected sentence in an email body destroy an unbounded amount of
  mail. The caller enumerates with `email.list`/`email.search` and passes
  the list it can be held to.
- **Per-id results, never a single `ok`.** A batch is not a transaction
  and nothing is rolled back, so the response names what moved and what
  did not, with a reason per id: `{"deleted": ["a"], "failed": [{"id":
  "b", "error": "…invalid message id: b"}], "total": 2}`. A single `id`
  keeps the old `{"ok": true}`.
- **One confirmation, stating the blast radius.** The elicitation prompt
  names the count, the resolved destination folder and the ids —
  `Confirm delete of 3 messages? They will be filed into "INBOX.Trash" —
  moved, never destroyed. Messages: a.eml, b.eml, c.eml.`

### Scope: read mail, not just unread

`email.list` and `email.search` take a `scope`:

| `scope` | Covers |
|---|---|
| `unread` (default) | the ingest backlog — messages not yet marked seen |
| `read` | messages already marked seen |
| `all` | the whole mailbox |

Omitting `scope` means `unread`, so existing agents are unaffected.
Widening it is read-only in every sense: listing and fetching never set
or clear `\Seen` (IMAP fetches with `BODY.PEEK[]`, the dir backend reads
the file in place), so looking back at processed mail cannot disturb the
backlog. A backend that cannot tell read from unread mail rejects a
wider scope with an error rather than silently returning unread ids.

An id carries no read state with it, so **every action applies to every
scope**: an id from a `read` or `all` listing can be fetched, marked
seen, archived, or deleted exactly like one from the backlog — no
re-listing, no unread-only privilege.

| Action on a read message | Behaviour |
|---|---|
| `email.fetch` | serves it; never sets `\Seen` |
| `email.mark_seen` | succeeds and changes nothing (idempotent) |
| `email.archive` / `email.delete` | soft-moves it, human-confirmed like any curation |

An unknown id is rejected as a bad id, not silently accepted — so an
`{"ok": true}` from `archive` or `delete` means a message actually
moved.

\* Sending registers only when an outbox is configured.

Beyond tools, the full MCP surface:

| Surface | What |
|---|---|
| Resources | `email://inbox`, `email://inbox/{id}` (raw RFC 5322), `email://inbox/{id}/headers` (parsed from/to/subject/date/message_id — triage without fetching the body), `email://outbox`, `email://outbox/{id}`, `email://folders` (folders plus the curation destinations and how each was decided), `email://accounts` — read state without spending tool calls; `{id}` serves and completes read and unread ids alike |
| Prompts | `summarize_inbox(count?)` (embeds up to `count` unread messages, default 20, each truncated at 16 KiB), `draft_reply(id)` (embeds the original — read or unread — truncated at 16 KiB) |
| Annotations | read tools are `readOnlyHint`, `mark_seen` is `idempotentHint`, `config.set` is `destructiveHint` |
| Instructions | the consumption contract (mark seen only after successful processing) ships as server instructions |
| **MCP Apps UI** | `ui://briefkasten/inbox` — an interactive inbox (switch between unread/read/all, read a message, mark seen, archive, delete, compose) rendered by hosts supporting the MCP Apps extension; linked from `email.list_unread` and `email.send_status` |

Built on [mcp-go](https://github.com/klarlabs-studio/mcp-go).

## Run

```bash
go install go.klarlabs.de/briefkasten/cmd/briefkasten@latest

BRIEFKASTEN_ADDR=:8090 BRIEFKASTEN_MAILDIR=./maildir briefkasten   # serve (default)
```

Or install the release build:

```bash
brew install klarlabs-studio/tap/briefkasten
```

### Transports

Briefkasten serves MCP over HTTP by default. Clients that spawn the
server as a child process — Claude Desktop, Claude Code, and most local
MCP hosts — want stdio instead:

```bash
briefkasten serve --stdio --config /path/to/briefkasten.yaml
```

Equivalently `transport: stdio` in the config file, or
`BRIEFKASTEN_TRANSPORT=stdio`. Precedence is flag > env > file.

On stdio the protocol owns stdout, so logs go to stderr instead. Basic
auth is an HTTP concern and is ignored (with a warning) — over stdio the
peer is the process that spawned the server.

Wiring it into a host that reads `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "briefkasten": {
      "command": "/opt/homebrew/bin/briefkasten",
      "args": ["serve", "--stdio", "--config", "/absolute/path/briefkasten.yaml"]
    }
  }
}
```

Pass `--config` as an absolute path: a spawned server does not inherit a
predictable working directory, so the bare `./briefkasten.yaml` lookup
will not find the file.

## CLI

The same binary is a human client over the same mailbox:

```bash
briefkasten list   [--scope unread|read|all] [--folder F] [--account A] [--json]
briefkasten read   <id>
briefkasten seen   <id> [id ...]
briefkasten search <query> [--scope unread|read|all]
briefkasten folders [--curation]   # --curation: where archive/delete would file
briefkasten profiles          # names switchable via config.set {"profile": ...}
briefkasten send   --to a@b.c --subject S --body B [--html '<p>H</p>'] [--attach file.pdf ...]
briefkasten retry  <id>       # re-queue a failed send and deliver
briefkasten outbox            # outbound ids by lifecycle state
briefkasten archive <id> [id ...]   # prompts y/N once; --yes to skip
briefkasten delete  <id> [id ...]   # prompts y/N; soft delete — to trash
briefkasten hashpw            # argon2id hash for auth.basic.password_hash
briefkasten --version         # or `version`; --json adds toolchain + platform
```

`--version` answers before any config is read, so it works on a machine
where nothing is set up yet:

```bash
$ briefkasten --version
briefkasten 0.21.0 (commit: ebe0094a073a…, built: 2026-07-25T11:43:43Z)

$ briefkasten --version --json
{
  "commit": "ebe0094a073a…",
  "date": "2026-07-25T11:43:43Z",
  "go": "go1.26.3",
  "platform": "darwin/arm64",
  "version": "0.21.0"
}
```

### Human-in-the-loop curation

Archive and delete are deliberately guarded, everywhere:

- **MCP**: `email.archive` / `email.delete` ask the human through MCP
  elicitation (the host shows a confirmation; decline aborts). Clients
  without elicitation must pass `confirm: true` — the tool descriptions
  instruct agents to ask the user first.
- **CLI**: interactive `[y/N]` prompt; only an explicit yes proceeds. A
  batch is one prompt, and it names the count and the destination.
- **Semantics**: both are soft moves. Dir backend files into
  `.archive`/`.trash` sub-maildirs; IMAP copies into Archive/Trash and
  marks the original seen — deliberately not `MOVE`, which expunges.
  Briefkasten never destroys data.
- **The destination is discovered, not assumed**: briefkasten asks the
  server where its archive and trash live rather than hardcoding folder
  names. See below.
- **Read mail is no exception**: widening the scope widens what an agent
  can *see*, never what it may do unattended. Curating an
  already-processed message passes through the same gate as curating the
  backlog, and a stale id is rejected rather than reported as moved.

### Where curated mail lands (IMAP)

Servers disagree about where archive and trash live. Many root every
folder under the inbox — `INBOX.Trash`, not `Trash` — so a hardcoded
name copies into nothing and, worse, invites creating a stray folder
outside the namespace the user's mail client reads. Briefkasten asks
instead, in order of authority:

1. **`archive_folder` / `trash_folder` in config** — an explicit
   override, for layouts that defy discovery.
2. **The server's own declaration** (RFC 6154 SPECIAL-USE) — whichever
   mailbox is marked `\Archive` or `\Trash`.
3. **The personal namespace's conventional path** — the prefix reported
   by `NAMESPACE` plus `Archive`/`Trash`, used when it already exists.
4. **A known localized or legacy name** — `Papierkorb`, `Corbeille`,
   `Deleted Items`, `Archiv`, … matched case-insensitively.

Only when none of those resolve does briefkasten create a folder, and it
creates it *inside* the namespace. If that fails, the error names every
location it looked in rather than reporting a move that never happened.

Servers commonly declare `\Trash` but not `\Archive`, so the two often
resolve by different routes on the same mailbox — that is expected.

Step 4 ranks last deliberately. A mailbox touched by several clients over
the years can hold `Trash`, `Deleted Messages`, and `Papierkorb` at once,
and a name table cannot tell which one the human still opens — only the
server or the operator can settle that. Aliases exist to stop briefkasten
creating a *fourth* one beside them, nothing more.

### Seeing where mail would go

Curation destinations are inspectable before anything moves:

```bash
$ briefkasten folders --curation
archive  INBOX.Archive  (convention)
delete   INBOX.Trash  (declared)
```

`--json` gives the machine-readable form. Over MCP the same plan rides on
the `email://folders` resource under `curation`, and the archive/delete
confirmation prompt names the destination — so whoever approves a move
can see where it lands before saying yes.

## Configure

Three layers, 12-factor precedence — **env > config file > defaults**:

```yaml
# briefkasten.yaml (or point BRIEFKASTEN_CONFIG elsewhere)
transport: http          # or stdio; http listens on addr, stdio uses stdin/stdout
addr: ":8090"
backend: imap            # or maildir; inferred from imap.addr when omitted
maildir: ./maildir
imap:
  addr: imap.example.org:993
  username: alice
  password: "..."
  mailbox: INBOX
  archive_folder: ""     # optional; empty means discover (see below)
  trash_folder: ""       # optional; empty means discover
runtime_config: false    # enable config.get / config.set MCP tools
profiles:                # named configurations, switchable at runtime
  personal:
    backend: maildir
    maildir: /var/mail/personal
```

Every key has an env override: `BRIEFKASTEN_TRANSPORT`, `BRIEFKASTEN_ADDR`, `BRIEFKASTEN_BACKEND`,
`BRIEFKASTEN_MAILDIR`, `BRIEFKASTEN_IMAP_ADDR` / `_USER` / `_PASSWORD` /
`_MAILBOX` / `_INSECURE`, `BRIEFKASTEN_RUNTIME_CONFIG`.

**Secrets stay where you put them.** `config.set` rewrites the config file, but a
password supplied through `BRIEFKASTEN_IMAP_PASSWORD`, `BRIEFKASTEN_SMTP_PASSWORD`,
`BRIEFKASTEN_AUTH_PASSWORD`, or `BRIEFKASTEN_AUTH_PASSWORD_HASH` is never written
to it — keeping credentials in the environment is a deliberate choice, and
persisting them would undo it silently. The same holds for `client_id` /
`client_secret` / `token_url` read from an OAuth2 `credentials_file`: they live in
that file and are re-read on each load, so they are not copied into the config
file either. Values you write in the file yourself are untouched and still persist.

### Endpoint auth

The MCP endpoint is open by default — fine on localhost. Before exposing
the port (and especially with `runtime_config: true`), guard it with
basic auth:

```yaml
auth:
  basic:
    username: alice
    password_hash: "$argon2id$..."   # briefkasten hashpw
    # or password: "..." — hashed (argon2id) at startup
```

Env overrides: `BRIEFKASTEN_AUTH_USER`, `BRIEFKASTEN_AUTH_PASSWORD`,
`BRIEFKASTEN_AUTH_PASSWORD_HASH`. Generate the hash with
`briefkasten hashpw` (reads the password from stdin). Every request must
carry `Authorization: Basic …`; verification is constant time
([auth-go](https://github.com/klarlabs-studio/auth-go) argon2id), failures
are opaque, and only the MCP handshake (`initialize`, `ping`) stays open
so clients can negotiate before presenting credentials.

### Sending

```yaml
outbox:
  dir: ./outbox             # lifecycle state lives here; enables email.send
  from: nexa@local.example
  deliver_dir: ./delivery   # DirSender: .eml into delivery/new (local loop)
  smtp:                     # set addr to deliver over SMTP instead
    addr: smtp.example.org:587
    username: alice
    password: "..."
```

Each message is a statechart: `queued → sending → sent | failed`, with
`failed → queued` on retry — modeled with
[statekit](https://github.com/klarlabs-studio/statekit), persisted as files
under `outbox/<state>/`, so a restart resumes where it stopped. Startup
recovery repairs an unclean shutdown: a message stranded mid-send moves to
`failed` (the wire outcome is unknowable — `email.retry` re-queues it
deliberately rather than risking a silent duplicate send). The worker
delivers asynchronously; `email.send` returns immediately with the outbox
id. SMTP delivery is fortify-wrapped (timeout, exponential-backoff retry).
Env overrides: `BRIEFKASTEN_OUTBOX_DIR` / `_FROM` / `_DELIVER_DIR`,
`BRIEFKASTEN_SMTP_ADDR` / `_USER` / `_PASSWORD` / `_INSECURE`.

### OAuth2 (Gmail, Outlook)

App passwords are being phased out; configure OAuth2 instead:

```yaml
imap:
  addr: imap.gmail.com:993
  username: you@gmail.com
  oauth2:
    client_id: "<oauth client id>"
    client_secret: "<oauth client secret>"
    refresh_token: "<refresh token>"
    token_url: https://oauth2.googleapis.com/token
    mechanism: xoauth2        # or oauthbearer
```

Access tokens are minted and refreshed automatically from the refresh
token. Obtain the refresh token once via your provider's consent flow
(for Google: create an OAuth client in Cloud Console with the
`https://mail.google.com/` scope, then run any standard authorization-code
flow — the OAuth 2.0 Playground works). The same block applies to
`outbox.smtp.oauth2` for sending.

#### Google credentials file

Instead of hand-copying the OAuth fields, point Briefkasten at a downloaded
Google credentials JSON with `credentials_file`. Both of the credential JSON
types Google issues are accepted:

```yaml
imap:
  addr: imap.gmail.com:993
  username: you@gmail.com
  oauth2:
    credentials_file: /run/secrets/google.json
    refresh_token: "<refresh token>"   # only for an OAuth client secret
```

- **OAuth client secret** (the `client_secret_*.json` downloaded from Cloud
  Console, `{"web":…}` or `{"installed":…}`) — fills `client_id`,
  `client_secret`, and `token_url` from the file. You still supply a
  `refresh_token` (from the consent flow).
- **Service-account key** (`type: service_account`) — server-to-server: the
  account impersonates `username` via domain-wide delegation, so **no refresh
  token is needed**. Workspace only — a service account cannot act for a
  consumer `@gmail.com` account, and delegation for the `https://mail.google.com/`
  scope must be granted in the Workspace admin console.

The file can also be supplied via environment:
`BRIEFKASTEN_IMAP_OAUTH2_CREDENTIALS_FILE` and
`BRIEFKASTEN_SMTP_OAUTH2_CREDENTIALS_FILE`.

### Multiple accounts

```yaml
maildir: ./maildir            # the default account
accounts:
  business:
    imap: { addr: imap.example.org:993, username: b@firm.example, password: "..." }
```

Tools route via `account`; `email://accounts` lists the names.

### Runtime reconfiguration over MCP

With `runtime_config: true` two extra tools are served:

| Tool | Does |
|---|---|
| `config.get` | Active configuration and the available profiles — credentials redacted |
| `config.set` | Switch to a declared `profile`, or apply a partial patch: validates the new backend **and outbound sender**, hot-swaps them, persists to the config file |

#### Profiles — the safe way to switch mailboxes

Declare whole configurations up front and switch between them by name:

```yaml
maildir: /var/mail/work
runtime_config: true
profiles:
  work:
    backend: maildir
    maildir: /var/mail/work
  personal:
    backend: imap
    imap:
      addr: imap.fastmail.com:993
      username: me@personal.example
      password: ${BRIEFKASTEN_PERSONAL_PASSWORD}
    outbox:
      from: me@personal.example
      smtp: { addr: smtp.fastmail.com:587, username: me@personal.example }
```

```jsonc
{ "profile": "personal", "confirm": true }
```

A profile is applied **whole** and inherits nothing from the live config — the
endpoint and its credentials are written together, by you, in this file. That is
what makes it safe: a caller picking a profile is choosing among destinations you
already approved, and can never name an endpoint of its own. It is also the only
way to move the mailbox outside the startup maildir, since field-level patches
are confined to that subtree.

`briefkasten profiles` lists them from the CLI. Profiles and field-level
settings cannot be combined in one call — a profile is applied whole, so mixing
the two would silently drop one.

#### Field-level patches

`config.set` reconfigures **without a restart** — the reading backend and the
outbound sender are swapped live (the delivery worker keeps running). It patches
the IMAP backend, the outbox SMTP sender, and the **OAuth2 credentials** of
either, including a Google `credentials_file`:

```jsonc
// point the sender at a new Google credentials file, live:
{
  "outbox": {
    "smtp": {
      "addr": "smtp.gmail.com:587",
      "username": "you@gmail.com",
      "oauth2": { "credentials_file": "/run/secrets/google.json" }
    }
  }
}
```

Patching any `oauth2` field rebuilds the OAuth2 settings from scratch, so a new
credentials file is re-read and a stale token source is dropped. A failed
`config.set` leaves the old backend and sender serving — validation happens
before either swap.

**Guards.** `config.set` requires human confirmation (elicitation, or
`confirm=true`). Credentials never follow an `addr` change: supply them for the
new endpoint in the same call, or pass `clear_credentials` to connect without
any — so a caller who does not know the password cannot choose where it is sent.
TLS is one-way at runtime (`insecure` can be turned off, never on), and the
maildir stays inside the one chosen at startup. Use a profile to move further.

Off by default, and worth keeping that way unless you trust the caller: the
caller may be a model acting on mail content it just read.

The default backend is a maildir-style directory: drop `.eml` files into
`<maildir>/new` — that's "receiving mail". Consumers fetch and mark seen;
seen messages move to `<maildir>/cur`. Ideal for development, testing, and
pipelines that already export messages to disk.

### IMAP backend

Set `BRIEFKASTEN_IMAP_ADDR` to serve a real mailbox instead:

```bash
BRIEFKASTEN_IMAP_ADDR=imap.example.org:993 \
BRIEFKASTEN_IMAP_USER=alice \
BRIEFKASTEN_IMAP_PASSWORD=... \
briefkasten
```

Ids are message UIDs. `email.list` is `UID SEARCH UNSEEN` / `SEEN` /
`ALL` depending on `scope`,
`email.fetch` reads `BODY.PEEK[]` (fetching never sets `\Seen`), and
`email.mark_seen` stores `+FLAGS \Seen`. One authenticated connection is
reused across calls — validated before each use, closed on any error, and
re-dialled when the server drops it, so there is still no state to lose
across restarts or idle timeouts. A batch (`ids`) issues one `COPY` and
one `STORE` for the whole set rather than a round trip per message.
Optional: `BRIEFKASTEN_IMAP_MAILBOX` (default `INBOX`),
`BRIEFKASTEN_IMAP_INSECURE=1` for plaintext IMAP (local/testing only).

Remote backends are wrapped in [fortify](https://github.com/klarlabs-studio/fortify)
resilience automatically: per-call timeout, exponential-backoff retry,
and a circuit breaker that fast-fails while the server is down. Bad
message ids are never retried and never trip the breaker.

#### Gmail

Gmail speaks IMAP — no extra backend needed:

1. Enable 2-step verification on the Google account.
2. Create an [app password](https://myaccount.google.com/apppasswords)
   (regular passwords don't work over IMAP).
3. Point briefkasten at it:

```yaml
imap:
  addr: imap.gmail.com:993
  username: you@gmail.com
  password: "<app password>"
```

Briefkasten only sets the `\Seen` flag — Gmail's "mark as read" — unless
you archive or delete, which file into whichever mailbox Gmail declares
as `\Archive` / `\Trash` (see [Where curated mail
lands](#where-curated-mail-lands-imap)). Use a Gmail filter + label and
set `imap.mailbox` to that label to scope what the connector sees.

## Consume

Any MCP client works. With mcp-go:

```go
transport, _ := client.NewHTTPTransport("http://localhost:8090")
c := client.New(transport)
c.Initialize(ctx)

res, _ := c.CallTool(ctx, "email.list_unread", map[string]any{})
// fetch each id, ingest, then email.mark_seen — only after success,
// so failures stay unread for retry.

// Looking back at processed mail is the same call with a scope, and the
// ids it returns act like any other: fetch, archive, or delete them.
old, _ := c.CallTool(ctx, "email.list", map[string]any{"scope": "read"})
```

Instead of polling, subscribe to `email://inbox` (mcp-go ≥ 1.17 supports
resource subscriptions over HTTP+SSE) — the server pushes
`notifications/resources/updated` when new mail arrives.

## Bring your own backend

Implement the `Mailbox` port and serve it:

```go
type Mailbox interface {
    ListUnread(ctx context.Context) ([]string, error)
    Fetch(ctx context.Context, id string) ([]byte, error)
    MarkSeen(ctx context.Context, id string) error
}

mcp.ServeHTTP(ctx, briefkasten.NewServer(myIMAPBox), ":8090")
```

Every method must honour its context: when it is cancelled or its
deadline passes, return promptly with an error that wraps
`context.Canceled` or `context.DeadlineExceeded`. That is what lets the
per-call timeout be a real bound rather than a documented one, and how
the retry and circuit-breaker layers tell "we stopped waiting" from "the
backend is broken".

Gmail, Exchange, a database queue — anything that can list, fetch, and
acknowledge. The tool contract stays identical for every consumer.
(Maildir and IMAP ship built-in: `NewDirMailbox`, `NewIMAPMailbox`.)

## Design notes

- **Mark-seen is the consumer's acknowledgement.** Briefkasten never deletes;
  backends decide what "seen" means (maildir move, IMAP flag, …). It is
  idempotent — acknowledging read mail again is a no-op, not an error.
- **Read state filters, it does not gate.** `scope` narrows what a
  listing returns; it never decides what an id may be used for. Every
  action resolves an id across the whole mailbox, so the only difference
  between curating fresh and processed mail is which listing surfaced
  it. Backends implementing `Curator` must honour that.
- **Ids are opaque** to consumers and validated by backends (the dir backend
  rejects path traversal).
- **Raw bytes, not parsed mail.** Parsing/MIME policy belongs to the
  consumer; the wire format is base64 RFC 5322.

## Architecture

Hexagonal, dependencies point inward only:

```
domain/          ports + invariants: Mailbox (+ Searcher, FolderMailbox,
                 Curator capabilities), Sender, OutboundMessage, the
                 outbox statechart, OutboxStore
application/     the use cases — Service (routing, list/read/seen/search/
                 folders/archive/delete) and the Outbox engine. The MCP
                 tools and the CLI call the SAME methods.
infrastructure/  maildir, imap, smtp, auth (OAuth2/XOAUTH2), resilience,
                 and mcpserver (the MCP presentation adapter)
briefkasten      root: compatibility facade + Config (composition)
cmd/briefkasten  composition root; CLI = thin presentation
```

Human-in-the-loop confirmation lives at the interface layer (MCP
elicitation, CLI prompt); the shared use case executes after approval.

## License

MIT
