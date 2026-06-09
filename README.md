# briefkasten

**A mailbox as an MCP server.**

Briefkasten (German: *letterbox*) exposes any message store through three
[Model Context Protocol](https://modelcontextprotocol.io) tools, so agent
runtimes and ingestion pipelines pull mail through a stable, language-agnostic
contract instead of binding to IMAP libraries:

| Tool | Does |
|---|---|
| `email.list_unread` | `{}` → `{"ids": ["..."]}` |
| `email.fetch` | `{"id": "..."}` → `{"raw": "<base64 RFC 5322>"}` |
| `email.mark_seen` | `{"id": "..."}` → `{"ok": true}` — message won't be listed again |
| `email.send`* | `{"to": [...], "subject", "body", "html_body?", "attachments?": [{"filename", "content_type", "content": "<base64>"}]}` → `{"id", "state": "queued"}` |
| `email.send_status`* | `{"id"}` → `{"state": "queued\|sending\|sent\|failed", "attempts", "error?"}` |
| `email.search` | `{"query", "folder?", "account?"}` → `{"ids": [...]}` — unread scope, case-insensitive; IMAP searches server-side |
| `email.archive` | `{"id", "confirm?"}` → `{"ok": true}` — **human-confirmed** (elicitation or confirm flag); soft: filed to Archive, never destroyed |
| `email.delete` | `{"id", "confirm?"}` → `{"ok": true}` — **human-confirmed**; soft delete to Trash, never expunged |

`email.list_unread`, `email.fetch`, `email.mark_seen`, and `email.search`
accept optional `folder` (see `email://folders`) and `account` (see
`email://accounts`) arguments.

\* Sending registers only when an outbox is configured.

Beyond tools, the full MCP surface:

| Surface | What |
|---|---|
| Resources | `email://inbox`, `email://inbox/{id}` (raw RFC 5322), `email://outbox`, `email://outbox/{id}`, `email://folders`, `email://accounts` — read state without spending tool calls; `{id}` completes from live unread ids |
| Prompts | `summarize_inbox` (embeds every unread message), `draft_reply(id)` (embeds the original) |
| Annotations | read tools are `readOnlyHint`, `mark_seen` is `idempotentHint`, `config.set` is `destructiveHint` |
| Instructions | the consumption contract (mark seen only after successful processing) ships as server instructions |
| **MCP Apps UI** | `ui://briefkasten/inbox` — an interactive inbox (list, read, mark seen, compose) rendered by hosts supporting the MCP Apps extension; linked from `email.list_unread` and `email.send_status` |

Built on [mcp-go](https://github.com/klarlabs-studio/mcp-go).

## Run

```bash
go install go.klarlabs.de/briefkasten/cmd/briefkasten@latest

BRIEFKASTEN_ADDR=:8090 BRIEFKASTEN_MAILDIR=./maildir briefkasten   # serve (default)
```

## CLI

The same binary is a human client over the same mailbox:

```bash
briefkasten list   [--folder F] [--account A] [--json]
briefkasten read   <id>
briefkasten seen   <id>
briefkasten search <query>
briefkasten folders
briefkasten send   --to a@b.c --subject S --body B [--html '<p>H</p>'] [--attach file.pdf ...]
briefkasten archive <id>      # prompts y/N; --yes to skip
briefkasten delete  <id>      # prompts y/N; soft delete — to trash
```

### Human-in-the-loop curation

Archive and delete are deliberately guarded, everywhere:

- **MCP**: `email.archive` / `email.delete` ask the human through MCP
  elicitation (the host shows a confirmation; decline aborts). Clients
  without elicitation must pass `confirm: true` — the tool descriptions
  instruct agents to ask the user first.
- **CLI**: interactive `[y/N]` prompt; only an explicit yes proceeds.
- **Semantics**: both are soft moves. Dir backend files into
  `.archive`/`.trash` sub-maildirs; IMAP copies into Archive/Trash and
  marks the original seen — deliberately not `MOVE`, which expunges.
  Briefkasten never destroys data.

## Configure

Three layers, 12-factor precedence — **env > config file > defaults**:

```yaml
# briefkasten.yaml (or point BRIEFKASTEN_CONFIG elsewhere)
addr: ":8090"
backend: imap            # or maildir; inferred from imap.addr when omitted
maildir: ./maildir
imap:
  addr: imap.example.org:993
  username: alice
  password: "..."
  mailbox: INBOX
runtime_config: false    # enable config.get / config.set MCP tools
```

Every key has an env override: `BRIEFKASTEN_ADDR`, `BRIEFKASTEN_BACKEND`,
`BRIEFKASTEN_MAILDIR`, `BRIEFKASTEN_IMAP_ADDR` / `_USER` / `_PASSWORD` /
`_MAILBOX` / `_INSECURE`, `BRIEFKASTEN_RUNTIME_CONFIG`.

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
under `outbox/<state>/`, so a restart resumes where it stopped. The worker
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
| `config.get` | Active configuration — credentials redacted |
| `config.set` | Partial patch: validates the new backend **and outbound sender**, hot-swaps them, persists to the config file |

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
before either swap. Off by default — `config.set` accepts mailbox credentials,
so enable it only on trusted networks.

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

Ids are message UIDs. `email.list_unread` is `UID SEARCH UNSEEN`,
`email.fetch` reads `BODY.PEEK[]` (fetching never sets `\Seen`), and
`email.mark_seen` stores `+FLAGS \Seen`. Each call dials a fresh
connection — no state to lose across server restarts or idle timeouts.
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

Briefkasten only sets the `\Seen` flag — Gmail's "mark as read". Nothing
is archived or deleted; use a Gmail filter + label and set
`imap.mailbox` to that label to scope what the connector sees.

## Consume

Any MCP client works. With mcp-go:

```go
transport, _ := client.NewHTTPTransport("http://localhost:8090")
c := client.New(transport)
c.Initialize(ctx)

res, _ := c.CallTool(ctx, "email.list_unread", map[string]any{})
// fetch each id, ingest, then email.mark_seen — only after success,
// so failures stay unread for retry.
```

## Bring your own backend

Implement the `Mailbox` port and serve it:

```go
type Mailbox interface {
    ListUnread() ([]string, error)
    Fetch(id string) ([]byte, error)
    MarkSeen(id string) error
}

mcp.ServeHTTP(ctx, briefkasten.NewServer(myIMAPBox), ":8090")
```

Gmail, Exchange, a database queue — anything that can list, fetch, and
acknowledge. The tool contract stays identical for every consumer.
(Maildir and IMAP ship built-in: `NewDirMailbox`, `NewIMAPMailbox`.)

## Design notes

- **Mark-seen is the consumer's acknowledgement.** Briefkasten never deletes;
  backends decide what "seen" means (maildir move, IMAP flag, …).
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
