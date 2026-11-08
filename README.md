# mado

Self-hosted Remote Control for Claude Code sessions on many machines, including sessions
whose models are open-weight models behind vLLM / SGLang / llama.cpp. Spec: `DESIGN.md`.
Verified CLI facts: `docs/claude-code-facts.md`.

One binary, `mado`:

| command | runs on | what |
|---|---|---|
| `mado local` | one computer | hub and worker in one process, loopback only, no IdP (see below) |
| `mado hub --config hub.toml` | hub host | web UI (OIDC login, Preact SPA + SSE), chat sessions, worker WebSocket, SQLite |
| `mado hub enroll <name>` | hub host | prints a one-time enrollment code (15 min) |
| `mado hub remove-machine <name>` | hub host | removes an offline machine (ending its live sessions) and revokes its token |
| `mado enroll --hub URL --name N --code C` | each machine | trades the code for a machine token (`~/.config/mado/machine.token`, 0600) |
| `mado worker` | each machine | systemd user service: hub connection, `tmux -L mado`, shim discovery, loopback model router |
| `mado shim --session ID` | inside tmux | owns one `claude -p` stream-json child, spool, unix socket, readable log |
| `mado approve-tool --socket S [--tools …]` | spawned by claude | MCP server for `--permission-prompt-tool mcp__mado__approve`, and the hub's `web_search`/`fetch_url` |
| `mado session-models <spool>` | anywhere | models, turns, Agent calls and tokens of one session |
| `mado guard-tool --enforce M` | spawned by claude | PreToolUse hook: denies the main model's edit tools (roles.enforce) |
| `mado print-argv --profile P --cwd D` | anywhere | prints the claude argv and env the worker would spawn (debugging) |

## Build

    make build   # bin/mado; GO defaults to ~/.local/go-sdk/go/bin/go
    make test    # MADO_NO_TMUX_TESTS=1 skips the tmux integration tests
    make vet     # go vet + gofmt check

## Run it on one computer (no server)

`mado local` runs the hub and a worker in one process on the computer in front of you:
no separate hub host, no Pocket ID, no enrollment. The hub listens on `127.0.0.1:8740` only and the
sign-in page has one "Continue" button for the local user (an admin).

    brew install tmux
    curl -fsSL https://claude.ai/install.sh | bash   # Claude Code; log in once with `claude`
    # the binary: dist/mado-<version>-darwin-arm64 from `make release` (darwin-amd64 for
    # Intel Macs), or `go build -o mado ./cmd/mado` in a checkout
    mado local                                       # opens http://127.0.0.1:8740

Flags: `--port 8740`, `--no-browser`, `--config ~/.config/mado/hub.toml`. The first run
writes, and prints the paths of:

* `~/.config/mado/hub.toml`: local defaults, with an AI& chat model and router profile
  commented out as examples.
* `~/.config/mado/secrets.toml` (0600): an empty `[keys]` table.
* `~/.config/mado/machine.token` (0600): the in-process worker's credential.

The database is `~/.local/share/mado/hub.db`, sockets live in `~/.local/state/mado/run`,
spools in `~/.local/state/mado/spool`. Code sessions on the built-in `subscription` profile
(your normal Claude Code login) work with no key. For chat models and router profiles, put
the key in `secrets.toml` as `aiand = "sk-..."` under `[keys]`, uncomment the examples in
`hub.toml` (`key_ref = "aiand"`) and restart `mado local`. An optional
`~/.config/mado/worker.toml` sets `project_roots` (default: your home directory),
`claude_bin` or `tmux_bin`; its hub settings are ignored.

