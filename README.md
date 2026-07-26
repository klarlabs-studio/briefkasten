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
| `email.fetch` | `{"id"}` → `{"raw": "<base64 RFC 5322>"}`, or `{"ids": [...]}` → `{"fetched": [{"id", "raw"}], "failed": [...], "total": N}` — read or unread; never sets `\Seen`; a batch is refused whole if it measures over 25 MiB |
| `email.mark_seen` | `{"id"}` → `{"ok": true}`, or `{"ids": [...]}` → `{"marked": [...], "failed": [...], "total": N}` — message won't be listed again; idempotent, so re-acknowledging read mail is not an error |
| `email.send`* | `{"to": [...], "subject", "body", "cc?", "bcc?", "html_body?", "attachments?": [{"filename", "content_type", "content": "<base64>"}], "confirm?"}` → `{"id", "state": "queued"}` — **human-confirmed**; `bcc` travels in the SMTP envelope only and is never rendered into the message; attachments ≤ 10 MiB each, ≤ 25 MiB per message |
| `email.reply`* | `{"id", "body", "all?", "html_body?", "attachments?", "confirm?"}` → `{"id", "state": "queued"}` — **human-confirmed**; you pass the *message id*, never recipients: they are derived from the original (`Reply-To` else `From`; `all` adds its To + Cc as Cc, never its Bcc, never this mailbox) and the reply threads via `In-Reply-To`/`References` |
| `email.forward`* | `{"id", "to": [...], "body?", "html_body?", "confirm?"}` → `{"id", "state": "queued"}` — **human-confirmed**; the original is attached whole as `message/rfc822`, so its own attachments survive byte for byte; one over 10 MiB is refused with its measured size |
| `email.send_status`* | `{"id"}` → `{"state": "queued\|sending\|sent\|failed", "attempts", "error?"}` |
| `email.retry`* | `{"id"}` → `{"id", "state": "queued"}` — re-queue a failed send |
| `email.search` | `{"query", "scope?", "folder?", "account?", "limit?"}` → `{"ids": [...], "total": N, "scope": "unread"}` — case-insensitive; IMAP searches server-side |
| `email.archive` | `{"id" \| "ids", "confirm?"}` → `{"ok": true}` / `{"archived": [...], "failed": [...], "total": N}` — **human-confirmed** (elicitation or confirm flag); soft: filed to Archive, never destroyed; read or unread |
| `email.delete` | `{"id" \| "ids", "confirm?"}` → `{"ok": true}` / `{"deleted": [...], "failed": [...], "total": N}` — **human-confirmed**; soft delete to Trash, never expunged; read or unread |
| `email.folder_create` | `{"name", "account?", "confirm?"}` → `{"ok": true, "folder": "Work"}` — **human-confirmed**; idempotent; the name is resolved into the account's folder space (`Work` → `INBOX.Work` on an `INBOX.`-rooted server) |
| `email.folder_delete` | `{"name", "account?", "confirm?"}` → `{"ok": true, "folder": "Work"}` — **human-confirmed**; **empty folders only**: one holding mail is refused with the count, no force flag; the inbox and the curation destinations are refused outright |

Every mailbox tool — `email.list`, `email.list_unread`, `email.fetch`,
`email.mark_seen`, `email.search`, `email.archive`, and `email.delete` —
accepts optional `folder` (see `email://folders`) and `account` (see
`email://accounts`) arguments. `limit` caps the ids returned; `total`
always reports the full count.

### Bulk: many ids, one call

`email.fetch`, `email.mark_seen`, `email.archive`, and `email.delete`
take either `id` (one message) or `ids` (a batch of up to **100**) —
exactly one of the two; both, or neither, is refused. On IMAP a batch
costs what one message costs: the curation folder is resolved once, and
one `UID FETCH`, one `UID COPY` and one `UID STORE` carry the whole set.
Fifty deletes go from **301 commands to 7**; fifty fetches from **100
commands to 3** — a connection probe, one `UID FETCH` to measure the
whole set, one to carry every body.
Backends that gain nothing from batching (the dir backend: local
renames, no round trips) loop through a shared fallback and behave
identically to the caller.