No key at all: with [Ollama](https://ollama.com) running on the same computer, a chat model needs
only `kind = "openai"`:

```toml
[[chat.models]]
name = "ollama-qwen"
label = "Qwen3 8B (Ollama)"
kind = "openai"
base_url = "http://127.0.0.1:11434/v1"
model = "qwen3:8b"                        # as `ollama list` shows it
context_window = 32768                    # Ollama's default context is small: raise
max_output = 8192                         # OLLAMA_CONTEXT_LENGTH to match
timeout_first_byte = "120s"
default = true
```

Sessions run only while the computer is awake; claude keeps running in tmux across a restart of
`mado local` and the session is resumed. Reaching the computer from a phone over Tailscale
(`tailscale serve` with header login) is coming in M10b (DESIGN §19.5); until then the
hub is reachable from this computer only.

## Deploy

Build once (any machine with Go): `make release` writes static `linux/amd64`,
`linux/arm64`, `darwin/arm64` and `darwin/amd64` binaries plus `SHA256SUMS` to `dist/`. Copy the matching binary to each
machine; `mado version` prints the `git describe` it was built from. On hosts where linger or `tailscale serve` need root, run `sudo loginctl enable-linger <user>` and `sudo tailscale set --operator=<user>` once, then rerun.

**Hub host** (any always-on Linux machine).

    deploy/install-hub.sh --binary dist/mado-<ver>-linux-amd64 \
        --base-url https://hub.<tailnet>.ts.net --issuer https://id.<tailnet>.ts.net
    tailscale serve --bg --https=443 http://127.0.0.1:8740

The script writes `~/.config/mado/hub.toml` and `secrets.toml` (0600, only if missing),
creates `~/.local/share/mado` (database), installs and starts `mado-hub.service` as a systemd
user unit and enables linger. In Pocket ID create the OIDC client: callback URL
`<base_url>/callback`, scopes `openid profile email groups` (the `groups` scope must be
allowed), allowed user groups `mado_users` and `mado_admins`, and put yourself in
`mado_admins`.

**Worker on each machine.**

    deploy/install-worker.sh --binary dist/mado-<ver>-linux-<arch> \
        --hub wss://hub.<tailnet>.ts.net/ws/worker --name gpu1 --root ~/work

**Enrollment.** On the hub run `mado hub enroll gpu1 --config ~/.config/mado/hub.toml` (or, as
an admin, use "Enroll machine" in the UI); it prints a one-time code valid 15 minutes. On the
machine run the printed `mado enroll --hub https://hub.<tailnet>.ts.net --name gpu1 --code ...`,
then `systemctl --user start mado-worker.service`. The machine shows as online on `/`.
To decommission a machine, stop its worker (or destroy the host), then run
`mado hub remove-machine gpu1 --config ~/.config/mado/hub.toml` (or use Remove on the admin
"Machines" page, `/admin/machines`); the hub refuses while the worker is connected. Sessions
on it that are still idle or running are marked ended ("machine removed"); the CLI prints how
many and the UI warns before removing. Its token stops working, its old sessions stay readable,
and the name can be enrolled again.

**First login.** Open `https://hub.<tailnet>.ts.net`, sign in through Pocket ID; your user is
created on that first login, and membership in `mado_admins` makes you an admin. Start a
session from "New session".

## Hub

Copy `deploy/hub.toml.example` to `~/.config/mado/hub.toml`, set the Pocket ID issuer and
client id, and profiles. Upstream keys go in `secrets.toml` (mode 0600).
Put it behind `tailscale serve` so `base_url` is https (cookies are then `Secure`).

Who may log in is decided in Pocket ID: give the client the allowed user groups `mado_users`
(everyone who may use mado) and `mado_admins`. Users are created on their first login. Members
of `oidc.admin_group` (default `mado_admins`, from the `groups` claim, rechecked at every login)
are admins: they get the user list with budgets (`/admin/users`), the profile and chat model
editor (`/profiles`), the Claude Code history (`/history`), the machine list with removal (`/admin/machines`), web enrollment (`/admin/enroll`),
everyone's usage and the hub-wide `/audit`. Everyone else sees only their own sessions, usage
and audit rows; admins also never see other users' sessions. An optional `[allow]` list in `hub.toml` narrows logins further.

Without an IdP: `mado hub --local-login` (refused unless the listen address is loopback;
`--dev-login` is the old name) offers the local login, an admin user `local`, to direct
loopback requests only. `mado local` turns it on by itself.

## Web UI

The hub serves a single-page app (`internal/hub/ui`, Preact 10 + htm as vendored ES
modules, no build step) at `/`, `/s/{id}`, `/new`, `/usage`, `/settings` and the admin pages;
it talks to the JSON API under `/api` and the session event stream `/s/{id}/events`. Only
the login page is rendered on the server. Layout as in the Claude
web app:

* **Sidebar**: New chat, New code session, a search box that filters Recents by title,
  Recents (chat and code, newest first, with a live state dot), links to the admin pages for
  admins, and the user menu with Log out. Below 900 px it is a drawer behind the ☰ button.
* **Transcript**: Markdown rendered by the hub, tool calls grouped into chunks ("Bash ×3"),
  subagent work nested under its Agent call, web_search results as links, generated
  images inline with a Download link, approval cards (Allow / Allow for session / Deny), a chat answer streaming as raw text until the stored message replaces it, a Copy
  button on code blocks. "diagnostics" shows hidden stderr lines and auto-allowed approvals.
  In a code session, file paths in messages (`~/x/NOTES.md`, `internal/hub/api.go`) are links
  that open the file from the session's machine in the details panel: Markdown rendered,
  other text as is, anything else as a download (DESIGN §27).
* **Composer**: Enter sends, Shift+Enter is a new line, Esc interrupts a running turn. In a
  chat session: attach files with the clip button, by pasting or by dropping them on the
  transcript (a file attached but never sent is deleted after 24 h), and switch the model
  for the next turn. In a code session the permission mode
  is shown and `/new <machine> <project> [profile] [mode]` starts another session.
* **Details panel** (right; a sheet behind the ⓘ button below 900 px): for code sessions the
  machine, directory, profile, main model, the permission mode (a select that changes it
  mid-session), "Auto decisions" with the auto switch (mado auto sessions), "Limits"
  (subscription sessions), the Models panel and pending approvals, plus "New session here"; for chat sessions the model, the enabled tools, tokens
  and cost this session and this month, and the attachments (generated images included). Rename, Interrupt, Stop (code) and Delete
  (chat always; code once stopped).
* **New session** (`/new`): a Chat tab (model, optional system prompt and first message) and
  a Code tab (machine, project from the machine's list, profile, main model, permission mode).

* **Usage** (`/usage`): tokens (input, cache read and write, output) and cost of a UTC
  month by model, with a month picker and a CSV export of the month's rows
  (`/api/usage.csv`). Admins pick any user or see everyone, with totals per user.
* **Settings** (`/settings`): your account, role, this month's spending and budget.
* **Admin pages**: `/admin/users` (users, last login, this month's cost, budget; edit a
  user's budget, the admin flag is read-only), `/profiles` (profiles, chat models and prices as JSON,
  validated by the hub, plus the classifier and concierge model; a `hub.toml` entry saved
  there becomes an override in the hub's database with "Revert to hub.toml", and the file
  itself is never written), `/history`, `/audit`, `/admin/enroll`.

Light and dark follow the system setting.

## Tools

`[chat.tools]` in `hub.toml` enables tools on the hub (see `deploy/hub.toml.example`):

* `web_search(query)`: SearXNG (`kind = "searxng"`, `base_url`) or the Brave Search API
  (`kind = "brave"`, `key_ref`); the top 8 hits with title, URL and snippet.
* `fetch_url(url)`: on by default; 10 s, at most 2 MB, HTML turned into readable text
  (go-readability), truncated at `fetch_max_chars` (20000).
* `generate_image(prompt, size?)`: the OpenAI Images API (`kind = "openai"`, `model`
  default `gpt-image-1`, `key_ref`). The image is stored as the user's file and shown in
  the chat; the model only hears that it was shown. Its token counts are priced through
  `[[prices]]`.

Chat sessions offer every enabled tool (at most `max_tool_rounds` rounds per message).
Code sessions get `web_search` and `fetch_url` as `mcp__mado__web_search` and
`mcp__mado__fetch_url`: the worker's MCP server relays each call to the hub, which runs it
(keys and backends never reach the worker); `web_search` needs no approval, `fetch_url`
asks like `WebFetch`. `fetch_url` and SearXNG refuse loopback, private, CGNAT (the
tailnet), link-local and ULA addresses, checked on every connection and redirect, unless
`allow_private = true` (`[chat.tools]` for fetch, `[chat.tools.search]` for SearXNG).

## Inbox

The inbox collects feeds, pushed messages, Slack, mail and (later) GitHub
notifications into mado, read-only, and turns the new items into a digest and task
proposals (DESIGN §23). Items are visible to the account's owner only; admins create and
edit accounts. Enable it in `hub.toml`:

```toml
[inbox]
enabled = true

[inbox.digest]
executor = "claude"            # default: a claude job on a worker with jobs; "chat" = the hub's chat models only
model = ""                     # chat executor and fallback: a [[chat.models]] name; omit = the concierge model
machine = ""                   # claude executor: pin a worker by name; omit = first online worker with jobs
main_model = "sonnet"          # claude executor: --model (fable, opus, sonnet, haiku or a claude-* id)
schedule = ["08:00", "20:00"]
timezone = "Asia/Tokyo"
language = "ja"

[[inbox.accounts]]             # seeded at every start, read-only in the UI
name = "x-ai"
kind = "rss"
owner = "me@example.org"       # a user's sub or e-mail (resolved at their login)
interval = "15m"
[inbox.accounts.config]
url = "http://home-twitter-rss.example.ts.net:1200/twitter/home_latest"
allow_private = true           # needed for tailnet/LAN hosts (SSRF guard)
```

**An rss account** (RSS, Atom, JSON Feed; any RSSHub route) needs only `url`; add it under
`[[inbox.accounts]]` or on Inbox → Accounts → New account (admins) with
`{"name": "x-ai", "kind": "rss", "owner_sub": "me@example.org", "interval_s": 900,
"config": {"url": "…", "allow_private": true}}`. It is polled every interval with
conditional GET; Poll now fetches at once.

**A push account** receives items instead of polling. Put a random token (at least 16
characters) in the secrets file, name it in `key_ref`, and POST JSON (an object or an
array of objects, at most 256 KiB, 60 requests per minute):

```sh
curl -X POST https://mado.example/api/inbox/ingest/angel \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"text": "hello", "author": "bob", "url": "https://x.com/bob/status/1", "ts": 1760918400}'
```

`{account}` is the account id (shown on Inbox → Accounts) or its name when only one push
account has it. Each object becomes an item: `text` from `text`/`body`/`message`/`title`,
`author` from `author`/`from`/`user`, `ts` from `ts`/`time`/`created_at` (RFC 3339 or Unix
seconds), `url` from `url`/`link`; the object is kept verbatim and a repeat is ignored.

Digests run at the scheduled times for every owner with new items, or on Digest now; the
consumed items become read, task proposals land under Tasks for you to accept. Chat and
concierge sessions get `search_inbox`, `get_item`, `list_tasks`, `create_task` and
`update_task` over your own inbox.

**Where a digest runs.** With `executor = "claude"` (the default) the hub starts a *job
session* on a worker that offers jobs (see [Worker](#worker)): one `claude -p` turn on that
machine's subscription login, `--model <main_model>`, no built-in tools (`--tools ""`), only
`propose_task` through the mado MCP server, at most 12 turns. It shows in the sidebar under
the "digests" filter like a code session (machine, transcript, Models panel). When no
worker with jobs is online, the pinned `machine` is offline, the start is refused or not
acknowledged within 60 s, or claude ends without a good result (the subscription's usage
limit makes claude exit 2 without one; `is_error`; no result within 20 min), the same items
run on the chat executor and the digest row says `chat`, with the claude reason in its
note. `executor = "chat"` never uses a worker.

### Slack

A `slack` account reads one workspace with your own user token; nothing is ever posted,
marked read or changed. Create the token once per workspace:

1. On <https://api.slack.com/apps> choose Create New App → From scratch, name it (e.g.
   `mado-inbox`) and pick the workspace.
1. OAuth & Permissions → Scopes → **User Token Scopes** (not Bot Token Scopes): add
   `channels:history`, `groups:history`, `im:history`, `mpim:history`, `channels:read`,
   `groups:read`, `im:read`, `mpim:read`, `users:read`.
1. Install to Workspace, allow, and copy the **User OAuth Token** (`xoxp-…`). Leave token
   rotation off (rotating `xoxe.xoxp-` tokens expire after 12 hours and are refused).
1. Put it in the secrets file: `[keys]` `slack-myws = "xoxp-…"`.

Channel ids: open the channel → its name (channel details) → the id `C…` is at the bottom
of the About tab. A private channel's id also starts with `C` (old ones with `G`); you must
be a member. Then:

```toml
[[inbox.accounts]]
name = "slack-myws"
kind = "slack"
owner = "me@example.org"
key_ref = "slack-myws"
interval = "10m"
[inbox.accounts.config]
channels = ["C0123456789", "C0987654321"]
dms = true                     # every open DM and group DM, list refreshed hourly
replies = true                 # thread replies (default true)
initial_hours = 24             # how far back the first poll reaches (default 24)
```

Collected: new messages of the listed channels and, with `dms`, of your DMs and group DMs;
thread replies of threads whose parent is younger than 7 days; bot messages that have text.
Names (`@user`, `#channel`) and links are written out in the text, each item links back
to Slack. Not collected: edits and deletions (the first version stays), joins and leaves,
reactions, file contents (only file names), channels not listed, other workspaces (one
account per workspace).

### Mail (IMAP)

An `imap` account collects mail read-only: the collector only EXAMINEs folders and
fetches with `BODY.PEEK`, so it never sets flags (mail stays unread in Gmail), never
moves and never deletes anything. It is built for Gmail with an app password:

1. Turn on 2-Step Verification for the Google account (required for app passwords).
1. Create an app password at https://myaccount.google.com/apppasswords and put it in
   `secrets.toml`:

   ```toml
   [keys]
   gmail_app_password = "abcdabcdabcdabcd"
   ```

1. Add the account (IMAP is always on for personal Gmail accounts):

   ```toml
   [[inbox.accounts]]
   name = "gmail"
   kind = "imap"
   owner = "me@example.org"
   key_ref = "gmail_app_password"
   interval = "10m"
   [inbox.accounts.config]
   host = "imap.gmail.com"
   port = 993                  # implicit TLS; plaintext is refused (143 needs starttls = true)
   username = "me@gmail.com"
   folders = ["INBOX"]         # Gmail labels: "[Gmail]/Starred", "[Gmail]/All Mail", "my-label"
   initial_days = 7            # how far back the first poll goes
   max_per_poll = 200          # per folder per request; the rest follows at once
   ```

Gmail labels appear as folders: system labels under `[Gmail]/…`, your own labels by
name (nested ones as `parent/child`). A message with several labels is collected once per
listed folder. Each item has the subject as title, `Name <address>` of the sender as
author, the `text/plain` part as text (else the HTML part converted to text), charsets
such as Shift_JIS and ISO-2022-JP decoded, the thread from `References`/`In-Reply-To`,
and a link that opens the message in Gmail. Attachments are listed (name, type, size) in
the item's raw data but never downloaded.

**Mail from another provider.** A mailbox you cannot read over IMAP (a school or employer
Microsoft 365 account, say) can be forwarded into Gmail by its own forwarding feature. To keep
it apart, create a Gmail filter on `to:<the forwarded address>` that applies a label such as
`forwarded` (optionally skipping the inbox) and list `"forwarded"` in `folders`. Forwarded mail
keeps its original sender.

### GitHub

A `github` account collects your GitHub notifications (the bell), one item per thread
**update**: a pull request that gets a new comment tomorrow is a new item in the same
thread. Nothing is ever marked read or changed on GitHub; the collector only sends GETs.

1. Create a fine-grained token: GitHub → Settings → Developer settings → Personal access
   tokens → Fine-grained tokens → Generate new token. Resource owner: your account; pick
   an expiry. Under **Account permissions** set **Notifications: Read-only**. To have the
   bodies of private issues and pull requests fetched, also give **Repository access** to
   those repositories with **Issues**, **Pull requests** and **Contents: Read-only**
   (public repositories need nothing more).
1. Put it in `secrets.toml`: `github_pat = "github_pat_…"` under `[keys]`.
1. Add the account in `hub.toml` (or Inbox → Accounts → New account):

```toml
[[inbox.accounts]]
name = "github"
kind = "github"
owner = "me@example.org"
key_ref = "github_pat"
interval = "5m"
[inbox.accounts.config]
participating_only = false   # true: only threads you take part in
fetch_bodies = true          # fetch the issue/PR body and the latest comment (first 4 KiB each)
received_events = false      # not implemented yet; true is rejected
initial_days = 7             # how far the first poll looks back
```

Each item's kind is the notification reason: `review_requested`, `mention`,
`team_mention`, `assign`, `author`, `comment`, `state_change`, `subscribed`,
`ci_activity`, `security_alert`, `manual`, `invitation`, …. The title is
`owner/repo: subject [Type]`, the link goes to the issue, pull request, commit, release
or discussion page (the latest comment when one was fetched), and the text holds the
state, the body and the latest comment. The digest proposes a task (`owner/repo#N: …`)
for review requests, mentions, assignments and threads where someone waits for you;
accept it under Tasks. mado never polls faster than GitHub's `X-Poll-Interval`; when the
rate limit runs out the account backs off and the error names the reset time.

## Usage and budgets

Every model response is a row in the `usage` table (code sessions from claude's stream,
chat sessions from the hub's own runner). `[[prices]]` and `[budgets]` in `hub.toml` are
plain numbers in `[budgets] currency` (default USD; `JPY` shows `¥1,234`). A user's budget
is their own (set by an admin on `/admin/users`) or `[budgets] default_monthly`; with
neither there is no limit. Once the month-to-date cost reaches it, chat messages, new chats,
new code sessions on router profiles and messages into them are refused (HTTP 402, with the
amounts) until the next UTC month; subscription sessions are never refused.

## Worker

    deploy/install-worker.sh --hub wss://hub.example.ts.net/ws/worker --name gpu1 --root ~/work
    mado enroll --hub https://hub.example.ts.net --name gpu1 --code <code>
    systemctl --user start mado-worker.service

If `systemctl --user` says "Failed to connect to bus", the shell lacks the user session
environment (typically after `su`). Fix it with

    export XDG_RUNTIME_DIR=/run/user/$(id -u) DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus

or log in via ssh instead.

Watch a session in a terminal: `tmux -f /dev/null -L mado attach`. Ctrl-C in a window detaches
the session (the hub will not restart it; continue with `claude --resume <id>` by hand).

**Usage limits on the subscription.** When Claude Code reports that the 5-hour or 7-day
window is used up, the turn ends and the transcript shows "Usage limit reached (5-hour
window). Resumes automatically at 02:30 (JST) · in 2 h 14 m" in your browser's time; the
details panel shows the windows ("5 h: 26 % · resets 11:30"). You may keep typing. About
two minutes after the reset the hub sends again, word for word, the messages that hit the
limit or came while it lasted, or asks claude to continue the interrupted work when the
limit struck mid-turn; a stopped session is resumed first. At most 6 automatic attempts in
a row. Untick "Resume automatically when the limit resets" in the banner or the panel to
continue by hand. Router sessions are not affected.

Recovery: the router listens on a fixed port (`router_listen`, default `127.0.0.1:8742`), so a
daemon restart does not break running sessions; the shim keeps the session's router token in
`spool_dir/<session>.token` (0600) and the hub pushes profile and keys again on reconnect, then
resyncs events from the spool. If a session's window is gone (tmux killed, reboot), the hub
has the worker recreate it as `mado shim --resume <claude-session-id>`. Spools live in
`spool_dir` (default `~/.local/state/mado/spool`), sockets in `$XDG_RUNTIME_DIR/mado`. Under
systemd the tmux server runs in its own `mado-tmux.scope`, outside the worker service.

**Jobs (inbox digests on the subscription).** On the machine whose `claude` is logged in to
the Claude subscription (e.g. the always-on host), add to `worker.toml`

```toml
jobs = true
# job_dir = "~/.local/state/mado/jobs"   # default $XDG_STATE_HOME/mado/jobs, created at start
```

and restart the worker (`systemctl --user restart mado-worker`). The worker offers the
capability `jobs` only when `jobs = true` and `~/.claude/.credentials.json` exists (on macOS
the login is in the keychain, so `jobs = true` is enough). Job sessions run in `job_dir`
(outside `project_roots`) with the default Claude Code config dir, so their transcripts land
in `~/.claude/projects/` like any session. Pin the machine with `[inbox.digest] machine =
"<name>"` in `hub.toml`; check the argv with `mado print-argv --job digest --cwd <job_dir>`.

## Profiles

`subscription` (always present) runs plain `claude` with the normal login. A `router` profile
points Claude Code at the worker's loopback router (`ANTHROPIC_BASE_URL`, per-session bearer,
`ANTHROPIC_DEFAULT_{FABLE,OPUS,SONNET,HAIKU}_MODEL`, `CLAUDE_CONFIG_DIR=~/.mado/claude-<profile>`);
the router maps the names to upstream models and adds the upstream key. Claude never sees a key.
Upstreams may declare `context_window` and `max_output`; for the session's main model they
become `CLAUDE_CODE_MAX_CONTEXT_TOKENS` and `CLAUDE_CODE_MAX_OUTPUT_TOKENS`.

**OpenAI-compatible upstreams.** An upstream (or a `[[chat.models]]` entry) with
`kind = "openai"` is any chat-completions server: vLLM or tt-inference-server, Ollama,
LM Studio, llama.cpp. `base_url` is the API root with `/chat/completions` under it, the
key is optional (sent as `Authorization: Bearer`), and the router translates Claude Code's
Messages API traffic both ways (tool calls, images, reasoning as thinking blocks, usage
with cached tokens; `count_tokens` is estimated). Here the director (`fable`, the main
model) runs on a local server and the implementers stay on AI&:

```toml
[[profiles]]
name = "local-director"
kind = "router"
main = "local-glm"
[profiles.aliases]
fable = "local-glm"
opus = "ai-ds"
sonnet = "ai-ds"
haiku = "ai-flash"
[profiles.upstreams.local-glm]
kind = "openai"
base_url = "http://gpu1:8000/v1"          # vLLM; Ollama: http://127.0.0.1:11434/v1
model = "zai-org/GLM-5.1-FP8"             # the server's model name
timeout_first_byte = "180s"               # default 60s; a cold prefill takes longer
timeout_total = "20m"                     # default 10m
context_window = 131072                   # the server's limits, not the model card's
max_output = 16000
[profiles.upstreams.ai-ds]
base_url = "https://api.aiand.com"
model = "deepseek-ai/deepseek-v4-pro"
key_ref = "aiand"
[profiles.upstreams.ai-flash]
base_url = "https://api.aiand.com"
model = "deepseek-ai/deepseek-v4-flash"
key_ref = "aiand"
```

The server must support tool calls (vLLM: `--enable-auto-tool-choice
--tool-call-parser <parser>`, plus `--reasoning-parser` for reasoning models) and a
context window large enough for Claude Code's system prompt and tools (about 20k tokens
before any work). Price the local model with a `[[prices]]` entry (zero is fine) keyed on
its `model` string. Workers must run the same mado version as the hub: an older worker
ignores `kind` and would send the Messages API to the server.

**Role split.** By default every profile starts claude with three subagents
(`--agents`): `implementer` and `researcher` on the `opus` alias, `simple` on `sonnet`,
and a short prompt (`--append-system-prompt`) telling the main model to direct, delegate
through the Agent tool and review instead of writing the code itself. For a router profile
the aliases resolve through the profile's `aliases`, so e.g. GLM directs and DeepSeek
implements. Tune it per profile:

```toml
[profiles.roles]
enabled = true          # false: one model does everything
implementer = "opus"    # fable | opus | sonnet | haiku | inherit
researcher  = "opus"
simple      = "sonnet"
prompt = ""             # empty: mado's default delegation prompt
enforce = "edits"       # off | edits | all
```

The prompt alone is not enough (a GLM main model answered a "look into this directory"
question with its own Bash and Read calls), so the split is also enforced: a PreToolUse
hook passed with `--settings` runs `mado guard-tool`, which denies `Edit`, `Write` and
`NotebookEdit` (`all`: also `Bash`) when the **main** model calls them and tells it to
delegate to `implementer`, `researcher` or `simple`. Subagents are not affected: Claude Code
marks their hook calls with `agent_id`. `off` keeps only the prompt. Under `edits` the main
model can still read and run commands, as the user's rule allows for checks.

The New session form (Code tab) preselects the profile's `main` (subscription: `fable`), shows the
profile's alias and role tables and its `enforce` value, and allows only the profile's own
upstream names as an override. Permission mode is `default` (ask in the browser),
`acceptEdits` or `auto`, and `auto` is the default for new sessions on every profile. On
the subscription profile `auto` is Claude Code's own auto mode ("auto (claude decides)":
its classifier decides, and what it still escalates reaches you as an approval card). On a
router profile it is **mado auto mode** ("auto (mado decides)"), below. `dontAsk`, `plan`
and `bypassPermissions` are refused by hub and worker.