Four properties are deliberate:

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
  moved, never destroyed. Messages: a.eml, b.eml, c.eml.` (Curation
  only: `email.fetch` is read-only and has no gate.)
- **`email.fetch` is bounded by size, not just by count.** A hundred ids
  is a fine blast radius for a soft move and a terrible one for a fetch:
  a hundred messages with attachments is easily hundreds of megabytes,
  enough to blow the client's context or its memory. So a batch is
  measured *before* any body is read — one `UID FETCH RFC822.SIZE` on
  IMAP, a `stat` per file on the dir backend — and refused whole if it
  totals over **25 MiB**, the same ceiling `email.send` puts on one
  outbound message. The refusal names the budget, the measured total and
  the id count, so the caller can split and retry. Nothing is ever
  truncated: a cut-off RFC 5322 message is corrupt data a consumer may
  parse without noticing, and an error is the honest answer.

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

### Reply, reply-all, forward

`email.reply` and `email.forward` take **the id of the message being
answered, never a recipient list**. The server reads the original and
derives the audience, so the arithmetic lives in one tested place instead
of being re-invented by each caller:

| Rule | Why |
|---|---|
| To = the original's `Reply-To`, else its `From` | a sender who set `Reply-To` asked for answers elsewhere; ignoring it lands the reply in an unattended mailbox |
| `all` additionally puts the original's To + Cc into Cc | everyone who could already see each other — and nobody who could not |
| **Bcc is never a source of recipients** | if you were Bcc'd you cannot see the others; where a Bcc list *is* visible (a Sent-folder copy), replying to it would broadcast a list its sender deliberately hid. `forward` drops it for the same reason |
| the configured `outbox.from` is excluded from every derived set | compared on the addr-spec, lowercased — a display name cannot smuggle it back in |
| an address in both To and Cc appears once | deduplicated on the addr-spec across the whole derived set |
| `In-Reply-To` = the original's `Message-Id`; `References` = its `References` + that id | so the reply threads instead of starting a new conversation in every client |
| no `Message-Id` on the original ⇒ **no threading headers at all** | a fabricated parent threads the conversation onto a message that never existed; an unthreaded reply merely shows up on its own |
| `Re:` / `Fwd:` are added only when absent (case-insensitive), and an existing prefix is left as written | no `Re: Re: Re:`, and a correspondent's `AW:` is not rewritten into our house style |
| every derived address is validated exactly like a caller-supplied one | it came out of a message's headers, which makes it attacker-supplied data |

A forward carries the original as a `message/rfc822` attachment rather
than re-rendering it inline. Re-encoding means decoding every part and
encoding it again, and whatever the renderer does not model — an inline
image, a signature, a part whose transfer encoding matters — is lost in
the round trip. Attached whole, the recipient gets the bytes that
arrived. Because the original cannot then be split, one over the 10 MiB
attachment ceiling is refused with its measured size, the way an
oversized `email.fetch` batch is.

**Bcc is never a rendered header.** `email.send` accepts `bcc`, and those
addresses reach the SMTP envelope (`RCPT TO`) and nothing else. A Bcc
that appeared in the message would not be blind: the header travels with
the message, so it would show the whole hidden list to exactly the people
it was hidden from — and the sender would not notice, because their own
copy looks correct.

#### The confirmation prompt leads with the count

Sending is the irreversible operation, and a reply-all can expand the
audience by two orders of magnitude from a request that looks identical
("reply to everyone with…" is one sentence in an email body — the
injection [SECURITY.md](SECURITY.md) describes). So the prompt states the
number of people first, breaks it down by field, samples a few addresses,
and calls the Bcc count out on its own:

```
Send this reply to 80 recipients? (2 To, 5 Cc, 73 Bcc) — alice@example.com,
bob@example.com, cc000@example.com, cc001@example.com, cc002@example.com and
75 more. Subject: "Re: Q3 planning". 73 of them are Bcc — hidden from every
other recipient and from each other, so that part of the audience cannot be
checked by eye. Sending cannot be undone.
```

Eighty addresses printed in full would bury the one number a human can
actually verify, so the list is a sample. The Bcc count is stated
separately because those addresses appear in no header: if it is not
said here it is said nowhere.

There is deliberately **no cap on recipients**. A reply-all to a large
thread is ordinary mail, and a cap would only teach callers to split one
send into batches that each understate the audience. Visibility is the
control.

\* Sending registers only when an outbox is configured.

Beyond tools, the full MCP surface:

| Surface | What |
|---|---|
| Resources | `email://inbox`, `email://inbox/{id}` (raw RFC 5322), `email://inbox/{id}/headers` (parsed from/to/subject/date/message_id — triage without fetching the body), `email://outbox`, `email://outbox/{id}`, `email://folders` (folders plus the curation destinations and how each was decided), `email://accounts` — read state without spending tool calls; `{id}` serves and completes read and unread ids alike |
| Prompts | `summarize_inbox(count?)` (embeds up to `count` unread messages, default 20, capped at 100 and clamped rather than refused, each truncated at 16 KiB), `draft_reply(id)` (embeds the original — read or unread — truncated at 16 KiB) |
| Annotations | read tools are `readOnlyHint`, `mark_seen` is `idempotentHint`, `config.set`, the sending tools and the folder tools are `destructiveHint` |
| Instructions | the consumption contract (mark seen only after successful processing) ships as server instructions |
| **MCP Apps UI** | `ui://briefkasten/inbox` — an interactive inbox rendered by hosts supporting the MCP Apps extension; linked from `email.list_unread` and `email.send_status`. Switch folders (read from `email://folders`, which also names where archive and delete file) and between unread/read/all, search the folder on show, read a message, mark seen, archive, delete, reply and reply-all in a composer that opens under the message, forward it to recipients you type, and compose with cc/bcc. Select several messages and mark seen, archive or delete them as one batch — the host is elicited once for the whole set, and the 100-id cap is refused before you are asked. Create and delete folders, where a folder holding mail is refused with its message count. Watch the outbox (`email://outbox`) with failures listed first, and re-queue a failed send with `email.retry` — the one action that could previously be started here and then lost sight of, since delivery is asynchronous. Every gated send still goes through the host's own confirmation — the page never sends `confirm` on your behalf |

Built on [mcp-go](https://github.com/klarlabs-studio/mcp-go).

## Run

```bash
go install go.klarlabs.de/briefkasten/cmd/briefkasten@latest

BRIEFKASTEN_ADDR=:8090 BRIEFKASTEN_MAILDIR=./maildir briefkasten   # serve (default)
```

Or install the release build:

```bash
brew trust klarlabs-studio/tap                       # first time only
brew install --cask klarlabs-studio/tap/briefkasten
```

Homebrew refuses to load a cask from a third-party tap it has not been
told to trust, and says so rather than installing:

```
Error: Refusing to load cask klarlabs-studio/tap/briefkasten from untrusted tap klarlabs-studio/tap.
```

`brew trust` is a per-machine decision and only needs making once for the
tap, not once per tool. Briefkasten ships as a cask rather than a formula
because it is a pre-compiled binary — Homebrew's own guidance — which is
also why the install strips the quarantine attribute macOS puts on
unnotarized downloads.

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
briefkasten read   <id> [id ...]   # several ids: length-prefixed records
briefkasten seen   <id> [id ...]
briefkasten search <query> [--scope unread|read|all]
briefkasten folders [--curation]   # --curation: where archive/delete would file
briefkasten folders --create NAME  # idempotent; namespace-aware on IMAP
briefkasten folders --delete NAME  # empty folders only; prompts y/N, --yes skips
briefkasten profiles          # names switchable via config.set {"profile": ...}
briefkasten send   --to a@b.c --subject S --body B [--cc x@y.z] [--bcc h@i.j] [--html '<p>H</p>'] [--attach file.pdf ...]
briefkasten reply  <id> --body B [--all] [--html '<p>H</p>'] [--attach file.pdf ...]
briefkasten forward <id> --to a@b.c [--body B] [--html '<p>H</p>']
briefkasten retry  <id>       # re-queue a failed send and deliver
briefkasten outbox            # outbound ids by lifecycle state
briefkasten archive <id> [id ...]   # prompts y/N once; --yes to skip
briefkasten delete  <id> [id ...]   # prompts y/N; soft delete — to trash
briefkasten hashpw            # argon2id hash for auth.basic.password_hash
briefkasten --version         # or `version`; --json adds toolchain + platform
```

`read` with one id prints the message and nothing else, exactly as it
always has. With several it length-prefixes each one, because no marker
line can safely delimit mail — any delimiter can occur inside a message,
and escaping mail is how a consumer ends up parsing something that is no
longer the message:

```
id a.eml 42
From: a@b.c
Subject: A

hallo
id b.eml 41
…
```

A reader takes the header line `id <id> <bytes>`, reads exactly that
many bytes, and skips the trailing newline — nothing to unescape.
`--json` gives the structured per-id form instead (`fetched` with `raw`
base64-encoded, plus `failed`), which is also what a single id returns
under `--json`. A batch measuring over 25 MiB is refused before anything
is read, and a partly failed batch exits non-zero with each unreadable
id named on stderr.

`reply` and `forward` take no recipients: they read the message and
derive them by the rules above, then print the derived audience — count
first, then the addresses — before the message goes, since that is the
one thing the operator did not type:

```
$ briefkasten reply --all --body 'Tuesday works.' orig.eml
to 3 recipient(s): "Alice" <alice@example.com>, "Bob" <bob@example.com>, carol@example.com
sent: 6f1c…
```

`--bcc` on `send` is envelope-only: the address is delivered to and never
written into the message, so no other recipient can see it.

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
  instruct agents to ask the user first. `email.folder_create` and
  `email.folder_delete` pass through the identical gate: reshaping the
  mailbox is as easy to ask for from inside an email body as moving mail
  is.
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

### Creating and deleting folders

```bash
briefkasten folders --create Work        # idempotent; no prompt — nothing moves
briefkasten folders --delete Work        # prompts y/N; empty folders only
```

Over MCP the same two operations are `email.folder_create` and
`email.folder_delete`, both behind the human gate.

**Creating** is namespace-aware and idempotent. `--create Work` on a
server that roots folders under the inbox makes `INBOX.Work` — the same
resolution curation uses, so the folder lands where the user's mail
client looks rather than beside it. A folder that already exists is a
success, not an error: the caller asked for a folder to exist, and it
does. On the dir backend a folder is a whole maildir (`new/`, `cur/`,
`tmp/`), and names that would escape the mailbox root — `../escape`,
`a/b`, a leading dot — are refused, as are the names reserved for
curation.

**Deleting never destroys mail.** That is the same invariant `email.delete`
holds, applied one level up:

| Asked to delete | Answer |
|---|---|
| an empty folder | deleted |
| a folder holding messages | **refused**, with the count: *`"Work"` holds 3 messages — archive or delete them first (both are soft moves, so nothing is destroyed), then delete the folder* |
| a folder with subfolders | **refused**, naming them — delete the leaves first |
| `INBOX` | **refused** — it is the mailbox itself |
| the archive or trash folder | **refused** — removing where curation files would break `email.archive` and `email.delete`; the destination is resolved by the same path curation resolves it, so an override moves the protection with it |

There is deliberately **no force flag**. A flag that turns the invariant
off is the invariant not holding; the way to delete a folder full of mail
is to move the mail out first, and every step of that is itself a soft
move.

The emptiness check races, and the docs say so rather than pretending
otherwise: the count is taken immediately before the delete, but on IMAP
a message delivered in that window is deleted with the folder, and no
ordering of two IMAP commands can prevent it. On the dir backend the race
loses safely — removing a directory that has just gained a file fails at
the kernel, so the folder and the message both survive.

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
  dir: ./outbox             # lifecycle state lives here; enables email.send/reply/forward
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
delivers asynchronously; `email.send`, `email.reply` and `email.forward`
all return immediately with the outbox id. `from` is also the address
excluded from every derived reply recipient set — the transport reports
it, so the reply rules and the `From:` header can never disagree. SMTP delivery is fortify-wrapped (timeout, exponential-backoff retry).
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
across restarts or idle timeouts. A curation batch (`ids`) issues one
`COPY` and one `STORE` for the whole set rather than a round trip per
message; a fetch batch issues one `UID FETCH RFC822.SIZE` to measure it
and one `UID FETCH BODY.PEEK[]` to carry it.
Optional: `BRIEFKASTEN_IMAP_MAILBOX` (default `INBOX`),
`BRIEFKASTEN_IMAP_INSECURE=1` for plaintext IMAP (local/testing only).

Remote backends are wrapped in [fortify](https://github.com/klarlabs-studio/fortify)
resilience automatically: per-call timeout, exponential-backoff retry,
and a circuit breaker that fast-fails while the server is down. Caller
mistakes — a bad message id, a malformed batch, a fetch measured over
the budget — are never retried and never trip the breaker: the server
answered them correctly.

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
// fetch the ids (one call: {"ids": [...]}, refused if it measures over
// 25 MiB — split and repeat), ingest, then email.mark_seen — only after
// success, so failures stay unread for retry.

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
                 FolderManager, Curator capabilities), Sender,
                 OutboundMessage, the outbox statechart, OutboxStore
application/     the use cases — Service (routing, list/read/seen/search/
                 folders/archive/delete/folder create+delete) and the
                 Outbox engine. The MCP tools and the CLI call the SAME
                 methods.
infrastructure/  maildir, imap, smtp, auth (OAuth2/XOAUTH2), resilience,
                 and mcpserver (the MCP presentation adapter)
briefkasten      root: compatibility facade + Config (composition)
cmd/briefkasten  composition root; CLI = thin presentation
```

Human-in-the-loop confirmation lives at the interface layer (MCP
elicitation, CLI prompt); the shared use case executes after approval.

## Security

Message bodies are written by whoever can send you mail, they reach every
tool verbatim, and some of those tools send, archive, and delete. That is
the threat model, and it is why the mutating tools are gated on a human
rather than on a content filter: an email that *persuades* an agent to
propose a deletion is expected and caught by the gate; an email that
causes one *without* the gate is a bug worth reporting.

Report privately — [open a draft
advisory](https://github.com/klarlabs-studio/briefkasten/security/advisories/new),
never a public issue or PR. [SECURITY.md](SECURITY.md) has the channels,
the timelines, and what is and is not in scope.

## License

MIT