**Changing the mode while a session runs.** The details panel's permissions row is a
select. A change is stored and sent to the running claude at once through Claude Code's
stream-json control protocol (`set_permission_mode`), so the next tool call already uses
it; for a router session `auto` puts claude in `acceptEdits` and turns mado's policy on.
When claude cannot be told (an older worker, the machine offline, no answer in 5 s) the
panel offers "Restart to apply", which stops claude and resumes the conversation in the new
mode. A stopped session keeps the new mode for its next Resume.

**mado auto mode.** Claude Code's auto mode needs an Anthropic classifier, so with an
open-weight main model it silently starts in `default` and every tool call waits for you.
mado owns the permission prompt, so for a router session in `auto` ("auto (mado
decides)" in the form and the panel) the hub decides instead: claude runs with `--permission-mode
acceptEdits` (edits inside the project pass; git can undo them) and every other approval
request goes through, in order,

1. an earlier "Allow for session" for the same call;
2. **deny rules** that nothing overrides: `sudo`, `su`, recursive `rm` of `/`, `~`, `..`,
   `*` or `$HOME`, force pushes, `git reset --hard`, `git clean -f`, `git checkout -- .`,
   `curl … | sh`, `chmod -R 777`, `mkfs`, `dd of=/dev/…`, fork bombs, `shutdown`/`reboot`,
   `systemctl stop|disable|mask`, `kill -9`, `pkill`, `killall`, `tmux kill-server`,
   `claude login`, any path with `.ssh/`, `.env`, `*.token`, `secrets.toml`,
   `.config/mado/`, `.claude/settings.json` or a private key name, and writes under `/etc`,
   `/usr`, `/boot` or `/var/lib`. Claude is told "mado policy denied this: <rule>; ask the
   user in your reply if it is needed" and carries on;
3. **allow rules**: Read, Glob, Grep, WebFetch, the hub's web tools, TodoWrite, Task/Agent,
   and Bash lines whose every command (split on `;`, `&&`, `||`, `|`) is a read-only one
   (`ls`, `cat`, `grep`, `rg`, `find`, `git status|diff|log|show`, …) or a build step
   (`go build|test|vet`, `make [target]`, `pytest`, `cargo`, `npm test|run`, `git add|commit`,
   `mkdir -p`, `cp`, `mv`, `touch`, `cd`) whose paths and redirections stay inside the
   machine's project roots or `/tmp`. Anything the lexer cannot follow (`$(…)`, subshells)
   is never allowed by rule;
4. the **classifier**, a chat model named in `[auto] classifier`, called with a forced
   `decide` tool (allow, deny or ask, with a reason). 20 s per call, one retry; any
   failure means ask. Its tokens are
   usage rows of kind `auto`, paid by the session's owner;
5. you, as before. The card shows why mado asked.

Every automatic decision is a collapsed card in the transcript ("auto-allowed by mado: git
status", "auto-denied by the classifier: …") and a row in the audit log. Auto-allowed cards
are hidden unless "diagnostics" is ticked; denied ones always show. The details panel counts
the decisions and has a switch that sends every approval to you while claude keeps running
(`PATCH /api/sessions/{id}` with `{"auto": false}`). `/new <machine> <project> <profile>
auto` starts such a session from the composer. `[auto] allow` and `deny` add RE2 regexes to
the defaults (see `deploy/hub.toml.example`); without `classifier` only the rules run.

**Which model did the work.** The session's details panel has a Models panel (turns per
model, main vs subagent, Agent calls, tokens), and `GET /api/sessions` carries the same
summary (`models`, e.g. `glm-5.3 ×15, deepseek-v4-flash ×14 (sub)`). On a worker, `mado session-models
~/.local/state/mado/spool/<session>.spool` prints the same table. Tool calls between two
texts collapse into one chunk ("Bash ×3"); a subagent's work nests under its Agent call.
Tick "diagnostics" to see hidden stderr lines such as `[claude-code:unrecognized_model]`.

In a session, the composer also accepts `/new <machine> <project> [profile] [mode]` (exact names from
the machine's project list; default profile `subscription`, default mode `default`; double-quote a path with spaces:
`/new gpu1 "/home/me/my app"`). "New session here" in the details panel opens the form with
the session's project preselected. Interrupt ends the current turn (also Esc in the
composer); Stop ends the turn and lets claude exit.

## Claude Code history (read-only)

With `claude_history = true` in `worker.toml` the worker ships the transcripts of the
user's own Claude Code (`~/.claude/projects/*/*.jsonl`, or `claude_home`) and of mado's
per-profile config dirs (`~/.mado/claude-*/projects/*/*.jsonl`) to the hub. Nothing is ever
written to those files. After each connect the hub tells the worker how far it has each file;
the worker then sends only the tail of files whose size or mtime changed (every 60 s, or at
once with "Rescan now" on `/history`). A last line without a newline is held back until it is
complete; a file that shrank is sent again from the start. Every line is stored raw; lines
that are not JSON are kept and shown as "unparsable", unknown entry types as collapsed JSON.

`/history` (admins only for now: machines have no owner yet) lists transcripts by machine with
a search over title and directory; `/history/{id}` renders one with the session view's
transcript (same blocks and tool chunks), 300 lines per page. "Resume in mado" starts a subscription session with
`claude --resume <id>` in the transcript's directory (which must lie inside one of the
machine's projects); it is offered only for transcripts from `~/.claude`.

Spools and token files of sessions the hub reports as ended or unknown are deleted by the
worker once they are older than `spool_retention` (default `168h`).
