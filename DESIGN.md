# mado — self-hosted Remote Control for coding agents

Status: design v1, 2026-10-18. Owner: the user. Design and briefs: Fable. Implementation: Opus / Sonnet.

## 1. Goal

A web UI on the user's hub host that shows and drives coding-agent sessions running on many
machines, the way Anthropic's Remote Control does for Claude Code, but for **open-weight models
served by vLLM / SGLang / llama.cpp** (and, as a bonus, for Claude itself on the subscription).

Anthropic's Remote Control refuses to run when `ANTHROPIC_BASE_URL` points at a non-Anthropic
host. That is the gap this project fills.

Non-goals: network security between machines (Tailscale, out of scope), hardware bring-up
(hugepages, docker), writing an agent loop (we run Claude Code as a child process), tmux-resurrect.

## 2. Decisions already made

| Topic | Decision |
|---|---|
| Language | Go for hub and worker. **No npm, no Node, no JS build step.** UI = Go `html/template` + htmx (vendored single file) + SSE. Since M6 the session UI is a Preact + htm SPA, still vendored and unbuilt (§17.5, §17.10); admin pages stay `html/template`. |
| Agent backend | Claude Code CLI (`claude`) as a child process in `-p` stream-json mode. Pinned version. Adapter interface kept so `pi` could be added later. |
| Why Claude Code for open-weight | Vendor docs (GLM, MiniMax, DeepSeek, Alibaba) point Claude Code at their Anthropic-compatible endpoints; GLM-5.1 card shows +5.5 TB2 points vs a generic harness; vLLM/SGLang/llama.cpp all serve `/v1/messages`. Codex CLI rejected: Responses-API-only wire, buggy on vLLM for non-gpt-oss, app-server marked experimental. |
| Auth | OIDC Authorization Code + PKCE against **Pocket ID**, scopes `openid profile email groups`. **Users are auto-created on first successful login** (who may log in is decided in Pocket ID via the client's allowed user groups). An optional `[allow]` list in hub config narrows this further. Members of the IdP group named by `oidc.admin_group` (default `mado_admins`) are admins; everyone else is a regular user. No per-machine ACL: every user may use every machine. |
| Visibility | A session is visible to its owner only. Sharing (like Claude Code's share links) is a TODO. |
| Roles | Regular user: start/drive own sessions, use existing profiles. Admin (IdP group): machine enrollment, profile editor, secrets key_ref visibility, hub-wide audit view. Admins still do not see other users' sessions. The admin flag is recomputed from the `groups` claim at every login. |
| Storage | SQLite via `modernc.org/sqlite` (pure Go, no cgo). Hub DB is the source of truth. |
| Process model per machine | **One** systemd user service (`mado-worker.service`, linger enabled). Sessions live as windows in a dedicated tmux server `tmux -L mado`, so a worker restart never kills a session. |
| Secrets | Upstream API keys live **only on the hub** (config file, mode 0600). They are pushed to the worker in memory at session start and never written to disk on the worker. The `claude` child never sees a real upstream key. |
| Two-model routing | **Core feature.** Each session has a *profile* that maps Claude Code's model aliases (`fable`/`opus`/`sonnet`/`haiku`) to upstream {endpoint, key, model}. The worker runs a local Anthropic-Messages-compatible router that performs the mapping. |
| Session start from UI | A form (machine, project directory from an allowlist, profile) and a `/new <machine> <project> [profile]` command in the chat box. Natural language later, via one tool call. Free text is never passed to a shell. |
| Formats | Claude Code's JSONL transcripts and stream-json are internal formats. Store the raw JSON, parse tolerantly, pin the CLI version. |
| Inbox | Read-only aggregation of Slack, mail, X (via RSSHub) and GitHub into per-user items, with model-made digests and task proposals (§23). No operating or notifying through those services. |

## 3. Topology

```
 phone / PC browser
        │ HTTPS (tailscale serve)
        ▼
 ┌──────────────┐   outbound WSS from each machine   ┌────────────────────────────┐
 │  hub (hub host)│◀───────────────────────────────────▶│ worker (per machine)       │
 │  Go binary   │                                     │ Go binary, systemd user svc│
 │  SQLite      │                                     │  ├─ tmux -L mado        │
 │  OIDC client │                                     │  │   └─ window per session:│
 │  htmx UI/SSE │                                     │  │       `mado shim`    │
 └──────────────┘                                     │  │         └─ claude -p    │
                                                      │  └─ local router :PORT     │
                                                      │       /v1/messages ──▶ vLLM A, vLLM B, Anthropic…
                                                      └────────────────────────────┘
```

### 3.1 Worker internals

* **daemon** (`mado worker`): connects to the hub (WSS, reconnect with backoff), owns the
  tmux server, discovers running shims through unix sockets in
  `$XDG_RUNTIME_DIR/mado/<session-id>.sock`, relays events hub⇄shim, runs the router.
* **shim** (`mado shim --session <id>`): runs *inside* the tmux window. It spawns
  `claude -p --output-format stream-json --input-format stream-json --verbose ...`, owns its stdin/stdout
  pipes, serves the unix socket, buffers events to a local spool file so nothing is lost while the
  daemon or hub is down, and prints a **human-readable rendering** of the stream to its own tty so
  that `tmux -L mado attach` shows a readable log. A user may stop the shim and run
  `claude --resume <claude-session-id>` in a normal terminal; the hub marks the session "detached".
* **router**: HTTP on `127.0.0.1:<port>`; path `/v1/messages` (streaming and non-streaming),
  pass-through of the Anthropic Messages API. Authenticates by a per-session bearer token
  (random, generated by the daemon). Looks at the request's `model`, maps it through the session's
  profile to an upstream, replaces `model`, forwards with the upstream key, streams the response
  back unchanged. Also handles `/v1/messages/count_tokens` (forward, or 501 if upstream lacks it).
* **approval MCP server** (`mado approve-tool`): a stdio MCP server spawned by `claude`
  (configured via `--mcp-config` and `--permission-prompt-tool mcp__mado__approve`). It forwards
  the permission request to the shim socket → daemon → hub → browser, waits for the decision, and
  returns `{"behavior":"allow"|"deny", ...}` in the format the Claude Code docs specify.
  The "allow for this session" answer is remembered in the hub and auto-answered thereafter.

### 3.2 Environment given to the `claude` child

For an open-weight profile:

```
ANTHROPIC_BASE_URL=http://127.0.0.1:<router-port>
ANTHROPIC_AUTH_TOKEN=<per-session token>
ANTHROPIC_DEFAULT_OPUS_MODEL=<alias-opus>      # names the router understands
ANTHROPIC_DEFAULT_SONNET_MODEL=<alias-sonnet>
ANTHROPIC_DEFAULT_HAIKU_MODEL=<alias-haiku>
CLAUDE_CODE_ATTRIBUTION_HEADER=0
API_TIMEOUT_MS=3000000
CLAUDE_CONFIG_DIR=$HOME/.mado/claude-<profile>   # keeps subscription login untouched; see §6
```

Plus `--model <alias-main>`. M4 adds the role split (§5, §15) to every profile kind unless
`roles.enabled = false`:

```
--permission-mode default|acceptEdits|auto  # form field, default preselected (auto since M5)
--model <main>                               # profile main (subscription: fable) unless overridden
--agents '{"implementer":{...,"model":"opus"},"researcher":{...,"model":"opus",
           "disallowedTools":["Edit","Write","NotebookEdit"]},"simple":{...,"model":"sonnet"}}'
--append-system-prompt '<delegation prompt>'
--forward-subagent-text                      # subagent text in the stream, for the Models panel
--settings '{"hooks":{"PreToolUse":[{"matcher":"Edit|Write|NotebookEdit",
             "hooks":[{"type":"command","command":"<mado>","args":["guard-tool","--enforce","edits"]}]}]}}'
                                             # M5: unless roles.enforce = "off" (§16)
```

The implementer must verify the exact alias env var names and
whether a `fable` alias env exists, from https://code.claude.com/docs/en/model-config and
https://code.claude.com/docs/en/settings before writing code; record findings in `docs/claude-code-facts.md`.

For the **subscription Claude** profile: none of the above is set; the child uses the normal
login in `~/.claude`. The two kinds of session coexist on the same machine because env is per
process. Nothing global (`~/.claude/settings.json`) is edited.

Router aliases: names like `oc/strong`, `oc/worker` are **not** allowed because Claude Code breaks
on `/` in model names; use `oc-strong`, `oc-worker`.

## 4. Hub ⇄ worker protocol (WebSocket, JSON text frames)

Every frame: `{"t": "<type>", "id": "<uuid>", "ts": "<rfc3339>", ...payload}`. The worker
authenticates with `Authorization: Bearer <machine-token>` on the upgrade request.

Worker → hub

| t | payload |
|---|---|
| `hello` | `machine`, `version`, `claude_version`, `capabilities[]`, `projects[]` (allowlisted dirs from worker config), `roots[]` (M12: the `project_roots` themselves), `job_dir?` (M16: with capability `jobs`, §23.6) |
| `session_state` | `session_id`, `state` (`starting`,`running`,`waiting_approval`,`idle`,`detached`,`exited`), `claude_session_id`, `exit_code?` |
| `event` | `session_id`, `seq` (monotonic per session, from the shim spool), `raw` (one stream-json object, verbatim) |
| `approval_request` | `session_id`, `request_id`, `tool_name`, `input` (raw), `suggestions?` |
| `sessions` | list of `{session_id, state, claude_session_id}` known to the worker (sent on connect, and on `list_sessions`); M3: `spools[]`, ids with spool/token files on disk but no live shim |
| `history_session` | M3, see §14: one transcript tail `{path, config_dir, default_config, claude_session_id, cwd, title, title_custom, mtime, size, offset, end, reset, lines[], line_offsets[]}` |
| `ack` | `ref` (id of the hub frame), `ok`, `error?` |
| `tool_call` | M8, see §17.12: `session_id`, `call_id`, `name` (`web_search`, `fetch_url`; M16: `propose_task` from a digest job), `args` (raw tool input) |
| `fs_result` | M12, see §20.2: `request_id`, `ok`, `data` (JSON) or `error` |

Hub → worker

| t | payload |
|---|---|
| `start_session` | `session_id`, `cwd`, `profile` (§5), `main_model`, `resume_claude_session_id?`, `permission_mode?`, `extra_args[]?`, `seq_base?`, `from_seq?`, `refresh?`, `tools[]?` (M8: hub tools the mado MCP server offers), `job?` (M16: `{kind, main_model, system}`, §23.6) |
| `user_message` | `session_id`, `text` |
| `interrupt` | `session_id` |
| `approval_response` | `session_id`, `request_id`, `decision` (`allow`,`allow_session`,`deny`), `message?` |
| `stop_session` | `session_id` (SIGINT the turn, then end stdin; never SIGKILL unless asked) |
| `list_sessions` | — |
| `resync` | `session_id`, `from_seq` (hub asks for events it missed) |
| `gc` | M3: `session_ids[]` the hub considers dead (exited, detached, unknown); the worker deletes their spool/token files after `spool_retention` |
| `history_offsets` | M3: `files[{path, offset, size, mtime}]`, `partial?` (after hello with capability `history`, and for a refused frame) |
| `history_rescan` | M3: scan the transcripts now |
| `tool_result` | M8: `session_id`, `call_id`, `content` (text for claude), `is_error?` |
| `fs_inspect` | M12, see §20.2: `request_id`, `kind` (`list_projects`, `inspect`), `path?` (worker capability `fs`) |
| `fs_prepare` | M12, see §20.3: `request_id`, `action` (`mkdir`, `git_init`, `clone`, `worktree`), `path`, `repo?`, `url?`, `branch?`, `base?` |

Recovery semantics (M2):

* The worker sends `sessions` on every connect **and whenever a shim disappears without
  reporting `exited` or `detached`**. The hub reconciles each such list against its sessions
  on that machine that have not ended:
  * listed (live shim): for a router profile, `start_session` with `refresh: true` (profile and
    resolved keys again; nothing is spawned, the worker re-registers the session's persisted
    router token), then `resync{from_seq}` where `from_seq` is the hub's last **contiguous**
    seq (events 1..n all stored), not its maximum;
  * not listed, state `running`/`idle`/`waiting_approval` (or `starting` with a known claude
    session id) and a `claude_session_id`: `start_session` with
    `resume_claude_session_id`, `seq_base` = the hub's highest seq (a resumed shim numbers its
    events after it, so a lost spool never reuses a stored seq) and `from_seq` = the contiguous
    seq. At most one automatic resume per session per 30 s. Pending approvals of that session
    are expired. The hub sets the state to `starting` until the shim reports;
  * not listed, no claude session id yet: marked `exited` with an error;
  * `detached` and `exited` sessions are never resumed.
* `resync` is also sent when an event arrives whose predecessor is missing (at most every 5 s
  per session).
* A restarted daemon attaches to surviving shims in **hold** mode (control frames only) until
  the hub's `resync` says where to start.
* A shim that ends on SIGINT (Ctrl-C in the tmux window) reports `detached`; on SIGHUP or
  SIGTERM (tmux server gone, shutdown) it ends claude with SIGINT and reports nothing, so the
  session stays resumable.

The worker treats hub frames as untrusted input: `cwd` must be inside one of the worker's
allowlisted project roots, `extra_args` must match an allowlist of flags, `profile` upstream URLs
must be http(s).

## 5. Profiles

Stored on the hub (`profiles` table, JSON). Example:

```json
{
  "name": "glm-pair",
  "kind": "router",
  "main": "oc-strong",
  "aliases": {
    "opus":   "oc-worker",
    "sonnet": "oc-worker",
    "haiku":  "oc-small"
  },
  "upstreams": {
    "oc-strong": {"base_url": "http://gpu1:8000", "model": "zai-org/GLM-5.1", "key_ref": "gpu1"},
    "oc-worker": {"base_url": "http://gpu2:8000",  "model": "Qwen/Qwen3-Coder-480B", "key_ref": "gpu2"},
    "oc-small":  {"base_url": "http://gpu2:8001",  "model": "Qwen/Qwen3-8B", "key_ref": "gpu2"}
  }
}
```

Upstream entries may also carry `"context_window"` (1000..10000000) and `"max_output"`
(1..1000000) in tokens. They describe the upstream model to Claude Code: for the session's
**main** upstream the shim's child gets `CLAUDE_CODE_MAX_CONTEXT_TOKENS` and
`CLAUDE_CODE_MAX_OUTPUT_TOKENS` (names verified in the env-vars and model-config docs). One
`claude` process has one value for each, so the values of the other upstreams (aliases used
by subagents) are not applied; they are kept for the editor and for a later per-model
mechanism. Router names never start with `claude-`, which is the case where Claude Code applies
`CLAUDE_CODE_MAX_CONTEXT_TOKENS` on its own. These variables do **not** silence the
`[claude-code:unrecognized_model]` diagnostic (see §13).

**Roles (M4).** Every profile has an optional `roles` block; omitted keys take the defaults:

```toml
[profiles.roles]
enabled = true
implementer = "opus"     # alias used by the "implementer" agent
researcher  = "opus"
simple      = "sonnet"
prompt = ""              # empty = mado's default delegation prompt (roles.DefaultPrompt)
enforce = "edits"        # M5: off | edits (main may not Edit/Write/NotebookEdit) | all (nor Bash)
```

(JSON in the editor: `"roles": {"enabled": true, "implementer": "opus", ...}`.) The worker
turns it into `--agents` with three agents, `implementer`, `researcher` and `simple`, each
with `model` = the alias, a description ending in "Use proactively" and a short prompt (the
researcher may not edit files), plus `--append-system-prompt` with the delegation prompt: the
main model directs and reviews, sends implementation and code changes to `implementer`,
investigation and reading across many files to `researcher`, scaffolding and mechanical
edits to `simple`, and does not write the implementation itself. Aliases are one of
`fable`, `opus`, `sonnet`, `haiku`, `inherit`; for a router profile they resolve through the
profile's `aliases` (the ANTHROPIC_DEFAULT_*_MODEL env), for a subscription profile they are
the real Anthropic aliases. A custom prompt is at most 4000 bytes. The agents carry `tools`
(M5): implementer all (omitted), researcher `Read, Grep, Glob, Bash, WebFetch` (plus
`disallowedTools` for the edit tools), simple `Read, Edit, Write, Bash`. `enforce` adds a
PreToolUse guard hook for the main model (§16). The built-in
`subscription` profile has no block, so it gets the defaults.

**Main model.** A session's main model is the profile's `main` (subscription: `fable`)
unless the user picks one of the profile's other upstream names (subscription: `opus`,
`sonnet`, `haiku`) in the form. `/new` always uses the default.

`key_ref` names an entry in the hub's secrets file. The hub resolves keys and sends the concrete
values inside `start_session`. A profile with `"kind": "subscription"` means "plain claude, no env".
Mixing a subscription Claude orchestrator with open-weight subagents is **not possible**: one
`claude` process has one endpoint. Mixing paid Anthropic API with open-weight *is* possible
(the router can forward `claude-*` names to `https://api.anthropic.com` with an API key).

## 6. Keeping the subscription login intact

Claude Code resolves credentials in `~/.claude` (or `$CLAUDE_CONFIG_DIR`). Open-weight sessions
set `CLAUDE_CONFIG_DIR` to a per-profile directory so their `settings.json`, MCP config and
transcripts never collide with the user's normal Claude Code. Subscription sessions use the
default dir. The implementer must confirm that `-p` mode with `ANTHROPIC_AUTH_TOKEN` set does not
touch the stored OAuth credentials.

## 7. Hub

* `cmd/hub`: HTTP server. Routes:
  `/login`, `/callback` (OIDC), `/logout`, `/` (machines + my sessions), `/s/{id}` (session view),
  `/s/{id}/events` (SSE), `/s/{id}/send` (POST form), `/s/{id}/approve` (POST),
  `/new` (form + POST), `/ws/worker` (worker WebSocket), `/healthz`.
  M2 adds `POST /s/{id}/interrupt`, `GET /audit` (own rows; hub-wide for admins), and the
  admin-only `GET /profiles`, `GET /profiles/new`, `GET /profiles/{name}`, `POST /profiles`,
  `GET|POST /admin/enroll`. `/new <machine> <project> [profile]` typed in the composer is
  handled by `POST /s/{id}/send`: machine name, project path and profile name must each match
  exactly (default profile `subscription`, router profiles start on their `main`).
  M3 adds the admin-only `GET /history`, `GET /history/{id}`, `POST /history/{id}/resume`,
  `POST /history/rescan`; `/new` tokens may be double quoted (`\"` and `\\` inside quotes).
* Profiles from `hub.toml` are seeded at every start (source `config`); profiles created in
  the editor are stored with source `ui`, and since M25 a `hub.toml` profile saved in the
  editor becomes a `ui` override that seeding leaves alone until it is reverted (§28.1). The editor accepts the §5 JSON with
  unknown fields rejected (so no inline `key`), every `key_ref` must exist in the secrets
  file, and router names with `/` are rejected.
* The SSE handler sends every pending approval of the session as its first event(s), so an
  approval that arrived between page render and SSE connect is not lost; the page skips
  cards it already shows.
* Session cookie: random id, httpOnly, Secure, SameSite=Lax; server-side session store in SQLite.
  CSRF token on every POST form. OIDC `state` and `nonce` verified, PKCE S256.
* Tables: `users`, `machines(id,name,token_hash,last_seen,projects_json)`, `profiles`,
  `sessions(id,machine_id,owner_sub,cwd,profile_name,main_model,state,claude_session_id,title,created_at)`,
  `events(session_id,seq,ts,raw)` with PK `(session_id,seq)`, `approvals`, `audit(ts,user,action,detail)`.
* Rendering: the session view renders events server-side into readable blocks (user text,
  assistant text, tool_use name + abbreviated input, tool_result abbreviated, approval cards with
  Allow / Allow for session / Deny buttons). Unknown event types render as a collapsed `<details>`
  with the raw JSON. SSE pushes new blocks as HTML fragments; htmx `sse-swap` appends them.
* User and assistant text is Markdown rendered server-side with goldmark (GFM tables,
  strikethrough, task lists, autolinks, fenced code). Raw HTML is shown escaped, never passed
  through; goldmark's unsafe option stays off. Links get `rel="noopener noreferrer"
  target="_blank"`, tables sit in a `.table-wrap` scroll box, cell alignment uses `align=`
  (a `style=` attribute would break the CSP). Tool results stay in `<pre>`; the shim tty
  stays plain text.
* Mobile first: single column, 16 px gutters, sticky composer at the bottom.
* Audit log: every login, session start, message, approval decision.

## 8. Worker service

* `deploy/mado-worker.service` (systemd **user** unit) + `deploy/install-worker.sh` that enables
  linger, installs the unit, and writes `~/.config/mado/worker.toml`:

```toml
hub_url = "wss://hub.tailnet.ts.net/ws/worker"
machine_name = "gpu1"
token_file = "~/.config/mado/machine.token"   # from `hub enroll`
project_roots = ["~/work", "~/src"]
claude_bin = "~/.local/bin/claude"
tmux_socket = "mado"
router_listen = "127.0.0.1:8742"   # default; fixed so claude children survive a daemon restart
# spool_dir = "~/.local/state/mado/spool"   # default $XDG_STATE_HOME/mado/spool, survives reboot
# tmux_scope = "auto"                        # auto | always | never
```

* The router port is fixed (default `127.0.0.1:8742`): a running claude keeps its
  `ANTHROPIC_BASE_URL` across a daemon restart. A daemon that finds the port busy exits.
* Sockets live in `$XDG_RUNTIME_DIR/mado`; spools and the per-session router token file
  (`<session>.token`, mode 0600, removed when the shim ends) live in `spool_dir` on disk.
* The tmux server is started through `systemd-run --user --scope --collect --unit mado-tmux`
  when the daemon runs under systemd (`INVOCATION_ID` set) and `systemd-run` exists
  (`tmux_scope = "auto"`), so it sits outside the service's cgroup and the unit can use the
  default `KillMode=control-group`. Without systemd, or if `systemd-run` fails, tmux is forked
  directly as in M1.

* On start: connect, send `hello` + `sessions` (discovered from `$XDG_RUNTIME_DIR/mado/*.sock`
  and `tmux -L mado list-windows`). The **hub** decides which sessions should be running
  (§4 recovery) and sends `start_session` with `resume_claude_session_id`; the worker then
  recreates the window as `shim --resume <claude_session_id> --seq-base <n>`.

## 9. Security checklist (must hold from M1)

* Worker opens no listening port except the router on `127.0.0.1`.
* Machine tokens: 32 random bytes, base64url, stored hashed (SHA-256) on the hub; enrollment via a
  one-time code printed by `hub enroll <name>`.
* Router: rejects requests without the exact per-session bearer; rejects `model` names not in the
  profile; strips `Authorization` / `x-api-key` from the inbound request before forwarding.
* Hub validates every form field; cwd is chosen from the worker-reported allowlist, never typed.
* No shell string building anywhere: `exec.Command` with argv slices only. tmux commands too.
* Permission prompts are never auto-allowed except by an explicit "allow for session" decision.
* Secrets file read once at start, never logged; audit log never contains prompts' contents of
  other users.

## 10. Milestones

**M1 (skeleton, this round)**: repo layout, `go.mod`, both binaries build, hub OIDC login works
against Pocket ID (config-driven issuer/client), machine enrollment, worker connects and sends
`hello`, `start_session` creates a tmux window running the shim, shim spawns `claude -p` in
stream-json mode and relays events, session view streams them via SSE, user can send a message,
approval round-trip via the MCP permission tool, router proxies `/v1/messages` with alias mapping
and streaming, subscription profile and router profile both work. `make test` runs unit tests for
router mapping, protocol encoding, token hashing, and the stream-json renderer with recorded
fixtures.

**M2** (done 2026-10-18): reboot recovery (resume), `resync`, "allow for session" memory, `/new` chat command,
interrupt button, profile editor UI, per-user audit view; fixed router port, persisted router
tokens, profile refresh on reconnect, tmux in a systemd user scope, per-upstream
`context_window`/`max_output`; roles (admin via IdP group, users auto-created).

**M3** (done 2026-10-18): release packaging (`make release`, `install-hub.sh`), spool GC,
quoted `/new`, Claude Code history import (worker ships `~/.claude/projects/*/*.jsonl`
read-only, hub renders them on `/history`, "Resume in mado"), phone layout smoke test.
**M4** (done 2026-10-18): role split per profile (`--agents`, delegation prompt), form
defaults and main-model validation, permission mode limited to `default`/`acceptEdits`,
model usage per session (Models panel, list column, `mado session-models`), tool chunks
and subagent nesting in the session view, noisy system events folded.
**M5** (done 2026-10-18): `auto` permission mode back, the role split enforced by a
PreToolUse guard hook (`roles.enforce`), a stricter delegation prompt, agent `tools`,
`mado print-argv`, a real AI& test (§16).
**M6** (done 2026-10-18): chat sessions on the hub, `internal/messages` client, files, JSON API,
Preact SPA (§17.9, §17.10). Deployed to home-mado with four AI& chat models and JPY prices.
**M7** (done 2026-10-18, not deployed yet): `[budgets]` currency and monthly budgets with
refusals, `/usage` with CSV export, `/admin/users`, `/settings`, the admin pages moved into
the SPA (only the login page is server-rendered), approvals stay pending while the machine
is offline, unsent uploads collected after 24 h (§17.11).
**M8** (done 2026-10-18, not deployed yet): `internal/tools` (web_search via SearXNG or
Brave, fetch_url with an SSRF guard, generate_image via the OpenAI Images API), the chat
tool loop with generated images inline, `web_search`/`fetch_url` for code sessions through
the mado MCP server and the new `tool_call`/`tool_result` frames (§17.12).
**M9** (done 2026-10-18, not deployed yet): mado auto mode (§18): `internal/policy` (shell
lexer, deny and allow rules, the `decide` classifier), the hub pipeline with decided cards,
audit rows, usage kind `auto`, the per-session switch, `/new … [mode]`, `[auto]` (§18.6).
**M10** (done 2026-10-19, not deployed yet): local mode (§19): `mado local` runs hub and worker in one
process on one computer, local login replaces the dev login, macOS worker defaults, darwin builds.
M10b: Tailscale header login (§19.5).

**M12** (done 2026-10-19, not deployed yet): concierge (§20): a chat session with mado's API as tools that
prepares directories (mkdir, git init, clone, worktree) and starts sessions from an approved plan card.

**M13** (done 2026-10-19, not deployed yet): answering `AskUserQuestion` from mado (§21): `updated_input` on the
approval response, a question card, never auto-decided.

**M14** (done 2026-10-19, not deployed yet): OpenAI-compatible upstreams (§22): `kind = "openai"` on upstreams and
chat models, translation in `internal/oai`, per-upstream timeouts.

**M15–M20** (decided 2026-10-20): the Inbox (§23): accounts, items, digests and tasks; kinds
`rss`, `push`, `slack`, `imap`, `github`; digests on the Claude subscription through job
sessions on `claude-core` with the hub's chat models as fallback; the X side as a separate
LXC (RSSHub per account) in the homelab repository.

**M21** (done 2026-10-21, not deployed yet): machine removal (§24): `mado hub remove-machine`,
`DELETE /api/machines/{name}`, the admin Machines page.

**M22** (done 2026-10-30, not deployed yet): permission mode for every profile (§25): `auto`
is the default on both profile kinds (mado auto on a router, Claude Code's own on the
subscription), and `POST /api/sessions/{id}/mode` changes the mode of a running session
through claude's `set_permission_mode` control request, with "Restart to apply" as the
fallback.

**M23** (done 2026-10-30, not deployed yet): rate-limit auto-resume (§26): the hub follows
`rate_limit_event`, shows the limit and the windows, and once the window resets sends the
user's messages from the limited time again (or a continue prompt), resuming a stopped
session first; per-session switch, at most 6 attempts in a row.

**M24** (done 2026-10-30, not deployed yet): file paths in the transcript and the file viewer
(§27): `fpath` links from the renderer, the `fs_read` frame, `GET /api/sessions/{id}/file`
and `/file/download`, the panel's File mode.

**M25** (done 2026-10-30, not deployed yet): editing `hub.toml` entries in the browser
(§28): profiles, chat models, prices and the `auto.classifier` and `concierge.model`
settings become DB overrides with Revert to hub.toml, prices move into the DB behind a
cache, profile saves reach live sessions, and the editor marks retiring or unlisted model
ids from the upstream's `/v1/models`.

Later: `pi` adapter, natural-language session start, sharing, `modelOverrides`.

## 11. Open questions for the user

* Where do the vLLM instances for the "strong" and "worker" models run day to day (same host or
  two hosts)? Affects nothing in the design, only the default profile.
* Hostname of the hub on the tailnet, and the Pocket ID issuer URL / client id (needed for M1 config).

## 12. Corrections found during M1 (2026-10-18)

* **tmux must run with `-f /dev/null`.** The user's `~/.tmux.conf` loads tmux-continuum with
  restore-on-start; a fresh `-L mado` server otherwise restores every saved user session into it
  (happened once during the M1 smoke, ~10 `claude -r` copies for 2.5 min). Enforced by a test.
* Routes added: `POST /enroll`, `POST /s/{id}/stop`.
* htmx `sse-swap` needs the vendored SSE extension (`htmx-ext-sse`), not only `htmx.min.js`.
* `-p` inherits the user's permission mode, so the shim always passes `--permission-mode default`;
  `bypassPermissions` is rejected. (Since M2 the form could choose `acceptEdits`, `plan`,
  `dontAsk` or `auto` too; M4 narrows that to `default` and `acceptEdits`, see §15.)
* **Daemon restart breaks router sessions**: tokens and keys are memory-only and `127.0.0.1:0` picks
  a new port. M2: pin `router_listen` to a fixed port, worker re-registers session tokens from the
  shims on start, hub re-pushes profiles and keys on reconnect.
* The tmux server forked by the daemon sits in the service cgroup; M1 uses `KillMode=process`,
  M2 moves it under `systemd-run --user --scope`.
* Per-upstream `CLAUDE_CODE_MAX_CONTEXT_TOKENS` / model overrides are needed to silence
  `unrecognized_model` and set context windows for open-weight names.
* Not yet verified against the real API: `-p --resume <id>`. Not yet done: real Pocket ID login,
  a visual check of the phone layout, approvals that arrive between page render and SSE connect.

## 13. Corrections found during M2 (2026-10-18)

* **DESIGN §8 had the worker deciding what to resume.** The worker does not know which hub
  sessions belong on it; the hub does, so the hub drives resumes from each `sessions` list.
* **tmux death was not "detached".** M1 reported `detached` whenever a shim's socket went away,
  which would have made every reboot or tmux crash unrecoverable (§8 resumes only
  running/idle). M2 separates a deliberate Ctrl-C (detached) from SIGHUP/SIGTERM (lost,
  resumable) and the shim no longer reports `exited` for a claude it ended itself.
* **The spool in `$XDG_RUNTIME_DIR` does not survive a reboot**, and a resumed shim would have
  restarted at seq 1, colliding with stored events (the hub ignores duplicate seqs). Fixed with
  `spool_dir` on disk plus `seq_base`.
* **Resync from the hub's max seq misses gaps**; the hub resyncs from its last contiguous seq.
* `-p --resume <id>` with stream-json works against the real API and keeps the same session id
  (docs/claude-code-facts.md §5). `system/init` of the resumed process appears with the first
  turn, not at start.
* `CLAUDE_CODE_MAX_CONTEXT_TOKENS` does not stop the per-request
  `[claude-code:unrecognized_model]` stderr line (verified with Claude Code 2.1.302). The docs'
  only way to stop it is a `modelOverrides` entry mapping an Anthropic model ID to the router
  name, which also makes Claude Code treat the router model as that Anthropic model (its window,
  capabilities). Not done; the line is harmless and is shown as a stderr block.
* Claude Code retries a router request cut by a daemon restart (`system/api_retry` events), so a
  turn in flight survives a daemon crash; the restarted router holds the reserved token up to
  60 s waiting for the hub's profile refresh, then answers 503.
* The worker unit can go back to the default `KillMode=control-group`.
* Spools and token files are never garbage-collected yet (M3).

## 14. Corrections found during M3 (2026-10-18)

* **Lines keyed by `claude_session_id` collide.** The brief had
  `history_lines(claude_session_id, n, raw)`. A session id is unique only per config dir: the
  same id can exist on two machines, or in `~/.claude` and a profile dir. Lines hang off the
  hub's own file id instead: `history_files(id, machine_id, path, ...)` unique on
  `(machine_id, path)`, `history_lines(file_id, off, raw)` keyed by the line's **byte
  offset**, which makes a resent frame idempotent.
* **The directory name is a lossy cwd.** Claude Code mangles the cwd by replacing every
  non-alphanumeric character with `-` (`/home/me/my-app` and `/home/me/my/app` look the
  same). The `cwd` field of the transcript lines is used; the decoded name is only the
  fallback.
* **"First user message" is usually not the prompt.** The first `user` entries are often
  `isMeta` caveats or `<command-name>` wrappers; those are skipped. A `custom-title` entry
  (`/rename`) wins over the first prompt.
* **Transcripts are mostly not messages.** Besides `user`/`assistant`/`system` there are
  `attachment`, `permission-mode`, `mode`, `custom-title`, `last-prompt`,
  `file-history-snapshot`, `queue-operation`, `bridge-session`. They render as collapsed raw
  JSON, as specified, which is noisy (60 to 70 % of the blocks on the pages opened in the smoke).
* **Real transcripts contain broken lines.** 2 of 271,289 lines on this machine start with
  a run of NUL bytes (1.3 KB and 3.4 KB, crash residue) followed by a valid entry. They are
  stored raw, counted as unparsable, and the history page strips the leading NULs before
  rendering. Lines reach 1.3 MB; frames carry up to 1 MiB of lines (a longer line goes alone),
  lines over 8 MiB are replaced by a `mado_oversize` placeholder.
* **Offsets need a frame of their own.** "The hub tells the worker the last byte offset" is
  `history_offsets`, sent after `hello` with capability `history`. A frame that does not
  start at the stored offset (or at 0 with `reset`) is refused and answered with a partial
  `history_offsets` for that file. The worker detects change by size+mtime, sends from the
  hub's offset, starts over (`reset`) when the file shrank, and holds back a last line
  without a newline. A file rewritten to a *larger* different content is not detected
  (transcripts are append-only).
* **Spool GC needed the worker to report spools.** The `sessions` list only had live shims,
  so the hub could not name dead spools. `sessions.spools` lists ids with files on disk; the
  hub answers `gc` with the ones whose session is exited, detached or unknown on that
  machine (a database error is not "unknown"); the worker deletes only ids that are not
  attached or starting, and only when the newest file is older than `spool_retention`.
* **A resumed transcript's cwd is often below a project.** The hub accepts it for "Resume in
  mado" if it lies inside a listed project (clean absolute path, prefix plus `/`); the
  worker still checks it against its roots. Only transcripts from the user's own
  `~/.claude` can be resumed, since a profile dir has no subscription login. A session
  created this way has its claude id set from the start, which reconcile would read as a
  stalled resume; it gets the same 30 s grace as an automatic resume.
* **History is admin-only.** Machines have no owner, so nobody but admins may read
  transcripts (they contain everything typed on that machine). TODO: machine owners.
* **Size.** All 135 top-level transcripts here (544 MB, 271,289 lines) shipped in 4.1 s over
  loopback and made a 609 MB hub database. The hub host needs disk for every machine's
  history; there is no history retention yet.
* **A single-frame test hid a multi-frame bug.** The first smoke stored only 94 MB: a
  mid-file batch reported an end offset past the next batch's first line, and the hub
  dropped every later frame as malformed (logged, but the worker believed it had shipped
  everything). Tests now check that batches chain and ship a file of several frames
  end to end.
* Subagent transcripts (`<session>/subagents/*.jsonl`) are not imported; the brief's
  `*/*.jsonl` excludes them on purpose.
* `modernc.org/sqlite` is pure Go, so `CGO_ENABLED=0` release binaries are static and need
  nothing on the target.

## 15. Corrections found during M4 (2026-10-18)

* **The two-model split never happened by itself.** A session on `aiand-pair` ran 12
  assistant events (4 API responses), all `deepseek-ai/deepseek-v4-flash`, with zero Agent
  calls. Claude Code's main model uses a second model only when it calls the Agent tool, and
  the subagent's model comes from the agent definition (or the call's `model` parameter);
  aliases in env alone change nothing. mado now passes a role split (§5) as `--agents` plus a
  delegation prompt as `--append-system-prompt`.
* **That session's main model was the user's pick.** The form listed every profile × model
  in one flat select, upstreams sorted by name, nothing preselected; `aiand-dsv4flash` sorts
  before `aiand-glm53`. The user confirmed they picked it. The form now has a profile select and
  a main-model select whose default is the profile's `main` (listed first, marked "(main)"),
  limited to the profile's own upstream names (subscription: fable, opus, sonnet, haiku), with
  the profile's alias and role tables shown read-only next to it. The hub rejects any other
  value. `autocomplete="off"` keeps browsers from restoring an earlier selection.
* **`--model sonnet` and `--permission-mode auto` were form choices too.** The worker log and
  the process list show two subscription sessions 14 s apart, both `--model sonnet`, the
  first with `--permission-mode auto`. The shim and worker passed through what the hub sent
  (§12's "always default" was true only until M2 added the form field); the form offered
  `sonnet` without a default and `plan`, `dontAsk` and `auto` as modes. Now the form offers
  `default` (preselected) and `acceptEdits` only; hub and worker both reject the others. A
  session stored with an older mode resumes as `default`.
* **Turns vs responses.** Claude Code streams one `assistant` event per content block, all
  with the same `message.id`; the 12 events above were 4 responses. The Models panel shows
  both (turns = events, as the list column counts them). `message.usage` on those events
  carries `output_tokens: 0` for the router upstreams (output arrives in message_delta, which
  stream-json does not forward), so the panel also shows `result.modelUsage`, which is
  cumulative per claude process (per turn `usage` is not) and keyed by the name Claude Code
  asked for (router name or Anthropic id). A smaller cumulative value means a new process
  (resume); the earlier total is carried over.
* **Subagent messages need `--forward-subagent-text`.** Without it Claude Code omits the text
  and thinking blocks of foreground subagents. With it, subagent `assistant`/`user` events
  carry `parent_tool_use_id` = the Agent call's id. A real session (fixture
  `internal/usage/testdata/agent_split.spool`, reduced to its structure) ran GLM-5.3 ×15 main
  and deepseek-v4-flash ×14 in a background Explore agent; the main model passed
  `model: "sonnet"` in the Agent call, which overrides the definition (docs: per-invocation
  `model` > definition `model` > `CLAUDE_CODE_SUBAGENT_MODEL` > main model).
* **The stream is mostly noise.** In that session 179 of 243 events were
  `system/thinking_tokens` (estimated_tokens_delta per streamed chunk). Background agents add
  `system/task_started`, `task_progress`, `task_updated`, `task_notification` and
  `background_tasks_changed`. The renderer now turns thinking_tokens into a hidden carrier
  that the page sums into one "thinking 12.3k tokens" counter per turn (shown on the turn's
  result line), task events into one-line status blocks tied to their Agent call, and the
  `[claude-code:unrecognized_model]` stderr line into a block hidden unless "diagnostics" is
  ticked. The shim tty skips thinking_tokens.
* **Tool chunks are built in the browser.** Each block carries `data-tid` (tool id),
  `data-parent` (parent_tool_use_id or a task's tool_use_id), `data-tool`, `data-agent` and
  `data-model`. `app.js` groups a run of tool calls, results, approvals and status lines
  between two texts into one `<details>` ("Bash ×3, Read ×1"), open while the turn runs and
  collapsed at its result; subagent blocks nest in a chunk under their Agent call labelled
  "Explore · deepseek-v4-flash". A tool result goes to its call's chunk even when it arrives
  later (background agents). The same code runs over the server-rendered page and over live
  SSE blocks (a MutationObserver), so there is one implementation; without JavaScript the
  page shows the flat blocks as before.
* **Model usage is cached per session in the hub**, built once from the stored assistant
  and result events (a substring prefilter in SQL) and then fed every inserted event, so
  gap-filling resyncs count too. Re-adding an event is harmless (responses are keyed by
  message id). The session page swaps the panel on an SSE `models` event.
* `make test` runs tmux integration tests on an isolated socket; set `MADO_NO_TMUX_TESTS=1`
  to skip them.
* **The prompt was not enough (found in production after M4).** An `aiand-pair` session
  (GLM-5.3 main) had `implementer`, `researcher` and `simple` in `system/init` and the
  delegation prompt in argv, yet for "dflash2-bench ディレクトリの中身を調べて、何を測っているか要約して"
  it made 0 Agent calls and ran Bash ×2 and Read ×2 itself. The M4 prompt left "small
  decisions" and "short questions" to the main model, which GLM read broadly. §16 enforces
  the split mechanically and makes the prompt's rules countable.


## 16. M5: enforcing the role split (2026-10-18)

The user's rule: the main model directs, reviews and may run commands to verify, but never
writes the code; implementation and investigation go to subagents. M4 relied on the prompt
alone (§15). Details and URLs: docs/claude-code-facts.md, "Learned in M5".

* **Detection.** PreToolUse hook input carries `agent_id` and `agent_type` only when the
  call comes from a subagent (docs, and an experiment with a scripted fake upstream: the main
  model's Write had neither, the implementer's had `agent_type: "implementer"`). The
  permission layer cannot tell them apart: `--disallowedTools` also removes the tool from
  subagents (they inherit the main pool), and the `--permission-prompt-tool` request has no
  agent context. `--agent` would narrow the main thread's tools but replaces Claude Code's
  whole system prompt. So mado uses a hook.
* **`roles.enforce`** = `off` | `edits` (default) | `all`. Unless `off`, the worker adds
  `--settings` with one PreToolUse matcher (`Edit|Write|NotebookEdit`, plus `Bash|PowerShell`
  for `all`) whose command is the mado binary itself in exec form (no shell):
  `mado guard-tool --enforce <mode>`. The guard reads the hook input (at most 4 MiB), stays
  silent for a call with `agent_id` or an unlisted tool, and otherwise prints a `deny` with a
  reason that names the tool and says to delegate with the Agent tool to `implementer`,
  `researcher` or `simple`, then review the report. Unreadable input exits 2 (Claude Code
  blocks the call, so the guard fails closed; subagents then fail too, which shows up at
  once). The hook runs before the permission prompt; a denied call never reaches the
  approval card. Settings lists merge, so the user's own hooks still run. Validated in
  `roles.Validate` (hub config, editor, worker).
* **Limits.** `edits` leaves Bash to the main model (the rule allows checks), so the main
  model could still write a file through the shell; `all` closes that at the cost of the
  quick checks. Read/Grep stay open in both modes; the "at most two file reads" rule is in
  the prompt only.
* **Prompt.** `roles.DefaultPrompt` (146 words) now states countable rules: any question
  about what code, docs or logs say or do (even one file), anything needing more than two
  file reads, and any command beyond a quick check go to `researcher`; every file change
  goes to `implementer`; the main model answers only after reviewing the report. The first
  version ("more than two file reads" alone) let GLM summarise a one-file package itself
  (run 2 below). Agent descriptions use the docs' "Use proactively …" style.
* **Agent tools.** researcher `Read, Grep, Glob, Bash, WebFetch`, simple
  `Read, Edit, Write, Bash`, implementer all. On Linux Glob/Grep are not in Claude Code's
  default pool and are dropped silently (researcher ends up with Read, Bash, WebFetch).
* **`auto` permission mode is back** in the form (third option, "a classifier decides;
  Claude models only") and in hub and worker validation, at the user's request. `dontAsk`,
  `plan` and `bypassPermissions` stay refused; stored `auto` sessions now resume as `auto`.
  Auto needs a supported Claude model; with an open-weight main model Claude Code starts in
  `default` instead (docs). The `/new` chat command keeps `default`.
* **`mado print-argv`** prints the spawn spec `BuildSpawn` makes for a profile JSON (router
  token as a placeholder), so the exact argv can be run by hand.
* **Real test.** Six `claude -p` runs against AI& with the exact argv (env re-pointed at
  `https://api.aiand.com`, aliases to the AI& model ids, `--model zai-org/glm-5.3`, a fresh
  `CLAUDE_CONFIG_DIR`, a scratch copy of the repo as cwd, a scratch approver on the MCP
  socket). With the final prompt every run made an Agent call (researcher for both
  questions); asked to "use the Edit tool yourself", GLM's Edit was denied with the
  delegation message, it delegated (once to simple, once to implementer), the subagent's
  Edit succeeded, and GLM read the file back before answering. Table in
  docs/claude-code-facts.md.

## 17. Chat sessions, SPA, usage accounting, tools (M6–M8, decided 2026-10-18)

**Why.** The family used Open WebUI behind LiteLLM for plain chat; the token accounting
there was a chore. mado already has the login, the users, the keys and the rendering, and it
needs its own web search anyway (Claude Code's `WebSearch` is an Anthropic server-side tool
and does not work through an open-weight endpoint). So plain chat moves into mado and the
old stack goes away. Decided: no RAG (documents go into the context whole, like Open WebUI's
"bypass embedding and retrieval"), no prompt/preset library, no voice; document upload,
image input, image generation and web search are in.

### 17.1 Session kinds

`sessions.kind` is `code` (everything so far; default for old rows) or `chat`. A chat session
has no machine, cwd or profile; it has `model` (a `[[chat.models]]` name), an optional
per-session `system` prompt (≤ 4000 bytes, typed in the new-chat form), and the owner. Chat
runs entirely on the hub: no worker, no tmux, no `claude` process. Both kinds share the
`sessions` row, the `events` table, the owner-only rule, the SSE path and the renderer.

```toml
[chat]
enabled = true
data_dir = "~/.local/share/mado/files"    # uploads and generated images, by sha256
max_file_mb = 20                          # per upload
user_quota_mb = 1024                      # per user, all files
max_tool_rounds = 8                       # tool_use loops per user turn

[[chat.models]]
name = "glm-5.3"                          # internal, [A-Za-z0-9._-]
label = "GLM 5.3"                         # shown to users
base_url = "https://api.aiand.com"
model = "zai-org/glm-5.3"
key_ref = "aiand"
# auth_header = "bearer"                  # x-api-key is inferred for api.anthropic.com
context_window = 200000
max_output = 16000
vision = false                            # image blocks allowed
documents = false                         # native `document` (PDF) blocks; true only for Anthropic
default = true                            # preselected in the form; the first entry otherwise
```

`[[chat.models]]` entries are stored in the DB like profiles (seeded from `hub.toml` at
start, source `config`) and editable by admins on the same editor page as profiles; editing
a `hub.toml` model stores an override that survives restarts (§28.1). A model
is visible to every logged-in user; there is no per-user model ACL (same decision as §2).

### 17.2 `internal/messages`: the Messages API client

One package shared by the router (types only, today it passes bytes through) and the chat
runner: `Request`, `Message`, content blocks (`text`, `image`, `document`, `tool_use`,
`tool_result`), `Tool`, `Usage`, `Response`, and `Stream(ctx, upstream, req) (<-chan Event,
error)` that parses the SSE stream (`message_start`, `content_block_start`,
`content_block_delta` with `text_delta` / `input_json_delta`, `content_block_stop`,
`message_delta` with `stop_reason` and `usage`, `message_stop`, `error`) into typed events
and assembles the final `Response`. Hand-written on `net/http`; no SDK. Auth header rule as in
the router (`authHeader`). Retries: 429 and 5xx, 3 attempts, 1/2/4 s, never on a stream that
already produced a block. Timeouts: 30 s to first byte, 10 min total. `anthropic-version:
2023-06-01` always.

### 17.3 The chat runner (`internal/chat`)

* One runner goroutine per session at a time (`sync.Map` of session id → cancel). A second
  `send` while a turn runs is rejected with 409. `POST /s/{id}/interrupt` cancels the context;
  the partial assistant text produced so far is stored as a normal assistant event with
  `stop_reason: "interrupted"`.
* **Events are stored in the Claude Code stream-json shape**, so `render.Blocks`, the usage
  package, the tool chunking and the Models panel work unchanged: `{"type":"user","message":
  {"role":"user","content":[...]}}`, `{"type":"assistant","message":{id, model, role, content,
  stop_reason, usage}}`, tool results as `{"type":"user","message":{"content":[{"type":
  "tool_result",...}]}}`, and after the loop `{"type":"result","subtype":"success",
  "usage":{...},"total_cost_usd":...,"duration_ms":...}`. `session_id` in every event is the
  mado session id. Attachments appear in the stored user event as mado blocks
  (`{"type":"mado_file","file_id":...,"name":...,"mime":...}`), never as base64; the request
  builder expands them when calling the upstream.
* Streaming to viewers: the SSE path gains `delta` events (`{"seq":<next>,"text":"..."}`,
  not stored) during an assistant text block and the usual `block` event once the message is
  stored. The SPA shows the raw delta text and replaces it with the rendered block.
* **Request building**: `system` = mado's default (`chat.DefaultSystem`: assistant name
  "mado", today's date, "answer in the user's language", Markdown allowed, tool guidance)
  followed by the session's `system`. Messages = every stored user/assistant event in order,
  attachments expanded (§17.4), `max_tokens` = the model's `max_output`, `tools` (M8) when
  the model has any enabled, `stream: true` always.
* **Context trimming**: before each call, if the last known `input_tokens` of this session
  plus an estimate for the new turn (bytes/3) exceeds `context_window − max_output − 2000`,
  drop the oldest user/assistant pairs (whole pairs, tool results with them) until it fits
  and store a `{"type":"system","subtype":"mado_trimmed","dropped":n}` event, rendered as a
  one-line notice. On an upstream 400 that mentions context length, drop two more pairs
  and retry once.
* **Usage**: every assistant response appends a row to `usage` (§17.6) immediately.

### 17.4 Files (uploads, images, PDFs)

* Table `files(id TEXT PK, owner_sub, session_id, name, mime, size, sha256, created_at,
  origin)` with `origin` ∈ `upload | generated`; bytes at `data_dir/<sha256[:2]>/<sha256>`.
  `POST /api/files` (multipart, one file, session id) stores and returns the id;
  `GET /files/{id}` serves it to the owner (and admins) with `Content-Disposition` and
  `X-Content-Type-Options: nosniff`; `DELETE /api/files/{id}` for files not yet referenced.
  Mime is sniffed server-side (`http.DetectContentType` + extension allowlist); the client's
  claim is ignored. Limits from `[chat]`; SVG and HTML are refused (stored XSS risk).
* Expansion when building the request:
  * text-like (`text/*`, JSON, CSV, source code by extension; ≤ 2 MB) → a text block
    `<document name="…">\n…\n</document>` before the user's text;
  * PDF → for a model with `documents = true` a `document` block (base64); otherwise text
    extracted with `pdftotext -layout` when the binary exists, else
    `github.com/ledongthuc/pdf`; figures are lost, which the user accepted;
  * image (`png`, `jpeg`, `gif`, `webp`, ≤ 5 MB) → an `image` block for a model with
    `vision = true`; for other models the send is refused with "this model cannot see
    images" before anything is stored.
  * Expanded text longer than `context_window/2` tokens (bytes/3) is refused with a clear
    message rather than truncated silently.

### 17.5 SPA (`internal/hub/ui`)

* Preact 10 + htm, vendored as ES modules (`preact.module.js`, `hooks.module.js`,
  `htm.module.js`, with their LICENSE files and the exact version in `ui/VENDOR.md`); no
  bundler, no npm, no `package.json`. The browser loads `ui/app.js` as a module. Code stays
  plain JS with JSDoc types. `esbuild` (`go run github.com/evanw/esbuild/cmd/esbuild`) may be
  added later for bundling; nothing in M6 depends on it.
* The shell `ui/index.html` is served for `/`, `/s/{id}`, `/new`, `/usage`, `/settings`;
  client-side routing by `history.pushState`. The server-rendered admin pages (`/profiles`,
  `/history`, `/audit`, `/admin/enroll`) stay as they are in M6, linked from the sidebar, and
  move into the SPA in M7.
* **Layout** (the Claude web app look, see the screenshot in the thread): left sidebar
  with search box, "New" (chat / code), "Recents" (both kinds, newest first, kind icon and
  live state dot), admin links, user menu with logout; centre column transcript + composer
  (attach button, model picker for chat, permission mode for code, Enter sends / Shift+Enter
  newline); right panel, collapsible: for code sessions machine, cwd, profile, Models panel,
  pending approvals; for chat sessions model, tokens in/out this session, cost this session
  and this month, attachments list. ≤ 900 px: sidebar becomes a drawer, the right panel a
  sheet behind an "i" button; 16 px gutters, no horizontal scroll.
  Light and dark via `prefers-color-scheme`; colours as CSS tokens on `:root`.
* **JSON API** (all under `/api`, cookie auth, `Origin` checked on every non-GET, CSRF via
  the `X-CSRF-Token` header whose value `GET /api/me` returns; `HX-*` handling goes away):
  `GET /api/me`, `GET /api/sessions`, `POST /api/sessions` (`{kind, machine, cwd, profile,
  main_model, permission_mode}` or `{kind:"chat", model, system}`), `GET /api/sessions/{id}`,
  `PATCH /api/sessions/{id}` (`title`), `DELETE /api/sessions/{id}` (chat always; code when
  stopped/error; removes events, approvals, files), `GET /api/sessions/{id}/blocks?after=`
  (rendered blocks as JSON: `{seq, kind, label, html, text, detail, is_error, subagent,
  parent, tool_id, agent, model, noise}`; text/assistant `html` is goldmark output, the rest
  escaped by the client), `POST /api/sessions/{id}/send` (`{text, file_ids}`), `.../approve`,
  `.../interrupt`, `.../stop`, `GET /api/machines` (since M21 also the admin-only
  `DELETE /api/machines/{name}`, §24), `GET /api/chat/models`. The SSE route
  `/s/{id}/events` sends `block` with the same JSON (not HTML), `delta`, `state`,
  `approval`. The old form routes are removed once the SPA covers them (M6 removes `/new`
  POST, `/s/{id}/send` form, index and session templates; `blocks.html` logic moves to
  `blocks.go` returning the JSON shape).
* CSP stays as strict as today: `script-src 'self'`, no inline scripts, no `style=`
  attributes; the SPA sets classes only. Markdown stays server-rendered (one sanitiser, no
  client Markdown library). Code blocks get a copy button; syntax highlighting is a later
  item (vendored highlight.js), not M6.
* Composer features: paste/drag of files and images, `/new <machine> <project>` still works
  in a code session, `Esc` interrupts.

### 17.6 Usage and budgets (M7)

* Table `usage(id INTEGER PK, ts, session_id, kind, owner_sub, model, input, cache_read,
  cache_write, output, cost_usd, message_id UNIQUE)`. Chat rows come from the runner; code
  rows from every stored `assistant` event with `message.usage` (deduped by `message.id`),
  cost 0 when the session's profile is `subscription`. Since §17.13 those are provisional
  and each `result` replaces them with exact per-model rows.
* Prices: `[[prices]] model = "zai-org/glm-5.3"  input = 0.6  output = 2.2  cache_read = 0.1
  cache_write = 0.75` (USD per million; matched on the upstream's `message.model`, prefix
  match allowed with a trailing `*`). Unknown model → cost 0 and a one-time log line.
* Budgets: `[budgets] default_monthly_usd = 10.0`; `users.monthly_budget_usd` (NULL = default)
  editable by admins at `/admin/users` (also: admin flag view, last login, totals). When a
  user's month-to-date cost ≥ budget: chat `send` and chat session creation are refused with
  the amount; code sessions on router profiles likewise; subscription sessions are exempt.
  Months are UTC calendar months.
* `/usage`: own rows by month and model (tokens, cost); admins see every user with a user
  filter. CSV export link.

### 17.7 Tools (M8)

* Tool loop in the runner: `web_search(query)`, `fetch_url(url)`, `generate_image(prompt,
  size?)`. Each tool call is stored as the stream-json `tool_use` / `tool_result` pair so the
  session view shows it as a chunk; `generate_image` additionally stores
  `{"type":"mado_file", ...}` so the image renders inline (`<img src="/files/{id}">`), while
  the model's `tool_result` only says "image <id> generated and shown to the user".
* Backends (`[chat.tools]`): search `kind = "searxng"` (`base_url`, `GET /search?q=&format=
  json`, top 8: title, url, snippet) or `kind = "brave"` (`key_ref`, `X-Subscription-Token`);
  fetch: 10 s, ≤ 2 MB, `go-readability` → text, ≤ `fetch_max_chars` (default 20000);
  images `kind = "openai"` (`base_url`, `model` e.g. `gpt-image-1`, `key_ref`, Images API
  `POST /v1/images/generations`, `b64_json`), `kind = "comfyui"` later.
* SSRF: `fetch_url` and `searxng` refuse loopback, link-local, RFC 1918 and CGNAT
  (100.64/10, the tailnet) targets unless `allow_private = true`; redirects re-checked; no
  `file:`.
* Code sessions get the same search: the worker's `mado` MCP server gains `web_search` and
  `fetch_url`; the MCP process asks the shim over the existing unix socket, the shim relays
  `{type:"tool_call", session_id, call_id, name, args}` over the WebSocket, the hub answers
  `tool_result`. No key or search backend ever reaches the worker.

### 17.8 Order

M6: §17.2, §17.3, §17.4, §17.5 with usage rows recorded (§17.6 table only). M7: §17.6 pages
and budgets, admin pages into the SPA. M8: §17.7. Each milestone ends with `make check`,
a deploy to home-mado and a note in this file.

### 17.9 M6 backend (2026-10-18)

Done: `internal/messages`, `internal/chat`, `internal/files`, the hub's `chat_models`,
`files` and `usage` tables, `sessions.kind/model/system`, the `/api` routes and
`?format=json` on the SSE route. The SPA is the next task. Deviations and choices:

* **AI& facts** (`docs/aiand-facts.md`) shaped the client: the final `Usage` comes from
  `message_delta`, `message_start` only fills fields the delta lacks; `documents` defaults
  to `vision`; `max_output` defaults to 32000 (`context_window` to 131072) and trimming
  never lowers `max_tokens`; a context overflow is a 400 starting `prompt is too long`
  (also accepted: "context length", "context window"). Error bodies in the OpenAI shape
  are parsed too.
* **Retries**: three attempts with 1 s and 2 s between them (the "1/2/4 s" of §17.2 has
  one wait too many for three attempts). Transport errors and in-stream
  `overloaded_error`/`api_error`/`rate_limit_error` before the first content block are
  retried as well; events before the first block are held back so a retry never shows
  twice. 30 s to first byte means response headers.
* **Trimming is persistent**: `system/mado_trimmed` carries `before_seq`, and the
  request builder skips user/assistant events below the latest one, so a trim is stored
  once instead of being recomputed and re-announced every turn. The estimate is the last
  response's input+output plus bytes/3 of what came after; dropping a turn subtracts
  that turn's bytes/3; images count 1600 tokens, native PDFs 3000. The current turn is
  never dropped.
* **Turn endings**: an interrupt stores the partial answer (`stop_reason:
  "interrupted"`) and a `result` with subtype `interrupted`; an upstream failure stores
  any partial answer (`stop_reason: "error"`), a `mado_error` event and a `result` with
  `error_during_execution`, `is_error: true`. Chat `result` events carry `usage` and
  `total_cost_usd` but no `modelUsage` (the usage package treats that as cumulative per
  claude process). `thinking` blocks are stored and render collapsed; replay drops them.
* **Files**: an upload needs an existing chat session (`session_id`), so the SPA creates
  the session first. Text ≤ 2 MB and images ≤ 5 MB are enforced at upload, not only at
  send; at most 20 files per message; the quota counts file rows (a deduplicated blob
  still counts per row). `/files/{id}` is inline for images and PDFs, an attachment
  otherwise; `.xml` is stored as `text/plain`.
* **Chat models**: config rows that left `hub.toml` are deleted at start (profiles keep
  theirs); a `ui` model whose name `hub.toml` takes becomes a config model.
* **Usage**: `input` excludes the cache parts; the merged row (largest of each count per
  `message_id`) has its cost recomputed. `[[prices]]` is in (prefix `*` match, unknown
  model 0 and logged once). Usage rows survive a session delete.
* **API additions**: `GET /api/profiles` (the code form needs the main-model choices);
  list endpoints wrap their arrays (`{"sessions": [...]}`); `/blocks` pages by 1000
  events and returns `{blocks, last, more, approvals}`; blocks also carry `delta`,
  `file_id`, `mime`. In JSON mode an SSE `block` is the JSON array of one event's blocks
  (`id` = seq), approvals come as `approval` events, `state` and `models` are JSON.
  Non-GET API calls need the `Origin` header (not only a matching one when present).
  `POST /api/sessions` → 201 `{id}`; chat `send` → 202 `{seq}`, 409 while a turn runs;
  `/new …` sent in a code session → 201 `{id}`. A code session can be deleted when
  exited or detached.
* **Kept for now**: the form routes and templates (§17.5 removes them in M6) stay until
  the SPA replaces them; the server session page and `POST /s/{id}/send|interrupt` work
  for chat sessions. The hub resets chat sessions left `running` at start and interrupts
  running turns on shutdown. An empty chat title becomes the first message (60 runes).
* **Fixed on the way**: events whose `message` is a string (the shim's `mado_error`)
  rendered as "unparsable"; `make check` (vet + test) did not exist.

### 17.10 M6 SPA (2026-10-18)

Done: `internal/hub/ui` (shell, `app.js`, `api.js`, `sse.js`, `components/`, `views/`,
`app.css`) with Preact 10.29.8, preact/hooks and htm 3.1.1 vendored (`ui/vendor/VENDOR.md`).
The htmx pages, their form routes, the HTML SSE mode and htmx itself are gone. Deviations
and choices:

* **Version in the path, not `?v=`.** Files are served at `/ui/<hash>/…` (12 hex of a
  sha256 over every embedded file), `immutable` when the hash is current and `no-cache`
  otherwise. A query string on the entry module does not reach the modules it imports by
  relative URL; a path prefix does. `index.html` is served only as the shell, never under
  `/ui/`, and directories are 404.
* **No import map.** `hooks.module.js` imports `./preact.module.js` instead of `preact`
  (an inline `<script type="importmap">` would need a CSP hash). The source-map comments
  were dropped. `TestUIModulesHaveNoBareImports` checks every specifier is relative with a
  `.js` suffix and that no module sets `style` or `innerHTML`; Markdown goes in through
  Preact's `dangerouslySetInnerHTML` only (hub-sanitized goldmark output). The CSP is
  unchanged.
* **`/` is the New page** (Chat tab first when chat is enabled) rather than a separate
  home; `/new?kind=code&machine=<id>&cwd=<dir>` preselects a project ("New session here"
  in a code session's panel replaces the old page's link).
* **API addition**: `PATCH /api/sessions/{id}` takes `model` for chat sessions (the
  composer's model picker; the next turn uses it). The permission mode of a code session is
  shown in the composer but cannot change mid-session.
* **SSE**: always JSON; `?format=json` is accepted and ignored. A decision taken by a
  remembered "allow for session" rule is an `approval` event too. Approvals carry no seq,
  so the SPA places a card after the last block it had when the card arrived (pending
  cards from `/blocks` go after the last block); decided cards from earlier page loads
  are not shown, as before.
* **Tool chunks** are rebuilt from the block list on every change (`components/chunks.js`,
  a pure function); a viewer's own open/closed choice per chunk survives re-renders. The
  server-rendered history page keeps the old DOM implementation in `static/log.js`, so
  the grouping rules live in two places until history moves into the SPA (M7).
* **Composer**: sending reads the textarea itself, because keys can arrive before Preact
  re-renders (found with Marionette: "text⏎" sent nothing). Enter during IME composition
  does not send. Files upload on attach (the session exists already), `×` deletes an
  unsent upload.
* **Recents** refresh on every `state` event of the open session and every 15 s; there is
  no hub-wide stream.
* **Admin pages** get a small header nav (Audit; Profiles, History, Enroll for admins),
  since the old index page that linked them is gone. `blocks.html` remains for the
  history page only.
* **Tests** moved from the HTML pages to the JSON API (`apiDo`, `apiCSRF`);
  `TestUIShellAndFiles` covers the shell routes, caching, 404s and the removed routes.
* **Screenshots.** Firefox here is a snap (private `/tmp`), and `--headless --screenshot`
  fires at the load event, before the SPA has fetched anything. The smoke run used the
  same binary headless over Marionette (profile under `~/snap/firefox/common`), the dev
  login, a scratch hub with a fake Messages API upstream and a code session seeded into
  the DB. Headless windows do not go below 500 px and ignore `devPixelsPerPx`; 390 px was
  emulated with full zoom 1.5 in a 585 px window. Checked at 1440 px and 390 px, light and
  dark: layout, drawer, sheet, no horizontal scroll (`scrollWidth == clientWidth`),
  upload chip → send → file block, streaming draft then the stored answer, approval error
  toast, rename, search, new-chat flow, chunk toggle, diagnostics toggle.
* **Open**: deciding an approval while the machine is offline records the decision and
  then answers 503 (pre-existing in `decideApproval`), so the card vanishes on reload
  although claude never got the answer. Syntax highlighting is still a later item. The
  JS has no test in `make check` beyond the static checks above (the chunk builder was
  exercised with gjs by hand).

### 17.11 M7 (2026-10-18)

Done: §17.6 budgets and pages, the admin pages in the SPA, the two M6 open issues, and the
settings page. Deviations and choices:

* **Currency, not USD.** `[budgets] currency` (any 3-letter code, default `USD`) names the
  unit of `[[prices]]` and of every budget; mado converts nothing. The keys are
  `default_monthly` and `users.monthly_budget` (no `_usd`; M6 had shipped neither, so there
  is no alias). The usage column stays `cost_usd` in SQLite (renaming a column is not worth a
  migration); in Go and JSON it is `cost`. Formatting is one rule on both sides
  (`usage.FormatMoney`, `fmtMoney` in `ui/util.js`): `¥1,234` for JPY (rounded, no decimals),
  `$1.23` for USD, `EUR 1.23` otherwise; the SPA shows a non-zero amount below the last digit
  as `<¥1` / `<$0.01`. Chat `result` events now carry `mado_currency`, so their result line
  shows `¥2` instead of `$1.8000`; chat results stored before M7 still show dollars, and code
  sessions keep showing claude's own `total_cost_usd` estimate in dollars.
* **No limit by default.** `default_monthly` omitted means no budget, so upgrading a hub never
  starts refusing work by itself; `0` is a real budget (everything paid refused). A user's
  own budget (set or cleared on `/admin/users`, audited as `budget_set`) wins over the
  default. There is no per-user "unlimited" other than a large number.
* **Refusals** answer HTTP 402 with `monthly budget reached: ¥2 used of ¥1 this month; it
  resets on 2026-11-01 (UTC)`. Checked before anything is stored or sent: chat send and
  creation, `POST /api/sessions` and `/new …` on a router profile, and composer messages
  into a running router session. Not checked: interrupts, approvals, automatic resumes,
  "Resume in mado" (subscription). A turn that starts under the budget runs to the end even
  if it crosses it; the month-to-date sum is read per request (one indexed query).
* **Months** are string ranges `[YYYY-MM, next)` over the RFC 3339 `ts`, which also fixes
  M6's `ts >= monthStart` comparison missing rows in the first second of a month.
* **API** (all JSON, `requireAPI`; the `admin` ones answer 403 to users): `GET /api/usage?
  month=&user=` (`months`, `models` by model and kind, `total`, `budget` for one user in
  the current month; admins also get `people` and, without `user`, `users`), `GET
  /api/usage.csv` (the month's rows: ts, user, email, kind, session, model, message id,
  tokens, cost, currency; cells starting with `= + - @` get a `'`), `GET /api/me` now has
  `currency` and `budget`; `GET /api/admin/users`, `PATCH /api/admin/users/{sub}`
  (`{"monthly_budget": n|null}` only), `GET /api/audit[?mine=1]`, `GET /api/admin/profiles`
  (profiles and chat models with their editor JSON, key_ref names, templates), `POST
  /api/admin/profiles` and `/api/admin/chat-models` (`{json, original}` → 200 `{name}` or 422
  with the validator's message), `GET /api/admin/history[?q=]`, `GET
  /api/admin/history/{id}?after=|tail=1` (blocks in the session shape, `seq` = byte offset),
  `POST /api/admin/history/{id}/resume` → 201 `{id}`, `POST /api/admin/history/rescan
  {machine}`, `POST /api/admin/enroll {name}` → `{code, command, minutes}`.
* **Server-rendered pages are gone** except `login.html`, which now stands alone on the
  SPA stylesheet (`/ui/<version>/app.css`); `layout.html`, `blocks.html`, the other
  templates, `static/` (`log.js`, the old `app.css`) and their form routes were deleted. The
  shell is served for every SPA route (`shellRoutes` in `ui.go`) to any logged-in user; an
  admin page opened by a user shows the API's "admins only". History renders through the
  session view's `Transcript`, so the chunk rules live in `components/chunks.js` only.
  Profiles and chat models are edited on one page (`/profiles?edit=profile:<name>`,
  `model:new`, …); there is still no delete.
* **Approvals while offline** (§17.10 open issue): `decideApproval` refuses with 503 before
  recording when the machine is offline, claims the approval, and reopens it if the send
  fails; the "allow for session" rule is stored only after delivery.
* **Unsent uploads**: the panel lists only files a stored event names (`"file_id":"…"`);
  an hourly housekeeping goroutine (first pass a minute after start, stopped by `Close`)
  deletes uploads no event names after 24 h, and their blobs when no other row uses them.
* **Phones**: on ≤ 640 px the user list is one card per user (`data-label` + `::before`);
  usage tables put cost second and scroll inside their box.
* **Screenshots** (as §17.10: headless Firefox over Marionette, scratch hub with the dev
  login, seeded users, three months of usage, a history transcript; JPY with
  `default_monthly = 1500`): `/usage` (everyone, one user over budget), `/admin/users`
  (list, inline budget edit), `/profiles` (list, read-only config profile, validation
  error), `/history`, `/history/{id}`, `/settings`, `/audit`, `/admin/enroll`, the chat
  panel; 1440 px and 390 px, `/usage` and `/admin/users` also dark. No horizontal scroll at
  either width. Two fixes came from it: the user cards, and htm dropping the space after an
  expression that ends a line (write the text on the same line).
* **Open**: no profile or chat model delete; budgets do not stop a turn already running;
  the CSV has no per-day aggregation; history is still admin-only (machines have no owner).

### 17.12 M8 (2026-10-18)

Done: §17.7. `internal/tools`, the runner's tool loop, the code-session relay, the SPA
parts. Not deployed. Deviations and choices:

* **Config.** `[chat.tools]` has `fetch` (default true: fetch_url needs no backend),
  `fetch_max_chars` (20000) and `allow_private`; `[chat.tools.search]` has `kind`
  (`searxng` | `brave`), `base_url`, `key_ref` and its own `allow_private`, because a
  SearXNG usually sits on the LAN or the tailnet while fetch_url should still refuse
  those; `[chat.tools.image]` has `kind` (`none` default | `openai`), `base_url`
  (default `https://api.openai.com`), `model` (default `gpt-image-1`), `key_ref`, `size`.
  `LoadConfig` checks the shape, `New` checks every `key_ref` against the secrets file.
  Every chat model gets every enabled tool; there is no per-model switch.
* **SSRF guard.** The address is checked when the connection is dialled, after DNS
  resolution, so every redirect (at most 5) and a DNS answer that changes between check
  and use are covered; literal hosts and the scheme (http/https only, no userinfo) are
  also checked up front and on each redirect. No proxy. Refused besides the §17.7 ranges:
  0/8, multicast, TEST-NETs, 198.18/15, 240/4, documentation IPv6, and IPv4-mapped and
  NAT64 (64:ff9b::/96) forms of refused addresses.
* **fetch_url.** The body is decoded by its charset (Shift_JIS pages work), HTML goes
  through go-readability and is rendered as text with headings (`## `) and list items
  (`- `); a page readability cannot parse falls back to the whole body. JSON, XML and
  `text/*` pass through; anything else (PDF, images) is a tool error. The result starts
  with `Title:` and the final `URL:`.
* **Images.** gpt-image models get `output_format: "png"` and no `response_format` (the
  API refuses it for them, per the OpenAI spec); dall-e models get `response_format:
  "b64_json"`. The image is stored as a file of the session owner (`origin = generated`,
  `image-YYYYMMDD-HHMMSS.png`, the user quota applies) and as a `mado_file` block with
  `"origin":"generated"` in the same user event, after the tool_results. Replay skips it,
  so the model never gets the image back. gpt-image's `usage` token counts become a usage
  row (`message_id = img_<file id>`, model = the image model) priced through `[[prices]]`;
  dall-e reports none and costs 0.
* **Tool loop.** `max_tool_rounds` counts rounds of tool execution. When an answer asks
  for tools after the last allowed round, its calls get `is_error` results ("Not run: the
  limit of N tool rounds per message was reached"), a `system/mado_tool_limit` notice is
  stored and the turn ends; the user continues with a new message. Calls of one answer run
  in order (4 minute backstop each); a failure becomes `Error: …` with `is_error`. An
  interrupt answers the running and remaining calls with "Interrupted by the user." so the
  stored conversation stays valid. Replay also drops `tool_use` blocks without a result
  and results without a call (an interrupted stream, a trim), which the API would refuse.
  The guidance sentence names the offered tools and asks for the URLs used.
* **Code sessions.** Frames use `t` like every other frame. `start_session.tools` lists
  the hub's code tools (not on `refresh`, which spawns nothing). The worker keeps only
  names it knows, adds `--tools web_search,fetch_url` to the approve-tool argv, passes
  `--allowedTools mcp__mado__web_search` (a query to the hub's search backend needs no
  prompt, like `WebSearch`; `fetch_url` asks, like `WebFetch`) and gives the researcher
  agent both MCP names (`roles.ArgsWith`; the guard hook and enforcement are unchanged).
  The MCP process connects to the shim socket with role `tool` and sends a local
  `tool_call` (`call_id`, `tool_name`, `input`); the shim queues it as a control frame for
  the attached daemon (none attached: an immediate error result; calls are not replayed
  to a daemon that attaches later), the daemon sends `tool_call` to the hub (not
  connected: an error result) and relays `tool_result` back. The MCP server returns an
  error result after 30 s; the hub gives a tool 25 s. The hub runs calls in a goroutine,
  only for sessions of the sending machine and only `web_search`/`fetch_url`; no budget
  applies. Compatibility: an old worker ignores `tools` (its claude never sees them), an
  old hub sends none; either side logs and skips the other's unknown frames.
* **API and UI.** `GET /api/sessions/{id}` has `tools` for chat sessions (the panel's
  "tools" row); blocks carry `origin`. The SPA shows a generated image between the tool
  chunks (never folded into one) at up to 512 px with a Download link, and renders
  web_search results (`web_search` and `mcp__mado__web_search`) as a numbered link list
  with the raw text behind "as text"; links get `rel="noopener noreferrer nofollow"` and
  only http(s) URLs match.
* **Not audited.** Tool calls get no audit row (neither do chat messages' contents).
* **Screenshots** (scratch hub with the dev login, a Python fake serving the Messages API
  that calls web_search, fetch_url and generate_image in turn, SearXNG JSON and the
  Images API): a chat with the collapsed chunk and the inline image, the chunk opened
  (search results as links, the fetched text), dark, and 390 px. No horizontal scroll;
  the image loaded (512 px natural width); no JS errors.
* **Open**: `kind = "comfyui"`; fetch_url does not read PDFs; a vision model cannot look
  at a generated image in a later turn; a chat model could put conversation text into a
  fetch_url or search query (prompt injection from a fetched page; chat sessions hold no
  keys, but the conversation itself could leak); tool calls of a code session in flight
  when the hub or daemon restarts get the 30 s timeout error.

### 17.13 Code usage from `result.modelUsage` (2026-10-19)

Found on a running deepseek-v4-pro session: mado had recorded ¥17,789 where Claude Code's
own counts give about ¥7,420, cache reads 183k where they were 119M, and output 0 in 2101
of 2115 rows. Claude Code's `assistant` events carry the usage of `message_start` and never
the final one. Behind AI& that usage is an estimate of the whole prompt with no cache split
and output 0 (the real counts come only in `message_delta`, docs/aiand-facts.md); behind
Anthropic the input is right but the output is still the start value. The exact counts
reach stream-json only in `result`: `usage` covers the main thread of the turn, and
`modelUsage` is cumulative per claude process and per requested model name, subagents and
side calls included.

* `usage` gets `seq` (the event a code row came from) and `provisional`. An `assistant`
  event still writes a provisional row per `message.id` (live figures within a turn).
* On a `result`, the hub takes the difference of its `modelUsage` against the session's
  previous `result` in the same process, writes one exact row per model
  (`message_id = turn:<session>:<seq>:<name>`, model = the upstream's model, so prices
  match as before) and deletes the provisional rows of events between the two, in one
  transaction. A process that died before its `result` keeps its provisional rows.
* The shim writes `{"type":"mado_spawn","pid":…}` when it starts claude, so a resumed
  process counts from zero. Without the marker (an older shim) a smaller cumulative count
  is taken as a new process. A `result` whose next `result` of the same process is already
  stored (a resync filled a gap) adds nothing; that one counted it. `usage.Acc` (Models
  panel) also resets on the marker. The render ignores it.
* `RecordUsage` merges and recomputes the cost in one statement (it was two).
* Rows written before this change are not repaired: they over-count input as uncached and
  carry no output.

## 18. mado auto mode: a hub-side approval policy for any model (M9, decided 2026-10-18)

**Why.** Claude Code's `auto` permission mode needs an Anthropic classifier model; on a router
profile Claude Code silently starts in `default` (docs, §16). The user wants sessions on
open-weight models to run unattended. mado owns the permission prompt tool
(`mcp__mado__approve`), so the hub can decide instead of the user. Claude Code's own
`auto` stays as it is for subscription profiles.

### 18.1 Semantics

* Permission mode `auto` on a **router** profile means "mado auto": the hub starts claude
  with `--permission-mode acceptEdits` (edits inside cwd pass without a prompt; they are
  reversible through git) and answers every remaining approval request itself through the
  policy below. The session keeps `permission_mode = auto` in the DB and the UI shows
  "auto (mado decides)". On resume the same mapping applies. One function
  (`claudeMode(session, profile)`) owns the mapping. On a **subscription** profile `auto`
  stays Claude Code's own auto mode ("auto (claude decides)"; claude gets
  `--permission-mode auto`, the hub policy stays off, and what claude's auto mode escalates
  reaches the user through the permission prompt tool as before). M22 kept this split and
  made `auto` the default on both kinds (§25.1).
* `PATCH /api/sessions/{id}` with `{"auto": false}` (or `true`) flips the hub between
  policy and ask mode while claude keeps running (claude stays in `acceptEdits`); the right
  panel gets the switch. It exists for every router-profile code session; on one in
  `default` or `acceptEdits` it turns the session into mado auto mid-session (§18.6).
  Since M22 the mode itself can change mid-session on any profile, and claude is told
  (§25.3).
* `/new <machine> <project> [profile] [mode]`: the fourth token may be `default`,
  `acceptEdits` or `auto`.
* The roles guard hook (§16) is unaffected: it runs inside claude before any prompt.

### 18.2 The policy, in order (`internal/policy`)

1. **Session rule** ("allow for session", existing) → allow.
2. **Deny rules** (hard, never overridden by the classifier). Defaults, matched as regexes
   on the Bash command or on the file path of the other tools, plus `[auto] deny = [...]`
   from `hub.toml`: `sudo`, `su `, `rm -rf` on `/`, `~`, `..`, `*`, `$HOME`; `git push`
   with `--force`/`-f`/`+`; `git reset --hard`; `git clean -fd`; `git checkout -- .`;
   `curl|wget … | sh|bash`; `chmod -R 777`; `mkfs`, `dd … of=/dev`; `:(){`; `shutdown`,
   `reboot`, `systemctl … (stop|disable|mask)`; `kill -9`, `pkill`, `killall`;
   `tmux kill-server`; `claude /login`, `claude login`; any path containing
   `.claude/settings.json`, `.config/mado/`, `.ssh/`, `secrets.toml`, `*.token`, `.env`,
   `id_rsa`, `id_ed25519`; writes under `/etc`, `/usr`, `/boot`, `/var/lib`. A deny
   answers the prompt with `deny` and the message "mado policy denied this: <rule>; ask the
   user in your reply if it is needed", so the model can explain and continue.
3. **Allow rules** (deterministic, no model call). Defaults plus `[auto] allow = [...]`:
   `Read`, `Glob`, `Grep`, `WebFetch`, `mcp__mado__web_search`, `TodoWrite`, `Task`/`Agent`;
   Bash whose every command (split on `;`, `&&`, `||`, `|`) matches one of `git (status|diff|
   log|show|branch|add|commit|stash list|rev-parse|remote -v)`, `ls|cat|head|tail|wc|pwd|
   echo|which|env|date|stat|file|du|df|tree`, `grep|rg|find|fd|awk|sed -n`, `go (build|test|
   vet|run|fmt|mod tidy|generate)`, `make( [a-z-]+)?`, `python3? (-I )?-m pytest…`, `pytest`,
   `cargo (build|test|check|fmt|clippy)`, `npm (test|run [a-z:-]+)`, `mkdir -p`, `cp`, `mv`,
   `touch`, `diff`, and — only when every absolute path in the command lies under one of the
   machine's project roots or `/tmp`. A command containing `>` or `>>` redirection to a
   path outside those roots is not allowed by rule.
4. **Classifier** (`[auto] classifier = "<[[chat.models]] name>"`; omitted = skip this step).
   The hub calls the model through `internal/messages` with a forced tool `decide`
   (`{"decision": "allow"|"deny"|"ask", "reason": string}`; every AI& model supports
   `tool_choice`) and this context: the policy summary, cwd and project roots, the session's
   last user message, the last 20 rendered blocks as plain text (abbreviated), the tool name
   and the full input. System prompt: it approves routine development work inside the
   project, denies anything destructive, irreversible, network-exfiltrating or touching
   secrets and system config, and answers `ask` when unsure. Timeout 20 s, one retry; any
   failure → `ask`. There is no per-turn call cap. Tokens are recorded as usage rows (`kind = auto`, the session owner pays).
5. **Ask the user** as today.

### 18.3 Visibility

* Every automatic decision is stored as a decided approval with `decided_by` =
  `rule:<name>` or `classifier` and the reason, published as a decided card (collapsed,
  "auto-allowed by mado: git status" / "auto-denied: sudo") and written to the audit log.
* The right panel of an auto session shows "Auto decisions: N allowed, M denied, K asked"
  for the session and the switch from §18.1. The transcript's diagnostics toggle hides the
  auto-allowed cards; denied and asked ones always show.

### 18.4 Config

```toml
[auto]
classifier = "deepseek-v4-flash"   # a [[chat.models]] name; omit = rules only
# allow = ['^git fetch( |$)']      # regexes added to the defaults (RE2, matched on the command)
# deny = ['docker ']
```

### 18.5 Order

M9 = all of §18, tests for the rule tables (every default has a positive and a negative
case), the pipeline order, the classifier with a fake upstream (allow, deny, ask, timeout,
malformed answer), the mid-session switch, and the audit rows. Then a real run on
home-mado with `aiand-pair`, `auto`, and the classifier on `deepseek-v4-flash`.

### 18.6 M9 (2026-10-18)

Built as specified, in `internal/policy` (`shell.go`, `rules.go`, `allow.go`,
`classifier.go`) and `internal/hub/auto.go`. Tests: every default rule with a positive and a
negative case, the brief's tricky cases, the classifier against a fake upstream (allow, deny,
ask, timeout, malformed answer, upstream 500, retry), and in the hub the pipeline order,
the switch, the resume mapping, `/new … auto`, audit and usage rows. Not deployed; the real
run on home-mado (§18.5) is still to do.

* **Lexer.** `policy.Split` is a small POSIX shell lexer: it splits on `;`, `&&`, `||`, `|`,
  `|&`, `&` and newlines, keeps quoted text together (`echo "a && b"` is one command), takes
  redirections apart (`2>&1` is a descriptor, not a file), skips here-document bodies, and
  parses the inside of `$(…)`, backticks, `(…)` and `<(…)` as commands of their own. Those
  constructs, brace groups, unterminated quotes and a line ending in an operator make the
  line *opaque*: the deny rules still see every inner command, but nothing opaque is allowed
  by rule.
* **Deny rules** run on each command after leading assignments and wrappers (`env`, `nohup`,
  `time`, `timeout`, `nice`, `xargs`, `exec`, `command`, `stdbuf`, `watch`, shell keywords)
  are stripped, and also inside `bash|sh -c '…'`, `eval` and `find -exec`; the curl-pipe,
  fork-bomb, secret-path and `[auto] deny` regexes run on the whole command text (or on the
  `file_path`/`path`/`notebook_path` of other tools, and Glob's pattern). Additions to the
  §18.2 list, all on the safe side: `doas`/`pkexec` count as sudo; recursive `rm` is also
  denied for `.`, `../…`, `~/x`, `$HOME/…`, a top-level directory (`/usr`) and `/home/<user>`;
  `git push --force-with-lease`/`--mirror`; any forced `git clean`; `git checkout .` and
  `git restore .`; `halt`, `poweroff`; `systemctl kill|isolate|poweroff|reboot|halt`;
  `mke2fs`, `wipefs`; `claude logout`, `claude setup-token`. The secret-path rules apply to
  Read, Glob and Grep too (deny runs before allow), and `Write`/`Edit`/`MultiEdit`/
  `NotebookEdit` under the system directories are denied. For `cp`, `mv`, `install`, `ln`
  and `rsync` only the destination counts as a write, so `cp /etc/hosts ./` stays possible.
  A config allow never overrides a deny.
* **Allow rules, deviation on paths.** The brief wants `ls /etc` allowed, which the "every
  absolute path under the roots or /tmp" rule of §18.2 forbids. So commands are in two
  classes: *read-only* ones (`ls cat head tail wc pwd echo which stat file du df tree diff
  grep rg find fd awk sed -n date`, bare `env`, `git status|diff|log|show|branch|rev-parse|
  remote -v|stash list`) may name any absolute path except under `/home`, `/root`, `/Users`,
  `/var/home` and `/proc` outside the roots; every other allowed command (`git add|commit`,
  `go …`, `make`, `pytest`, `cargo`, `npm`, `mkdir -p`, `cp`, `mv`, `touch`, `cd` and the
  `[auto] allow` regexes) needs every path argument, absolute or with a `..`, under a
  project root or `/tmp`. Redirections with `>` must stay inside the roots or `/tmp` (or
  `/dev/null`); `<` follows the read rule. Words with `$VAR`, `~` or a substitution are
  unknown and never allowed. Dangerous options are refused per command: `find -exec|-delete|
  -fprint`, `rg --pre`, `fd -x`, `awk` with `system`/`getline`/pipes/redirection, `sed -n`
  with `w`/`e` commands or `-i`, `tree -o`, `date -s`, `git diff --output|--ext-diff`.
* **`cd` is allowed** (not in §18.2) because `cd /home/u/proj && make` must pass; the target
  must be in a root or `/tmp`, and later relative paths are resolved against it. `make`
  takes at most one lowercase target and nothing else; `env` only bare. Of the wrappers only
  `time`, `timeout` and `nice` keep a command allowable, and assignment prefixes only when
  they are not `PATH`, `LD_*`, `DYLD_*`, `GIT_*`, `BASH_ENV`, `IFS`, `PYTHONPATH`,
  `NODE_OPTIONS`, `GOFLAGS`, `MAKEFLAGS` and similar. `mcp__mado__fetch_url` is allowed
  next to `web_search`. The rule name of a Bash allow lists the commands
  (`rule:go test, git status`).
* **Classifier.** `policy.Classifier` with `tool_choice: {"type":"tool","name":"decide"}`
  (added to `messages.Request`), `max_tokens` 4096, its own client with 20 s per attempt
  and no inner retries, one outer retry after any failure (timeout, transport, 5xx,
  malformed answer: no `decide` call, bad JSON, a decision other than allow/deny/ask). The
  context is the last 20 non-noise blocks of the last 80 events, one line each, cut at 300
  runes, plus the last user message (4000 runes) and the tool input (20000 runes). The
  reason is collapsed to one line of at most 300 runes. The model and key are read from the
  `[[chat.models]]` entry at hub start; an editor change needs a restart. A session over
  its owner's budget skips the classifier (ask). The classifier runs in a goroutine (the worker
  connection keeps reading); when it returns, the switch is checked again, and a request a
  viewer answered meanwhile is left alone.
* **Store.** `sessions.auto_policy` (default 1) is the switch; `approvals` gains `auto`
  (the policy outcome: allow, deny, ask), `reason` and `after_seq` (the highest event seq
  when the request came, so a card keeps its transcript place across reloads).
  `GET /blocks` now returns the pending approvals and every approval the policy handled.
  An earlier "Allow for session" in an auto session is stored with `decided_by = rule` and
  counts as allowed. A decision that cannot reach the machine goes to the user as asked.
  Audit rows: `approval_auto` with `<session> <tool> <decision> <decided_by>` (user empty,
  so the owner sees them) and `session_auto` for the switch.
* **API.** `mado_auto` in the session view; `auto` (`enabled`, `allowed`, `denied`,
  `asked`, `classifier`) in `GET`/`PATCH /api/sessions/{id}` for mado auto sessions;
  `PATCH {"auto": bool}` answers 400 for any other session. Turning the switch on does not
  re-run approvals that are already pending.
* **`/new`.** The mode is the fifth token (`/new <machine> <project> <profile> <mode>`), so
  a mode needs the profile written out.
* **SPA.** "auto (mado)" in the form (router profiles: "edits pass, mado's rules and
  classifier decide the rest"), the panel and the composer. An automatic decision is a
  collapsed `<details>` card whose summary is the decision ("auto-allowed by mado: go test,
  git status", "auto-denied by the classifier: <reason>"), with the call inside;
  auto-allowed cards carry the `noise` class (diagnostics toggle), denied ones always show,
  and a closed tool chunk says "· N auto-denied". A pending card the policy left to the
  user shows "mado asks you: <reason>". The panel's "Auto decisions" section has the counts
  (refetched at most every 500 ms while decisions stream in) and a switch.
* **Screenshots** (scratch hub with the dev login, `[auto]` with a classifier, a seeded
  router session in auto with two rule allows, a classifier allow, a rule deny, a
  classifier deny and an asked approval): desktop with the chunk closed and open, with
  diagnostics on (auto-allowed cards visible), the switch turned off (toast, label), the
  new-session form's "auto (mado)" option, and 390 px with the transcript and the details
  sheet. No horizontal scroll; no JS errors. Dark mode not checked.
* **Open.** The real AI& run (aiand-pair, auto, classifier deepseek-v4-flash); how often the
  classifier answers `ask` in practice; the
  lexer is a heuristic, not a shell (aliases, functions defined earlier in the session and
  `PATH` lookups are not visible to it), which is why it only ever allows a narrow set.
* **Per-turn cap removed.** The per-turn classifier cap was removed on 2026-10-18 at the user's request; budgets (§17.6) remain the cost bound. `max_classifier_per_turn` is still accepted in `[auto]` but ignored.
* **Mid-session switch (follow-up, 2026-10-18).** A router-profile code session started in
  `default` or `acceptEdits` can be switched to mado auto while it runs: `PATCH {"auto":
  true}` stores `permission_mode = auto` and `auto_policy = 1` (audit `session_auto`), so a
  resume maps it to `acceptEdits` through `claudeMode`; the running claude keeps its mode
  and is not told. `{"auto": false}` only turns the policy off (the mode stays `auto`).
  Subscription-profile and chat sessions still get 400. Because claude may still be in
  `default`, edits reach the hub, so the allow rules gain `rule:edit-in-project`: `Edit`,
  `Write`, `MultiEdit` and `NotebookEdit` whose every path field lies under a project root
  or `/tmp` (no `~`, no `$`), after the deny rules as usual. It applies to every mado auto
  session (in `acceptEdits` such edits never ask anyway). `GET`/`PATCH` carry `auto` for
  every router code session, with `enabled` true only while the hub really decides; the
  panel shows the switch off for a `default`/`acceptEdits` session with the hint "turn on
  to let mado decide approvals from now on", and after the switch the permissions label
  reads "auto (mado)".

## 19. Local mode: one computer, no server, no IdP (M10, decided 2026-10-19)

A friend of the user wants mado without a server. The hub is pure Go (no cgo, Go SQLite),
the worker needs only `claude` and `tmux`, so a single binary on a Mac can run both. The
missing pieces are a login that does not need an IdP, a way to pair the worker with the
hub without the enrollment dance, macOS defaults, and darwin builds.

### 19.1 Scope and non-goals

* **One user, one machine.** Local mode is for the person sitting at the computer (or reaching
  it over their own tailnet, §19.5). It is not a shared hub: there is no second user and
  no IdP. Everything that exists today for a user (budgets, usage, settings, admin pages)
  belongs to the local user, who is an admin.
* **Same binary, same UI, same store.** No feature is removed or forked. A local hub can
  later be turned into a served hub by adding `[oidc]` and `base_url` and dropping the
  local login; the database carries over.
* The Mac must be awake for sessions to run; sleeping suspends claude. Not our problem to
  solve in M10 (documented).

### 19.2 `mado local`

One command runs the hub and a worker in the same process:

    mado local [--config ~/.config/mado/hub.toml] [--port 8740] [--no-browser]

1. **Files on first run.** If `hub.toml` is missing, write it with local defaults and the
   chat/model sections commented out as an example (AI& base URL, `key_ref = "aiand"`),
   and write `~/.config/mado/secrets.toml` with mode 0600 and an empty `[keys]` table if it
   is missing. Print both paths once. Do not stop: code sessions on the built-in
   `subscription` profile work with no key at all. Chat and router profiles need a key, so
   the printed note says where to put it.
2. **Hub.** `listen` defaults to `127.0.0.1:<port>`, `base_url` to `http://127.0.0.1:<port>`,
   `db_path` to `~/.local/share/mado/hub.db`. The not-https cookie warning is not logged in
   local mode (cookies never leave loopback). `mado local` refuses a `listen` that is not
   loopback, with the same check `hub.LocalLoginAllowed` uses (§19.3).
3. **Worker in-process.** After the store opens, `mado local` ensures one machine row for
   this computer: name = short hostname (`local` when empty), token read from
   `~/.config/mado/machine.token` (0600); when the file is missing a new token is minted,
   the row is created or its token replaced, and the file written. No enrollment code is
   involved because the process owns the store. Then it starts `worker.New` with a config
   derived the same way as `worker.LoadConfig` would (reading `~/.config/mado/worker.toml`
   first when it exists, so a power user can still set `project_roots` or `claude_bin`),
   with `hub_url = ws://127.0.0.1:<port>`, `machine_name` = the row's name, `token_file` =
   the path above, and `project_roots` defaulting to the home directory. The worker starts
   once the listener accepts connections (dial the port, or start the listener first and
   serve from it) so the first connect does not fail and wait a backoff.
4. **Browser.** When stdout is a terminal and `--no-browser` is absent, open
   `base_url` with `open` (darwin) or `xdg-open` (linux); failures are logged, not fatal.
5. **Shutdown.** SIGINT/SIGTERM stop the HTTP server and the worker daemon in that order
   with the same grace as `mado hub`; running claude processes stay in tmux and are
   resumed on the next start, as on a server.

`mado hub` and `mado worker` are unchanged for server installs.

### 19.3 Local login replaces the dev login

* `POST /local-login` (renamed from `/dev-login`) signs in the fixed identity
  `sub = "local"`, name = the OS user name (`$USER`, else "Local user"), email
  `local@localhost`, verified, groups = `[admin_group]`. The audit row says `login local`.
* It is offered when `hub.Options.LocalLogin` is true. `mado local` sets it; `mado hub
  --local-login` sets it too (the flag `--dev-login` stays as a hidden alias). The gate is
  `LocalLoginAllowed(listen)`: the listen address must be loopback. The `MADO_DEV=1`
  requirement is dropped; the environment variable is no longer read anywhere.
* The request itself must also come from loopback: `RemoteAddr` loopback and no
  `X-Forwarded-For` / `Tailscale-User-Login` header. A request proxied in by `tailscale
  serve` carries those headers and gets 403 here; it uses §19.5 instead.
* The login page: when local login is on, show one button "Continue" and hide the IdP
  button unless `[oidc] issuer` is configured; when it is off, the page is as today.
* Tests that used `MADO_DEV` switch to the option.

### 19.4 macOS defaults in the worker

* `tmux_bin`: when `/usr/bin/tmux` does not exist, look up `tmux` on PATH (Homebrew puts it
  at `/opt/homebrew/bin/tmux`); when neither is found the daemon fails at start with
  "tmux not found; install it (brew install tmux) or set tmux_bin".
* `runtime_dir`: when `XDG_RUNTIME_DIR` is unset, default to `~/.local/state/mado/run`
  (mode 0700) instead of failing. Unix socket paths stay under the 104-byte macOS limit
  with that default; a configured `runtime_dir` that would exceed it is rejected with a
  clear message.
* `spool_dir`: `$XDG_STATE_HOME/mado/spool`, else `~/.local/state/mado/spool` (already so;
  confirm on darwin).
* `tmux_scope = auto` already means "systemd-run only when it exists"; confirm it is a
  no-op on darwin without touching the binary lookup.
* `claude_bin` default: PATH lookup; the error when missing says to install Claude Code.

### 19.5 Reaching the computer from a phone over Tailscale (optional, M10b)

`tailscale serve --bg --https=443 http://127.0.0.1:8740` gives the computer an https name and
forwards requests from tailscaled with `Tailscale-User-Login`, `Tailscale-User-Name`
(and `X-Forwarded-For`) set; tailscaled strips those headers from clients, so on a
loopback listener they are trustworthy for requests that arrive from tailscaled.

* `[auth] tailscale = true` in hub.toml turns on header login: a request to `/login` (or
  any page) with `Tailscale-User-Login` present and `RemoteAddr` loopback is signed in
  as `sub = "ts:" + login`, name from `Tailscale-User-Name`, email = the login when it
  has an `@`. No cookie dance beyond the normal web session.
* Admins: `[auth] tailscale_admins = ["alice@github"]`. When the list is empty every
  Tailscale identity is an admin (single-person tailnet default); the README says a shared
  tailnet must set the list.
* `base_url` becomes the https name; local login stays available on the computer itself
  (loopback request without the headers), and it is a *different* user (`local`) from the
  Tailscale one. The README says to pick one and stick to it, or budgets split in two.
* The hub refuses `[auth] tailscale = true` when `listen` is not loopback (then the headers
  could come from anyone on the LAN).

M10b is implemented after M10 is deployed and the single-computer flow is confirmed by a tester.

### 19.6 Build, docs, tests

* `make release` adds `darwin/arm64` and `darwin/amd64`. `make check` adds a cross
  build-only step `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...` so darwin stays
  compiling; the tmux and daemon tests keep running on linux only.
* README gains "Run it on one computer (no server)": `brew install tmux`, install Claude Code,
  download the darwin binary (or `go install`), `mado local`, where the files are, how to
  add the AI& key, and the Tailscale note (§19.5) marked as coming in M10b.
* `deploy/hub.toml.example` gets a short "local mode" comment at the top pointing to
  `mado local` and saying none of the `[oidc]` section is needed for it.
* Tests: `LocalLoginAllowed` (loopback forms, rejection of `0.0.0.0` and tailnet
  addresses), `/local-login` 403 on a request with `X-Forwarded-For`, the login page
  variants, `mado local` first-run file creation in a temp HOME (hub.toml + secrets.toml
  0600 + machine.token 0600 + machine row), the in-process worker connecting (reuse the
  M1 hub+worker harness without tmux: `MADO_NO_TMUX_TESTS=1` path), worker defaults when
  `XDG_RUNTIME_DIR` is unset.

### 19.7 M10 notes

What the implementation decided where §19.1–19.6 left it open, or did differently:

* **Config defaults.** `hub.LoadLocalConfig` fills the defaults before validation, so a
  hand-written local hub.toml may omit `listen` (`127.0.0.1:<port>`), `base_url`
  (`http://<listen>`) and `secrets_file` (`~/.config/mado/secrets.toml`). `--port` is used
  for the first-run file and a missing `listen`; given explicitly, it also replaces the
  port of an existing `listen` and resets a loopback `http` `base_url` to match.
* **Machine pairing.** The name is the first label of the hostname reduced to
  `[A-Za-z0-9_-]` (64 bytes max), `local` when nothing is left. When `machine.token`
  exists and its hash matches a row, that row (and its name) is used; when it exists but
  this database does not know it, its hash is stored for the hostname row instead of
  minting a new token. `Store.SetMachineToken` is the enrollment upsert without a code.
* **Startup order.** `worker.New` runs before the listener so a missing tmux or claude
  ends `mado local` before anything is served; then the listener opens, the HTTP server
  starts on it, and the daemon dials `ws://<listen>/ws/worker`. A worker that stops with
  an error (router port taken) stops `mado local` too.
* **Worker config.** `worker.LocalConfig` reads `worker.toml` when present (unknown keys
  stay an error) and always replaces `hub_url`, `machine_name` and `token_file`.
* **Warning.** `hub.Options.LocalMode` (set only by `mado local`) suppresses the not-https
  warning; `Options.LocalLogin` alone does not, so `mado hub --local-login` on http now
  logs it (the old dev login suppressed it).
* **Login page and audit.** The IdP is offered as a link to `/login?idp=1` (the old
  redirect) when `[oidc] issuer` is set. The audit row is action `login`, detail `local`.
  `POST /dev-login` is gone; only the `--dev-login` flag stays as a hidden alias. The shim
  no longer lists `MADO_DEV` among the variables it strips from claude's environment.
* **Proxy check.** Any `X-Forwarded-For` or `Tailscale-User-Login` header, whatever its
  value, makes `/local-login` answer 403; so does a non-loopback `RemoteAddr`.
* **runtime_dir.** Without `XDG_RUNTIME_DIR` the default is `$XDG_STATE_HOME/mado/run`,
  i.e. `~/.local/state/mado/run` when `XDG_STATE_HOME` is unset (the spool's base). The
  length check applies to every `runtime_dir`, derived or configured: a session socket is
  `<runtime_dir>/<uuid>.sock`, 42 bytes after the directory, and the path may be at most
  103 bytes, so `runtime_dir` is at most 61 bytes.
* **Binaries.** The tmux lookup is `/usr/bin/tmux`, PATH, then `/opt/homebrew/bin/tmux`
  and `/usr/local/bin/tmux` (a process started outside a login shell has no Homebrew in
  PATH). `claude_bin` defaults to `~/.local/bin/claude` when it exists (the native
  installer, so server installs keep their path), else PATH. `worker.New` now fails when
  tmux or claude is missing, on servers too; before, a missing claude only failed each
  session.
* **Confirmed unchanged.** The spool default already is `$XDG_STATE_HOME/mado/spool` or
  `~/.local/state/mado/spool`; `tmux_scope = auto` only looks up `systemd-run`, which
  does not exist on darwin, so it is a no-op there.
* **darwin build.** The shim's peer check needed `peercred_darwin.go`
  (`LOCAL_PEERCRED` through `golang.org/x/sys/unix`, which moved from an indirect to a
  direct requirement; no new module). `make check` cross-builds `darwin/arm64` only;
  `make release` builds both darwin architectures.
* **Browser.** "stdout is a terminal" is `os.ModeCharDevice` on stdout (no `x/term`).
* **Not done in M10.** No test runs on a real macOS machine; the darwin binary is only
  cross-compiled.

### 17.14 Tail-first transcript loading (decided 2026-10-19)

A long code session (22k events, 45 MB of raw stream-json) took many seconds to open
because the SPA paged forward from seq 0 and rendered every block before showing anything.
The transcript now loads from the end.

* `GET /api/sessions/{id}/blocks?before=<seq>` returns the newest page of events with
  `seq < before` (`before` absent or 0 means the end), in ascending order, with `first`
  (the smallest seq returned), `last`, and `more_before` (older events exist). The
  existing `after=` form stays for forward paging and for tests. The page boundary is
  extended backwards to the nearest user turn (a stream-json `user` event whose content
  is human text, not a `tool_result`) so a tool call is never shown without its result
  and the first visible block is a prompt; the extension is capped at three pages, then
  the page is cut anyway.
* The session view opens with one `before` request, scrolls to the bottom, and starts
  the SSE stream from `last`. A "Load earlier" control at the top (and reaching the top
  by scrolling) fetches the previous page and prepends it while keeping the viewport on
  the same block (measure `scrollHeight` before and after in a layout effect). Drafts,
  approvals, the Models panel and the right panel are unaffected: they do not depend on
  having every block.
* Pending approvals still come with every blocks response as today.
* Tests: the store query (page shape, boundary extension, cap), the API (`first`,
  `more_before`, `before=0`), and a UI check with a seeded 5k-event session that the
  first paint waits for one request only. Measure before/after open time on the seeded
  session with the Marionette screenshot tool in the scratchpad and record both numbers
  in the milestone note.

### 17.15 M11 notes (2026-10-19)

* `Store.EventsBefore(session, before, limit)` reads the newest `limit` events with
  `seq < before` (descending, then reversed). When older events exist and the page does
  not start at a human turn, `userTurnBefore` scans at most three pages back for one
  (`humanTurn`: a main-thread `user` event whose content is a string, or text, image or
  `mado_file` blocks without a `tool_result`; a subagent prompt with
  `parent_tool_use_id` is not one). Not found within the three pages: the page is cut at
  its original boundary, so a response is never more than four pages and usually one.
  The scan reaching the session's first event includes it. `more_before` is an
  `EXISTS` query on the final first seq. `blocksPage` stays 1000 events for both forms.
* `GET .../blocks?before=` and `?before=0` both mean the end (`q.Has("before")` picks
  the form); without `before` the `after=` form answers as before.
* Block keys in `buildTree` are `seq-<index within the event>` instead of the index in
  the whole list, so prepending a page keeps every DOM node and every chunk's
  open/closed override. The session view anchors on `scrollHeight` (before and after
  the prepend, in the layout effect) and loads the earlier page when `scrollTop < 200`
  or on the "Load earlier" button. Decided approvals placed before the oldest loaded
  block are held back until their page is loaded; pending ones always show.
* Measured on a dev hub (loopback, headless Firefox, `scratchpad/tools/ttfp.py`, median
  of five warm opens, time from navigation to the transcript painted):

  | seeded session | before (forward paging) | after (tail first) |
  | --- | --- | --- |
  | 5,031 events, 10 MB | 335 ms, 6 requests, 5,031 blocks | 117 ms, 1 request, 1,060 blocks |
  | 22,033 events, 45 MB | 1,266 ms, 23 requests | 118 ms, 1 request, 1,026 blocks |

  The open time no longer grows with the session. Over a remote link the old cost also
  paid 6 or 23 round trips and the full transfer; the new one pays one page.
* The UI check (one `/blocks` request before the first paint, the top block keeps its
  position within a pixel after the prepend, all 5,031 blocks reachable by "Load
  earlier", a chat's streaming draft replaced by its stored block) runs with the
  Marionette tools in the scratchpad; there is no JS test runner in the repo.

## 20. Concierge: start projects and sessions from a conversation (M12, decided 2026-10-19)

The New Session form asks the user to translate "what I want to do" into machine, directory,
profile and mode. The concierge moves that translation into a chat with mado. The intended
flow, fixed with the user: **talk to the concierge → it prepares directories and starts the
mado/claude sessions → the design discussion happens inside the created claude session →
implementation starts there.** The concierge therefore does not need a strong model and must
not try to design the project itself.

### 20.1 What it is

A chat session (§17) with `concierge = true`: mado's own API is offered to the model as tools,
the system prompt is the concierge prompt (§20.4), and the model is `[concierge] model`
(a `[[chat.models]]` name; default: the chat default model; the user picked a cheap one).
Everything else is the chat runner: streaming, usage rows of kind `chat`, the tool loop and
`max_tool_rounds`, context trimming, interrupt.

Entry points: a third tab **Plan** on `/new` (optional first message, same as the chat tab),
and `/new plan` in the composer. Concierge sessions show a distinct icon in the sidebar.

### 20.2 Tools

All read tools are free to call; the write steps run only inside an approved plan (§20.3).

| tool | args | returns |
|---|---|---|
| `list_machines` | – | online machines: name, project roots, claude version, number of running sessions |
| `list_projects` | `machine` | the roots and, for each, its top-level directories (name, is_git, mtime), at most 200 per root |
| `inspect_dir` | `machine`, `path` | entries (≤ 200, dirs first), the first 2 KB of README*, git summary (branch, upstream, dirty count, `git worktree list` when the path is a repo or a worktree), languages guessed from file names. `path` must be inside a root. |
| `list_profiles` | – | profiles with kind, main model and the aliases' upstream models; which one the user used last |
| `propose` | `plan` (§20.3) | stores the plan and renders the plan card; returns `plan_id` |
| `session_status` | `session_id` | state, title, machine, cwd, last user text and last assistant text (abbreviated), pending approvals, month-to-date cost; only the user's own sessions |

`inspect_dir` and `list_projects` need a worker round trip: new frames hub→worker
`fs_inspect` `{request_id, path, kind: "list_projects" | "inspect"}` and worker→hub
`fs_result` `{request_id, ok, data | error}`, 20 s timeout. The worker refuses paths outside
`project_roots` and follows no symlinks out of them.

### 20.3 The plan and its card

```json
{"summary": "one line",
 "prepare": [
   {"machine": "gpu1", "action": "mkdir",    "path": "/home/me/work/newproj"},
   {"machine": "gpu1", "action": "git_init", "path": "/home/me/work/newproj"},
   {"machine": "gpu1", "action": "clone",    "path": "/home/me/work/x", "url": "https://github.com/o/r.git", "branch": "main"},
   {"machine": "gpu1", "action": "worktree", "repo": "/home/me/work/tt-metal", "path": "/home/me/work/wt-foo", "branch": "foo", "base": "main"}],
 "sessions": [
   {"machine": "gpu1", "cwd": "/home/me/work/newproj", "profile": "aiand-pair", "main_model": "",
    "mode": "auto", "title": "newproj: design", "first_prompt": "…"}]}
```

* Actions are exactly `mkdir`, `git_init`, `clone`, `worktree`. Paths must be inside a root
  of that machine (hub validates against the worker's `Hello.Projects`, worker validates
  again); `url` must be `https://` or `ssh://`/`git@` git URLs; `branch`/`base` are plain
  ref names. `mkdir` creates parents; `git_init` on an existing repo is a no-op.
* The **plan card** is a stored chat event `{"type":"system","subtype":"mado_plan", …}` with
  the plan and its state (`proposed`, `running`, `done`, `failed`, `rejected`); the SPA
  renders it as a card with every step listed, the first prompt expandable, and
  **Approve** / **Reject** buttons while `proposed`. `POST /api/sessions/{id}/plans/{plan_id}`
  `{"decision":"approve"|"reject"}` (Origin + CSRF, owner only). Approval is the consent
  for every write step in the card; no further approval cards for them.
* Execution, in order: each `prepare` step as a hub→worker `fs_prepare` frame (same shape
  as `fs_inspect` with the action; `clone` and `worktree` get a 10 min timeout and return
  the last 2 KB of git's output), then each session through `newSession` (§3) with the
  stated profile/mode/title and `apiSend` of `first_prompt`. The card updates step by step
  over SSE (`✓ mkdir …`, `✓ session started → link`). The first failure stops the sequence;
  the card shows the error and the steps not run; the model receives the same text as the
  `propose` tool result so it can propose a corrected plan.
* `mode` defaults to `auto` for router profiles and `acceptEdits` for `subscription`;
  `profile` defaults to the one the user used last. Budget checks (§17.6) apply to the
  sessions as for `/new`.
* The concierge session keeps `spawned` (the session ids it started) so `session_status`
  can be asked without ids ("how are they doing?") and the card links stay valid.

### 20.4 Concierge prompt (fixed text, English, in `internal/chat/concierge.go`)

The gist: you are mado's concierge; your job is to turn what the user wants into directories
and sessions, not to design or implement it; look before you ask (use the tools); ask at most
one or two questions when the machine, the directory or the project kind is really unclear,
otherwise propose; write the `first_prompt` so that the created session starts by discussing
the design with the user before implementing, and carries everything the user said that the
session needs (goal, constraints, repo facts you found); prefer worktrees for existing repos
and new directories under the root the user names; never run a step outside a plan; after a
plan is done, answer with the links and stop.

### 20.5 Build, docs, tests

* Hub: `internal/hub/concierge.go` (tools, plan store, execution), `plans` table
  (`id, session_id, json, state, created_at, decided_at`), API route, SSE block.
* Worker: `fs_inspect` / `fs_prepare` handling with the root check; `git` is run with a
  clean environment and the timeout; stdout/stderr tails only.
* SPA: Plan tab, the plan card in the transcript, the sidebar icon.
* Config: `[concierge] model`, validated like `[auto] classifier`.
* Tests: path validation (inside/outside roots, `..`, symlink escape), plan JSON
  validation, the execute sequence with a fake worker (stop on first failure, card states,
  model receives the error text), `session_status` ownership, the API decision endpoint,
  the worker frames end to end in the hub+worker harness (`MADO_NO_TMUX_TESTS=1` path with
  a fake claude).

### 20.6 Deferred

Read-only visibility of Claude Code sessions started outside mado (`/rc` sessions) was
discussed and put on hold by the user on 2026-10-19: it would need the history reader
(§14) enabled for live sessions and research into where Claude Code records the Remote
Control state. Taking control of such sessions is rejected (two controllers on one claude).

### 20.7 M12 notes (2026-10-19)

Done: §20.1–§20.5. `internal/hub/concierge.go` (tools, plans, execution, the decision
route), `internal/chat/concierge.go` (the prompt), `internal/worker/fs.go` (the frames),
`internal/proto/fs.go` (frames and the URL, ref and path validators shared by both sides),
the SPA parts. Not deployed. What the spec left open and how it was decided:

* **Roots.** `hello` gains `roots[]` (the `project_roots` themselves) and the capability
  `fs`; `projects[]` also lists the roots' children, so it cannot tell which entries are
  roots. For an older worker the hub derives the roots (entries not below another entry);
  such a worker has no `fs`, so plans with prepare steps on it are refused ("too old"),
  while plans that only start sessions still work. The hub keeps roots and capabilities in
  memory per connection: every fs operation needs the worker online anyway.
* **The outcome reaches the model as a note, then a turn.** `propose` returns at once
  (`plan_id: …` and "nothing has run yet, end your turn"), so the composer is not blocked
  while the user reads the card. When the plan ends, the hub stores a user event with
  `"mado_note": "plan"` whose text is the card's content (each step with ✓/✗/not run, the
  error, links as `/s/<id>`), and runs a concierge turn on it: the model answers with the
  links or proposes a corrected plan. The note renders as a "mado" notice, not as the
  user's message; replay sends it as a user message. A running turn is waited for; over
  the budget, or when the turn cannot start, the note is stored without a turn. A rejection
  stores "The user rejected plan …" without a turn. The instructions about what to do with
  the outcome live in the prompt, not in the note the user also sees.
* **The card is live.** Stored events are immutable, so the `mado_plan` event carries the
  plan as proposed and every plan block served by `/blocks` and SSE carries the current
  state from the `plans` table; changes go out as SSE `plan` events (the card's JSON). The
  `plans.json` column holds `{plan, steps, error}`; `steps` has one entry per prepare step
  and per session with `kind`, `label`, `state` (`pending`, `running`, `done`, `failed`,
  `skipped`), `output` (git's tail or the error) and `session_id`.
* **One waiting plan.** A new `propose` turns a plan of the same session that is still
  `proposed` into `rejected` ("replaced by a newer plan"), so an outdated card cannot be
  approved by mistake.
* **Plan validation.** The JSON is decoded strictly (unknown fields refused); a plan sent as
  a JSON string is accepted (some models do that). At most 20 prepare steps and 10
  sessions, at least one of either: prepare-only plans and session-only plans are allowed.
  `mkdir`/`git_init` take only `machine` and `path`; `clone` takes `url` and an optional
  `branch`; `worktree` takes `repo`, `branch` (required) and an optional `base`. Paths must
  be absolute and clean (no `..`, `//` or trailing `/`) and inside a root of the machine;
  the machine must be online. URLs: `https://` without credentials, query or fragment;
  `ssh://` with a user but no password; `git@host:path`; nothing starting with `-`. Ref
  names: `[A-Za-z0-9][A-Za-z0-9._/+-]*`, no `..`, `//`, `/.`, `@{`, trailing `/`, `.` or
  `.lock`, at most 200 bytes. A session's `title` defaults to the cwd's last element,
  `first_prompt` may be empty (the session just starts) and is at most 100 kB.
* **Defaults.** `profile`: the profile of the user's newest code session (no new state;
  `subscription` when there is none or it is gone). `mode`: `auto` for router profiles,
  which is mado auto (§18, claude runs in acceptEdits), `acceptEdits` for subscription. The
  defaults are written into the stored plan, so the card shows what will run.
* **Sessions.** Started through `newSession` with a new `InsideRoot` flag: the cwd may be any
  directory inside the roots (a new worktree is not a listed project), as for "Resume in
  mado"; the worker checks it again with `ResolveCWD`. The hub waits up to 2 minutes for
  the worker's ack of `start_session` and only then sends `first_prompt` as a
  `user_message` (the code path of `apiSend`; the budget check is `newSession`'s). A session
  that was created but failed to start stays in `spawned`, so its card link works.
* **Worker side.** Paths are resolved by `ResolveInside`: the longest existing prefix goes
  through `EvalSymlinks`, the rest does not exist yet, and the result must stay inside a
  resolved root; a dangling symlink on the way is refused. `list_projects` skips hidden
  directories and symlinks that leave the root, newest first. `inspect` lists hidden
  entries too (dirs first), reads the first `README*` regular file, runs git with
  `-c core.fsmonitor=false` (a repository's config could name a command), counts dirty
  files from `git status --porcelain` (untracked included), and guesses languages from file
  names two levels deep (at most 3000 entries, marker files such as `go.mod` count double).
  `git` runs with `LC_ALL=C`, `GIT_TERMINAL_PROMPT=0`, `GIT_SSH_COMMAND=ssh -o BatchMode=yes`
  and only `PATH`, `HOME`, `USER`, `LOGNAME`, `SSH_AUTH_SOCK`, `TMPDIR` from the daemon's
  environment. Timeouts: the worker gives itself 18 s (inspect, mkdir, git_init) or 10 min
  (clone, worktree), the hub waits 20 s or 10.5 min. `mkdir` on an existing directory and
  `git_init` on a repository are no-ops; `clone` needs a missing or empty target;
  `worktree` needs a missing path, checks out an existing local branch, else creates it
  with `-b` from `base` (default HEAD).
* **Restarts.** A hub that stops cancels running plans; plans left `running` are marked
  `failed` at the next start (the running step "check the machine", the rest skipped).
* **Entry points.** `POST /api/sessions {"kind":"chat","concierge":true}`; `GET /api/me` has
  `concierge` (chat enabled and a model to use), which shows the Plan tab. `/new plan [first
  message]` works in chat and code sessions; with text after it, a machine named `plan`
  keeps `/new plan <project>` the code command.
* **Concierge sessions** are chat sessions in every other respect: the model picker,
  attachments, delete, usage kind `chat`. The panel lists the concierge's tools and the
  started sessions; the sidebar shows a compass. `session_status` with no id reports every
  started session; it gives the session's total cost and the user's month-to-date cost.
  `list_machines` lists online machines (and the names of offline ones) with
  `live_sessions` (sessions not ended) and `can_prepare` (`fs`).
* **Audit.** `plan_approve` / `plan_reject` (`<session> <plan>`), `session_start` with
  `concierge` for the concierge itself; the started sessions and their first prompts are
  audited like `/new` (`session_start`, `message` with the length only).
* **Screenshots** (scratch hub with `--local-login`, a real worker on a separate tmux socket
  with a fake claude, a Python fake of the Messages API scripted to call list_machines,
  list_projects, inspect_dir, list_profiles and propose): the Plan tab, the proposed card,
  the tool chunk opened, approval → the card done with the session link and the note, the
  started session answering its first prompt, a failed card (the tmux socket path was too
  long: the error reached the card and the model), dark, `/new plan` from a code session,
  390 px. No horizontal scroll at 390 px; no JS errors.
* **Open.** The concierge cannot stop or delete sessions, and has no tool to send a message
  into a started session; `session_status` reads only the last 200 events. A clone over
  ssh needs the daemon's ssh agent or keys without a passphrase (BatchMode). Long paths
  make long step labels.


## 21. Answering `AskUserQuestion` from mado (M13, decided 2026-10-19)

Claude Code's `AskUserQuestion` tool reaches mado through the permission prompt tool like
any other tool call, so today it shows as "Allow AskUserQuestion?" with Allow / Allow for
session / Deny, and allowing it returns the input unchanged: claude gets no answer. The
tool is answered by returning `updatedInput` = the original input plus `answers`
(`{question text: answer}`) and optional `annotations`; that is how Claude Code's own UI
and the SDK's `canUseTool` answer it.

### 21.1 Protocol

* `ApprovalResponse` gains `updated_input` (raw JSON, optional). The shim's decider passes
  it to `toolResult`: `updatedInput` is `updated_input` when present, else the original
  input (unchanged behaviour). The worker relays it. Workers must be redeployed for this.
* `POST /api/sessions/{id}/approve` accepts, with `decision: "allow"`, an `answers` object
  (`{question: string}`) and optional `notes` (`{question: string}`). The hub validates that
  the approval's `tool_name` is `AskUserQuestion`, that every key is one of the questions in
  the stored input, and that every answer is non-empty and ≤ 2000 bytes; it builds
  `updated_input` = input + `answers` (+ `annotations: {question: {notes}}` when notes are
  given) and stores the answers on the approval row (new column `answers` JSON) so the
  transcript shows what was answered.
* Answer format (verify against the installed `claude` bundle by grepping for `answers`
  near `AskUserQuestion` / `multiSelect` and record the finding in §21.4): a single-select
  answer is the option label; a multi-select answer is the chosen labels joined with ", ";
  "Other" is the user's free text.

### 21.2 Card

When `tool_name == "AskUserQuestion"` the approval card is a **question card** instead of
the generic Allow/Deny card:

* For each question: the `header` chip, the question text, the options as radio buttons
  (checkboxes when `multiSelect`) with label and description, a `preview` shown in a
  monospace box when the focused/selected option has one, and an "Other" entry with a text
  field. Optional free-text notes per question (collapsed by default).
* One **Answer** button (disabled until every question has an answer) sends `allow` with
  `answers`; a **Dismiss** link sends `deny` with the message "The user dismissed the
  question in mado." No "Allow for session" for this tool.
* After the decision the card shows the chosen answers (or "dismissed"), like a decided
  approval card. The transcript's `tool_result` block of the question then shows claude's
  view of the answer as today.
* Pending question cards are listed in the right panel with the other approvals and open
  the card on tap.

### 21.3 mado auto

`AskUserQuestion` is never decided by rules, session rules or the classifier: the pipeline
(§18.2) routes it to the user first, before the session rule, with the reason "a question
for you". `allow_session` is rejected by the API for this tool.

### 21.4 Tests and notes

Shim: `toolResult` with and without `updated_input`. Hub: answers validation (unknown
question, empty, too long, wrong tool), stored answers, the auto pipeline bypass, the API
rejection of `allow_session`. UI: a seeded `AskUserQuestion` approval with two questions
(one multiSelect, one with previews) rendered and answered through the browser tool; a
screenshot under `scratchpad/ui-m13/`. Record the verified answer format here.

**Verified answer format** (grepped from the installed bundle, Claude Code 2.1.303, a
bun-compiled binary whose JS is readable with `strings`):

* The tool's input schema has `answers: record<string, string>` ("User answers collected by
  the permission component", keyed by question text) and `annotations: record<string,
  {preview?: string, notes?: string}>` ("Keyed by question text"). The answer value is
  preprocessed: an array of strings is joined with `", "`, so a string is canonical.
* Claude Code's own dialog returns `{behavior: "allow", updatedInput: {...input, answers,
  annotations}}`; `annotations[q]` carries `preview` (the selected option's preview, when
  the question has previews) and `notes` (the user's free text on that question).
* Single select: the option label. Multi select: the labels joined with `", "` (the result
  schema says "multi-select answers are comma-separated"; when checking an answer the tool
  splits on `", "` and matches each part against the labels). "Other": the typed text as
  the answer; there is no "Other" option in the input ("the form lets the user type their
  own answer"). An unanswered question is simply absent; `"(notes only)"` is the value the
  dialog uses for a question with notes but no choice.
* What claude reads (the tool result): `Your questions have been answered: "q"="a"
  selected preview: … notes: …, …. You can now continue with these answers in mind.` when
  every answer is a label, else `The user answered: …`. A deny's message reaches claude as
  the error result.
* Newer inputs may carry `kind: "text" | "number"` questions without options (a gated
  experiment); their answer is the typed text.

So §21.1 holds as written; mado adds `preview` to `annotations` as Claude Code does.

### 21.5 M13 notes

* Code path: `ApprovalResponse.updated_input` (proto) → worker daemon → shim
  (`proto.Local.updated_input`, the shim needed the same one-line relay as the worker) →
  `approve.SocketDecider` → `toolResult`, which uses it when present and valid JSON, else
  the original input. Older workers drop the field, so an answered question reaches claude
  without answers until the worker is redeployed.
* Hub: `decideApproval` takes `answers` and `notes`. For `AskUserQuestion`:
  `allow_session` is 400; `allow` needs at least one answer (keys must be questions of the
  stored input, values non-empty after trimming and ≤ 2000 bytes; notes the same, empty
  notes dropped); `deny` sends "The user dismissed the question in mado.". Answers on any
  other tool, or with deny, are 400. `updated_input` is the stored input plus `answers` and
  `annotations` (`notes`, plus `preview` for a single-select answer that is an option with
  a preview). The `answers` column holds `{"answers":{…},"notes":{…}}`.
* `onApprovalRequest` routes `AskUserQuestion` before the session rule: pending in a plain
  session, `autoAsk` with "a question for you" in mado auto (so it counts as asked). An
  "allow for session" stored for the tool before M13 is ignored.
* API: an approval of this tool carries `questions` (from the input) and, once answered,
  `answers` and `notes`. `TranscriptApprovals` also returns every `AskUserQuestion`, so a
  reload shows the answered or dismissed card. `ToolInputSummary` shows the first question
  and "(+N more)".
* UI: the question card replaces the Allow/Deny card in the transcript; the right panel
  lists a pending question as a button that closes the phone sheet, scrolls the card into
  view and focuses its first choice (a full second form in the panel would compete with
  the transcript's). Answer is enabled once every question has an answer; the API itself
  accepts a subset. `kind: "text"/"number"` questions get a plain input.
* Checked end to end with a real worker, shim and approve-tool and a fake claude that asks
  the fixture `internal/hub/testdata/ask_user_question.json` (the first question is the
  real one from the home-mado hub DB, the other two add multiSelect and previews) and
  words the result like 2.1.303; screenshots in `scratchpad/ui-m13/` (desktop light and
  dark, 390 px phone: no horizontal scroll, no JS errors).
* Not done: a viewer's decision is still not pushed to other open viewers (as before for
  every approval); the timeout `afkTimeoutMs` path of Claude Code's own dialog has no mado
  equivalent.

### 21.6 Resume

A stopped code session (`exited` or `detached`, with a `claude_session_id`) has a Resume
button in the panel: `POST /api/sessions/{id}/resume` starts a new session with
`claude --resume` on the same machine, cwd, profile, model, mode and title (audit
`session_resume`). It is the way to move a conversation to a new shim or to changed
profile aliases; 409 with the live session's id if the conversation already runs.

A resume continues the same session as far as the user can see. The stopped row gets
`resumed_by` (the new session's id) and the new row `resumed_from` (the old one's id);
`GET /api/sessions` leaves out rows with a `resumed_by` unless `all=1`, so the sidebar
shows one entry per conversation, and both fields are in the session view. A continued
session is not resumable (a resume of it answers 409 with the newer id); its page still
loads, with a banner linking to the newer session and an "Open latest" button that
follows the chain, and the newer session's panel links back with "Continued from".
Deleting a session clears `resumed_by` on the rows it continued, so the previous session
reappears and can be resumed again; the deleted row's `resumed_from` target is left as it
is. Sessions resumed before the columns existed are linked when the store opens: in each
group of code sessions of one owner sharing a `claude_session_id`, every older row gets
the newest's id as `resumed_by` and the newest gets the row before it as `resumed_from`,
only where the field is still empty.

## 22. OpenAI-compatible upstreams (M14, decided 2026-10-19)

The user wants the director role (`fable`, the profile's `main`) served by a model running
on their own hardware (a tuned GLM on the P300s, later) while the implementer roles stay on
AI&. Building that inference service is a separate project; mado's part is to accept
**any OpenAI-compatible chat-completions server** as an upstream, so that whatever serves
the model (tt-inference-server/vLLM, Ollama, LM Studio, llama.cpp) can be swapped in by
editing a profile. The same adapter lets `[[chat.models]]` and the `[auto]` classifier use
such servers, which also serves the local-mode user (§19).

### 22.1 Config

`kind = "openai"` on an upstream (`[profiles.upstreams.<name>]`) or a chat model
(`[[chat.models]]`); default `"anthropic"` (today's behaviour, unchanged). For `openai`:
`base_url` is the API root that has `/chat/completions` under it (e.g.
`http://127.0.0.1:8000/v1`), `key_ref` is optional (local servers have no key), the auth
header is `Authorization: Bearer`. New optional fields on every upstream and chat model:
`timeout_first_byte` (Go duration, default `60s`; a local server with no prefix cache
needs `180s`) and `timeout_total` (default `10m`). `context_window` and `max_output`
exist already and matter more here (a local server's limits).

### 22.2 Translation (`internal/oai`)

One package used by the router (for `claude` traffic) and by `internal/messages` (for chat
and the classifier). Request, Anthropic Messages → chat completions:

* `system` (string or text blocks) → one leading `system` message; `messages` → messages
  with text blocks joined; `image` blocks → `image_url` with a `data:` URI (if the server
  rejects it, the error is passed through); `document` blocks → their extracted text.
* assistant `tool_use` → `tool_calls[]` (`id`, `function.name`, `function.arguments` as a
  JSON string); user `tool_result` → a `tool` message with `tool_call_id` and the text
  content (images inside a result are replaced by `[image omitted]`); `thinking` blocks are
  dropped on the way up.
* `tools[]` → `tools[]` of type `function` (`name`, `description`, `parameters` =
  `input_schema`); `tool_choice`: `auto` → `auto`, `any` → `required`, `tool` →
  `{type:function, function:{name}}`, `none` → `none`.
* `max_tokens` → `max_tokens`; `temperature`, `top_p`, `stop_sequences` → `stop`;
  `metadata.user_id` → `user`; `stream` → `stream` with `stream_options.include_usage`.
  Unknown Anthropic fields are dropped; `thinking` requests are dropped (the server's own
  reasoning setting applies).

Response, chat completions → Anthropic Messages (streaming and non-streaming):

* `message_start` (id from the server or synthesised, `model`, usage zeros) →
  `content_block_start`/`delta`/`stop` per block: `content` deltas → one `text` block;
  `reasoning_content` / `reasoning` deltas (DeepSeek, vLLM, Ollama styles) → one `thinking`
  block with `thinking_delta` and no signature; `tool_calls` deltas → one `tool_use` block
  per index with `input_json_delta` carrying the argument fragments (ids from the server,
  else `call_<n>`); `finish_reason` `stop`→`end_turn`, `tool_calls`→`tool_use`,
  `length`→`max_tokens`; `usage` (`prompt_tokens`, `completion_tokens`,
  `prompt_tokens_details.cached_tokens` → `cache_read_input_tokens`, uncached input =
  prompt − cached) in `message_delta`; `message_stop`.
* Errors: a non-2xx upstream response becomes an Anthropic-shaped error with the same
  status; a stream that ends without `[DONE]` or with a malformed chunk ends the message
  with `stop_reason: "end_turn"` and an `error` event only if nothing was emitted.
* `POST /v1/messages/count_tokens` (Claude Code calls it) → an estimate (bytes/4 of the
  translated request) with `input_tokens`; never an error.

### 22.3 Router and client

* The router (§3, `internal/router`) picks the adapter by the resolved upstream's `kind`
  and otherwise behaves as today (model swap, key injection, usage passthrough).
* `internal/messages.Client` uses the adapter when `Upstream.Kind == "openai"`, so chat
  sessions, the concierge and the classifier can run on such a server. `message_delta` usage
  is the only reliable usage, as with AI&.
* The hub's usage recording and prices key on the upstream `model` string as today.

### 22.4 Tests and docs

Golden tests for request translation (system, tools, tool_use/tool_result round trip,
images, tool_choice) and for stream translation (text, reasoning, parallel tool calls,
usage, length stop, error mid-stream), a fake OpenAI server used end to end through the
router and through the messages client (a chat turn with a tool call), and the profile
validation (`kind`, optional key, timeouts). README: a profile example with `fable` on a
local vLLM/Ollama server and `opus`/`sonnet` on AI&, plus the single-computer note (Ollama as the chat
model in local mode).

### 22.5 M14 notes (2026-10-19)

* **Package layout.** `internal/oai` works on JSON bytes and imports neither user:
  `TranslateRequest`, `Stream` (chat-completions SSE in, Anthropic events out through an
  `Emit` callback), `TranslateResponse`, `ErrorBody`, `EstimateTokens`. The router writes
  the events to Claude Code with a flush per event; `internal/messages` pipes them into its
  existing SSE parser, so assembly, retries and usage merging are unchanged.
  `internal/oai/oaitest` is the fake server the router, messages, chat and hub tests share.
* **Timeouts.** "First byte" is the first byte of the **body**, for both kinds, in the
  router and in the client (it was the response headers, §17.9): vLLM sends its headers
  before the prefill, so a header timeout would bound nothing. The client's default moved
  from 30 s to the §22.1 60 s for every chat model. In the router an `anthropic` upstream
  gets a timeout only when it sets one (today's behaviour, no limit beyond the transport's
  50-minute header wait); an `openai` upstream gets 60 s / 10 min by default. A
  non-streaming request gets only `timeout_total` (no early bytes to time). The values
  must be 1 s..24 h and the first-byte wait no longer than the total (10 min when unset).
  The classifier keeps its fixed 20 s (§18.2): the `auto.go` hook passes only `Kind`, not
  the model's timeouts, so a slow local classifier answers `ask`.
* **Errors before anything was sent.** When the router has written nothing yet, a
  first-byte timeout or an upstream stream that fails before its first chunk is answered
  as an HTTP 504 (timeout) or 502 with the Anthropic error body, rather than a 200 stream
  with an `error` event; Claude Code retries 5xx. Non-2xx answers keep their status; the
  error type comes from the status (OpenAI types do not map onto Anthropic ones), and a 400
  that talks about the context length ("maximum context length", vLLM) gets the message
  prefix `prompt is too long: `, so Claude Code compacts and `IsContextOverflow` matches.
* **Request details the spec left open.** Text-only user content is sent as a string,
  with images as content parts. A user message's `tool_result` blocks become `tool`
  messages first, the rest one `user` message after them. An assistant message with only
  tool calls has `content: null`. `is_error` results get an `Error: ` prefix (the only way
  to carry the flag). `tool_use.input` is compacted into `arguments`. Tools without
  `input_schema` (Anthropic server tools) are dropped; `tool_choice` is dropped when no
  tool remains; `disable_parallel_tool_use` becomes `parallel_tool_calls: false`;
  `top_k` is dropped. Document blocks: a `text` or `content` source as is, a base64 PDF
  through `files.PDFText` (a temporary file), a titled one wrapped in
  `<document name="...">`. JSON is written without HTML escaping.
* **Stream details.** Blocks are strictly sequential: a switch between thinking, text and
  a tool call closes the open block (text after a tool call opens a second text block).
  Tool-call fragments are assumed to arrive in index order, as vLLM, Ollama, llama.cpp and
  OpenAI send them; a fragment for an already closed index is treated as a malformed
  stream. A tool block opens once its name is known. A missing or repeated id becomes
  `call_<n>`; an `arguments` object (instead of a string) is accepted. `finish_reason:
  "stop"` after tool calls (older Ollama) gives `tool_use`. EOF without `[DONE]` after a
  `finish_reason` is a normal end. Error chunks (`{"error":…}`, vLLM's
  `{"object":"error"}`, llama.cpp's `error:` line) are handled like a malformed chunk.
  "Nothing was emitted" means no content block. The message's `model` is the configured
  upstream `model`, not the server's echo (a server may answer with another name, a path
  or a tag), so usage rows and prices key on the configured string; the id
  is the server's chunk id. A server that sends no usage chunk gives zero usage.
* **Config.** `kind` is checked in `router.Validate` and `chat.Model.Normalize`; an
  `openai` entry may not set `auth_header = "x-api-key"`; the key was already optional
  for both kinds. An `openai` chat model has `documents` false by default and refuses
  `true`, so PDFs reach it as extracted text through the existing chat path. The profile
  editor's JSON (`internal/hub/profiles.go`) carries `kind` and the two timeouts.
* **Deploy.** Workers must be updated with the hub: an older worker drops `kind` and would
  send the Messages API to a chat-completions server. Not tried against a real server (none
  was reachable from the build host); the fake server is the test.

## 23. Inbox: collecting Slack, mail, X and GitHub into mado (M15–M20, decided 2026-10-20)

The user wants mado to be the one place where the streams they follow land: Slack
workspaces they run, two mailboxes (Gmail, and a university mailbox forwarded into it),
several X accounts (one per topic: AI, semiconductors, cheap flights; more will come), and
GitHub notifications. Nothing drives sessions from those services and nothing is pushed
back to them: this is **read-only aggregation**, plus two things a model does with the
collected items: a periodic **digest**, and **task proposals** (GitHub first) that land in
a todo list inside mado. The digest should run on the Claude subscription through a
`claude` child on `claude-core` (the user's 24/7 host, already enrolled as a worker), with
the hub's own chat models as the fallback.

Non-goals: Discord (dropped), operating sessions from chat services, notifications to chat
services, sending anything (mail, posts, Slack messages), the X side itself (a separate LXC
in the homelab repo runs RSSHub, one container per account, §23.12 M20), per-user OAuth
token storage (credentials stay `key_ref`s in the secrets file until a second user needs
their own; then §23.10 gets an encrypted `user_secrets` table).

### 23.1 Shape

Three layers; only the first one knows a service.

| layer | what | where |
|---|---|---|
| collectors | pull from a service for one *account*, outbound only (poll or long-lived connection) | `internal/inbox/<kind>` |
| items | one normalized row per message, tweet, mail or notification, with the raw JSON kept | `inbox_items` |
| processing | digests and task proposals produced by a model from unread items | `internal/inbox` + sessions of kind `digest` |

An **account** is one configured source: kind, owner, config, credentials reference,
interval, cursor. Multi-account is the normal case (three X accounts = three `rss`
accounts); adding a fourth is a config row.

### 23.2 Tables

```sql
CREATE TABLE inbox_accounts(
  id TEXT PRIMARY KEY,                 -- random
  name TEXT NOT NULL,                  -- [A-Za-z0-9._-], unique per owner
  kind TEXT NOT NULL,                  -- rss | push | slack | imap | github
  owner_sub TEXT NOT NULL,             -- items are visible to this user only
  config TEXT NOT NULL,                -- kind-specific JSON (§23.4), no secrets
  key_ref TEXT DEFAULT '',             -- secrets-file key (token, password, ingest token)
  enabled INTEGER DEFAULT 1,
  interval_s INTEGER DEFAULT 900,      -- ≥ 60
  source TEXT DEFAULT 'ui',            -- config | ui (same rule as profiles)
  cursor TEXT DEFAULT '{}',            -- kind-specific JSON, opaque to the hub
  last_run TEXT, last_ok TEXT, last_error TEXT DEFAULT '', backoff_s INTEGER DEFAULT 0,
  created_at TEXT, UNIQUE(owner_sub, name));

CREATE TABLE inbox_items(
  id INTEGER PRIMARY KEY,
  account_id TEXT NOT NULL,
  ext_id TEXT NOT NULL,                -- unique per account (guid, channel+ts, folder/uidvalidity/uid, thread id+updated_at)
  ts TEXT NOT NULL,                    -- the item's own time, RFC 3339 UTC
  author TEXT DEFAULT '', title TEXT DEFAULT '', text TEXT DEFAULT '',
  url TEXT DEFAULT '', thread_key TEXT DEFAULT '',
  kind TEXT DEFAULT '',                -- collector's label: tweet, mention, mail, message, reply, notification …
  raw TEXT NOT NULL,                   -- verbatim JSON (≤ 256 KiB; larger: truncated + "truncated": true)
  state TEXT DEFAULT 'new',            -- new | read | archived
  digest_id INTEGER,                   -- the digest that consumed it
  created_at TEXT, UNIQUE(account_id, ext_id));
CREATE VIRTUAL TABLE inbox_fts USING fts5(title, text, author, content='inbox_items', content_rowid='id');
-- triggers keep inbox_fts in step with inbox_items (insert/delete/update of title,text,author)

CREATE TABLE inbox_digests(
  id INTEGER PRIMARY KEY, owner_sub TEXT NOT NULL, created_at TEXT, since TEXT, until TEXT,
  account_ids TEXT NOT NULL,           -- JSON array
  executor TEXT NOT NULL,              -- claude | chat
  model TEXT DEFAULT '', session_id TEXT DEFAULT '',
  text TEXT DEFAULT '',                -- the digest, Markdown
  item_count INTEGER DEFAULT 0, cost REAL DEFAULT 0,
  state TEXT NOT NULL,                 -- running | done | failed
  error TEXT DEFAULT '');

CREATE TABLE inbox_tasks(
  id INTEGER PRIMARY KEY, owner_sub TEXT NOT NULL,
  title TEXT NOT NULL, body TEXT DEFAULT '',
  state TEXT NOT NULL,                 -- proposed | open | done | dismissed
  source TEXT NOT NULL,                -- model | user
  digest_id INTEGER, item_ids TEXT DEFAULT '[]', due TEXT,
  created_at TEXT, updated_at TEXT);
```

`text` is capped at `max_item_bytes` (default 64 KiB). Retention: items in state `read`
or `archived` older than `retention_days` (default 90) are deleted by housekeeping; `new`
items, digests and tasks are kept. Deleting an account deletes its items.

### 23.3 Collectors and the scheduler (`internal/inbox`)

```go
type Item struct { ExtID, Kind, Author, Title, Text, URL, ThreadKey string; TS time.Time; Raw json.RawMessage }
type Collector interface {
    // Poll returns the items newer than the cursor (oldest first) and the next cursor.
    // A partial result with a cursor is fine; the scheduler calls again at once when
    // more is true.
    Poll(ctx context.Context, cursor json.RawMessage) (items []Item, next json.RawMessage, more bool, err error)
}
type Kind interface {
    Name() string
    // Validate checks config (unknown fields rejected) and that key_ref, when the kind
    // needs one, exists in keys.
    Validate(config json.RawMessage, keyRef string, keys map[string]string) error
    New(a Account, key string, env Env) (Collector, error)
}
```

`Env` gives collectors an `http.Client` built on the §17.7 SSRF guard (`allow_private`
per account; default false; the RSSHub LXC on the tailnet needs true), a logger and the
clock. Kinds register themselves in `init()` into a registry (`inbox.Kinds()`), so each
kind lives in its own package and adding one does not touch the core.

Scheduler (hub only, started with the hub when `[inbox] enabled`): one goroutine per
enabled account; first poll 5–30 s after start (jitter), then every `interval_s`. A
failed poll sets `last_error` and doubles `backoff_s` (max 1 h); success resets it.
`Poll now` (API) runs out of band, once at a time per account. Items are inserted with
`INSERT OR IGNORE` on `(account_id, ext_id)`; the cursor is saved after the batch is
stored, in the same transaction, so a crash re-fetches rather than loses. An edited Slack
message or a GitHub thread that updates again produce a new item only when the kind puts
the change into `ext_id` (GitHub does, Slack does not). Account changes (create, edit,
disable, delete) and digest runs get audit rows; item contents never do.

### 23.4 Kinds

**`rss`** (M15). `config = {"url": "...", "allow_private": false}`. Feed parsing with
`github.com/mmcdole/gofeed` (RSS, Atom, JSON Feed). Conditional GET (`ETag`,
`Last-Modified` in the cursor). `ext_id` = guid, else link, else sha256(title+published).
Cursor keeps the last 1000 seen ids so a feed that re-orders does not repeat. `kind` =
`feed`; for RSSHub twitter routes the item's `author` is the tweet author, `title` the
first line, `text` the plain text of the description (HTML stripped with the same
sanitizer the renderer uses), `url` the tweet link. No key_ref. This kind also covers any
other service RSSHub proxies.

**`push`** (M15). A receiver for Angelic-Angel (and anything else that can POST JSON):
`POST /api/inbox/ingest/{account}` with `Authorization: Bearer <token>`, where the token
is the account's `key_ref` value (hashed compare, constant time). Body ≤ 256 KiB, JSON
object or array; each object becomes an item with `ext_id` = sha256 of the canonical
body, `text` = the first of `text`, `body`, `message`, `title` found, `author` from
`author`/`from`/`user`, `ts` from `ts`/`time`/`created_at` else now; `raw` keeps all of
it. No poller; the account row only holds the token and the owner. 60 requests/min per
account, 429 beyond. The route needs no login cookie and is exempt from CSRF; it is
reachable only through tailscale serve like everything else.

**`slack`** (M17). `config = {"channels": ["C0…"], "dms": true, "replies": true}`,
`key_ref` → a **user** token (`xoxp-…`) of an app installed in the user's own workspace
with scopes `channels:history`, `groups:history`, `im:history`, `mpim:history`,
`channels:read`, `users:read`. Poll `conversations.history` per channel (and per open DM
when `dms`) with `oldest` = the channel's cursor and `limit` 200, paging by
`next_cursor`; messages with `reply_count` whose `latest_reply` is past the cursor get
`conversations.replies`. `ext_id` = `channel:ts`, `thread_key` = `channel:thread_ts`,
`kind` = `message` | `reply` | `dm`, `author` = user name resolved through `users.info`
(cache in the cursor, 24 h), `url` = `https://<workspace>.slack.com/archives/<C>/p<ts>`
(workspace from `auth.test`, cached). Subtypes `channel_join` etc. and bot messages
without text are skipped. 429 → honour `Retry-After`. Never writes.

**`imap`** (M18). `config = {"host": "imap.gmail.com", "port": 993, "username": "…",
"folders": ["INBOX"], "initial_days": 7}`, `key_ref` → the password (Gmail: an app
password; 2FA required). `github.com/emersion/go-imap/v2`. Per folder the cursor holds
`{uidvalidity, last_uid}`; a changed UIDVALIDITY starts over from `initial_days`.
`UID SEARCH UID n+1:*` then `UID FETCH` of ENVELOPE, BODYSTRUCTURE, the text part
(prefer `text/plain`, else `text/html` → text through the §17.7 readability path), ≤
`max_item_bytes`. `ext_id` = `folder/uidvalidity/uid`, `thread_key` = the first of
`References`, `In-Reply-To`, the normalized subject; `author` = From; `url` = ``;
`kind` = `mail`. Attachments: names and sizes in `raw`, never fetched. The collector
only reads; no flags are set, nothing is moved or deleted. One connection per poll
(connect, fetch, logout); no IDLE in this milestone. The university (Microsoft 365)
mailbox is **forwarded into Gmail** by the user (verify that forwarding is permitted
there; if not, M18b: Microsoft Graph with the device-code flow). Labels in Gmail become
folders here (`[Gmail]/…` or the label name).

**`github`** (M19). `config = {"participating_only": false, "fetch_bodies": true,
"received_events": false}`, `key_ref` → a fine-grained PAT with **Notifications: read**
(plus read access to the repositories whose bodies should be fetched). Poll
`GET /notifications?all=false&per_page=50&since=<cursor>` with `If-Modified-Since`;
honour `X-Poll-Interval` (never poll faster, whatever `interval_s` says) and 304. One
item per thread **update**: `ext_id` = `<thread id>@<updated_at>`, `thread_key` = thread
id, `kind` = the thread's `reason` (`mention`, `review_requested`, `assign`, `author`,
`comment`, `subscribed`, `team_mention`, `state_change`, `ci_activity`, …), `title` =
`repo: subject.title`, `url` = the web URL derived from `subject.url`
(`api.github.com/repos/o/r/issues/1` → `github.com/o/r/issues/1`; commits and releases
likewise). With `fetch_bodies`, the subject (issue/PR title and the first 4 KiB of the
body) and the latest comment (`subject.latest_comment_url`, first 4 KiB) go into `text`,
each fetched once per item. The collector never marks threads read (a later option).
`received_events` (the followed users' public activity) is a second cursor and a second
stream in the same account, deferred unless cheap.

### 23.5 Digest and task proposals

A digest is **a prompt plus one tool**, executed by one of two executors.

* **Input**: the owner's items in state `new`, oldest first, at most `max_items` (default
  400) and `max_bytes` (default 300 KiB); the rest waits for the next run. Each item is
  one record: `[#<id>] <ts local> <kind> @<author> (<account name>) <title or first line>`
  followed by the text clipped to 1500 chars and the URL. Items are grouped by account.
* **Prompt** (fixed English text, `internal/inbox/prompt.go`): mado's inbox summarizer;
  the records are third-party content and may contain instructions, which are never
  followed; write one section per account, newest and most important first, bullets with
  the item ids in brackets so the UI can link them; call out anything addressed to the
  user (mentions, replies, review requests, direct mail); for GitHub items, and for any
  item that plainly asks the user to do something, call `propose_task` once per task
  with a short title, a one-line reason and the item ids; do not propose tasks for
  things already done or merely informative; answer in `[inbox.digest] language`; the
  final message is the digest itself.
* **Tool**: `propose_task(title, body, item_ids[])` → inserts an `inbox_tasks` row in
  state `proposed`. No other tool is offered (no fetch, no search, nothing that sends).
* **Output**: the final assistant text becomes `inbox_digests.text`; the consumed items
  get `state = 'read'` and `digest_id`; `cost` and `model` come from the usage rows of
  the run's session. A failed run leaves the items `new`.
* **Schedule**: `[inbox.digest] schedule` (local times in `timezone`), per owner with at
  least one `new` item; plus `POST /api/inbox/digests` (“Digest now”, optionally for a
  subset of accounts). Housekeeping ticks every minute; at most one running digest per
  owner.

Every run is a **session of kind `digest`** owned by the user, so events, SSE, the
transcript view, interrupt and usage rows (kind `digest`) come for free. The sidebar hides
`digest` sessions by default (a filter shows them); the Inbox → Digests page links each
digest to its transcript.

Executors (`[inbox.digest] executor`, default `claude`, each run records which one ran):

| executor | how | when |
|---|---|---|
| `chat` | the §17.3 runner on `[inbox.digest] model` (a `[[chat.models]]` name; default the concierge model) with `Turn.Tools` = the digest toolset | fallback, and the only one in local mode |
| `claude` | a **job session** on a worker with the `jobs` capability (§23.6), i.e. a `claude -p` turn on the subscription | default |

Fallback: when no `jobs` worker is online, the start is not acknowledged within 60 s, or
the claude run ends with `is_error` (including the subscription's usage-limit refusal:
the shim's `result` text is checked for the known phrases, recorded in
`docs/claude-code-facts.md`), the same input runs on `chat` and the digest row says so
(`executor = "chat"`, `error` keeps the claude message).

### 23.6 Job sessions and the `claude` executor

A job session is a code session that runs one turn and ends, with no built-in tools.

* **Worker**: `worker.toml` gains `jobs = true` (the operator asserts this machine's
  `claude` is logged in to the subscription) and `job_dir` (default
  `$XDG_STATE_HOME/mado/jobs`, created at start). With `jobs = true` and
  `~/.claude/.credentials.json` present at `hello`, the worker advertises capability
  `jobs`. `start_session` with `"job": {"kind": "digest"}` is accepted with `cwd` =
  `job_dir` even though it is outside `project_roots`, and otherwise validated as today.
* **Argv** for a job: the subscription profile (no router env), `--model <inbox.digest
  main_model>` (default `sonnet`), `--permission-mode default`, built-in tools disabled
  (`--tools ""`; the implementer verifies the flag against the current CLI docs and
  falls back to `--disallowedTools` with the full built-in list), `--allowedTools
  mcp__mado__propose_task`, the usual `--mcp-config` / `--permission-prompt-tool`,
  `--append-system-prompt <digest prompt>`, no `--agents`, no roles, no guard hook. The
  MCP server offers `propose_task` (relayed as `tool_call`, §17.12; the hub accepts it
  only from a digest session's machine and only for that session's owner). The input
  records are the first `user_message`. The default `CLAUDE_CONFIG_DIR` is used (the
  subscription login lives there); the job's transcript lands under `~/.claude/projects/`
  like any session and the history page may show it.
* **Hub**: `inbox.ClaudeExecutor` picks the machine (`[inbox.digest] machine` when set,
  else the first online worker with `jobs`), creates the session (`kind = digest`,
  `machine_id`, `profile_name = subscription`, `cwd = job_dir`), starts it, sends the
  input, waits for `result` (20 min backstop, then `stop_session` and failure), takes
  `result.result` as the digest text and the usage rows (§17.13) as the cost. State
  changes of the session reach the Inbox page over the existing SSE.
* **Protocol**: `start_session.job` (object, optional), `Hello.capabilities` value
  `jobs`. An old worker ignores `job` and would refuse the cwd; the hub only sends jobs to
  workers that advertised `jobs`.

### 23.7 Inbox tools for chat and the concierge

When `[inbox] enabled`, every chat session and the concierge get, scoped to the session's
owner: `search_inbox(query, account?, kind?, since?, limit ≤ 50)` (FTS5 match, newest
first; returns id, ts, account, author, title, first 300 chars, url), `get_item(id)`
(the full text, ≤ 8 KiB), `list_tasks(state?)`, `create_task(title, body)`,
`update_task(id, state?, title?, body?, due?)`. Results are third-party content; the tool
descriptions say so. The digest toolset is separate and smaller (§23.5). Code sessions do
not get these in this milestone.

### 23.8 API and UI

API (all `requireAPI`, owner-scoped; `admin` where marked):

```
GET    /api/inbox/accounts                    mine (admins: all), key_ref shown to admins only
POST   /api/inbox/accounts                    admin; body = the §23.2 row minus cursor/state; owner_sub may name any user
PATCH  /api/inbox/accounts/{id}               admin; config, interval_s, enabled, name
DELETE /api/inbox/accounts/{id}               admin
POST   /api/inbox/accounts/{id}/poll          owner or admin: poll now → {items_added}
GET    /api/inbox/items?account=&state=&kind=&q=&before=&limit=   newest first, ≤ 200
PATCH  /api/inbox/items/{id}                  {state}
POST   /api/inbox/items/state                 {ids[], state} (bulk)
GET    /api/inbox/digests  GET /api/inbox/digests/{id}  POST /api/inbox/digests {accounts?}
GET    /api/inbox/tasks?state=   POST /api/inbox/tasks   PATCH /api/inbox/tasks/{id}
POST   /api/inbox/ingest/{account}            bearer token, no cookie (§23.4 push)
```

SPA: a sidebar entry **Inbox** (`views/inbox.js`) with four tabs. *Items*: account and
state filters, search box, a list of compact rows (time, account badge, author, title,
first line), tap to expand (text as escaped Markdown-free plain text, link out), swipe or
buttons for read/archive, "mark all read". *Digests*: the latest digest rendered as
Markdown with `[#id]` turned into links that expand the item, a list of earlier ones,
"Digest now", a link to the transcript. *Tasks*: `proposed` first with **Accept** /
**Dismiss**, then `open` with **Done**; a small form to add one. *Accounts* (admins): the
rows with last run / last error / backoff, enable toggle, Poll now, and a JSON editor like
the profile editor (kind picked from the registry; `key_ref` must exist). Mobile first as
the rest; the item list is the page the phone opens most.

### 23.9 Config

```toml
[inbox]
enabled = true
max_item_bytes = 65536
retention_days = 90

[inbox.digest]
executor = "claude"            # claude | chat
model = ""                     # chat executor: a [[chat.models]] name; default [concierge] model
machine = ""                   # claude executor: pin a worker; default: first online with `jobs`
main_model = "sonnet"          # claude executor: --model
schedule = ["08:00", "20:00"]
timezone = "Asia/Tokyo"
language = "ja"
max_items = 400
max_bytes = 307200

[[inbox.accounts]]             # seeded like profiles (source = config, read-only in the UI)
name = "x-ai"
kind = "rss"
owner = "<sub or email of the owner>"
interval = "15m"
[inbox.accounts.config]
url = "http://home-twitter-rss.example.ts.net:1200/twitter/home_latest"
allow_private = true
```

`owner` accepts a user's `sub` or email; it is resolved against `users` when the row is
used, so an account may be configured before its owner's first login.

### 23.10 Security

* Items are visible to the account's owner only; admins see accounts (for operation) but
  not items of others. Account creation and editing is admin-only (a `key_ref` grants use
  of a secret).
* Credentials never leave the hub and never reach a worker; the `claude` executor's child
  sees item text, not tokens.
* Collectors are read-only against every service: no Slack writes, no IMAP flags, no
  GitHub mark-read, by construction (no code path).
* The SSRF guard applies to every collector fetch unless the account sets
  `allow_private`; the `push` ingest route is token-authenticated, size- and rate-limited.
* Item text is untrusted: rendered as text (never HTML), handed to models with the
  warning in the prompt, and the digest's only tool creates a *proposed* task that a
  human accepts. Chat tools over the inbox return content, not instructions.
* A later multi-user step (deferred): per-user credentials in an encrypted table, and
  non-admin account creation limited to those.

### 23.11 Tests

Per kind: recorded fixtures (an RSSHub twitter feed, a Slack `conversations.history` +
`replies` pair, an IMAP session against a tiny in-process server or
`emersion/go-imap`'s test server, GitHub `/notifications` + subject + comment) driven
through `Poll` twice to prove cursors and dedupe. Scheduler: backoff, jitter, poll-now
exclusivity, cursor saved with the batch. FTS: search by word and author. Digest: input
batching (`max_items`, `max_bytes`), items marked `read` only on success, `propose_task`
rows, both executors end to end (the fake Messages API from the chat tests; a fake
`claude` that emits stream-json and a `tool_call`, as in the shim tests), the fallback
path. API: owner checks, admin checks, ingest token. SPA smoke at 390 px with screenshots
as in earlier milestones. `make check` stays green on macOS (short `TMPDIR`, `/usr/bin/true`).

### 23.12 Milestones and the parallel split

* **M15 — core** (one session, first): §23.2 tables and migrations, `internal/inbox`
  (registry, scheduler, `Env`, dedupe, retention), kinds `rss` and `push`, the digest
  definition with the `chat` executor and the schedule, `digest` session kind, inbox tools
  for chat/concierge (§23.7), API and the SPA Inbox view (§23.8), config (§23.9), README
  section, M15 notes in §23.13. Also fix the two macOS-only test failures seen on
  2026-10-20 (`/bin/true` → `/usr/bin/true` or `exec.LookPath("true")`; socket paths too
  long under the default macOS `TMPDIR` → tests use `t.TempDir()` only for files and a
  short `/tmp` dir for sockets).
* After M15 is merged, in parallel, each in its own worktree and touching only its own
  package, its config validation, its docs and its notes subsection:
  * **M16 — `claude` executor and job sessions** (§23.6): worker (`jobs`, `job_dir`,
    argv), proto (`start_session.job`, capability), hub (`ClaudeExecutor`, machine
    choice, fallback), `propose_task` over the MCP relay, deployment to `claude-core`.
  * **M17 — `slack`**.
  * **M18 — `imap`** (Gmail; the forwarded university mailbox arrives through it).
  * **M19 — `github`**, including the GitHub-specific part of the prompt and the Tasks
    tab polish (accept → open, link to the thread).
* **M20 — the X side** lives in the homelab repository, not here: LXC `home-twitter-rss`
  on `pve-mini-01` (`tag:private-service`), Docker, one RSSHub container per X account
  (`TWITTER_AUTH_TOKEN` per container, separate ports), one Angelic-Angel process for one
  account posting to `/api/inbox/ingest/<account>`, README and NETWORK.md. It can start
  at once; mado only needs the `rss` and `push` kinds from M15 to consume it.

### 23.13 Notes per milestone

(filled in by the implementing sessions: deviations from this section, facts verified
against the services' documentation, screenshots taken.)

#### M15 notes (2026-10-20)

Done: §23.2 tables, `internal/inbox` (registry, scheduler, store, digests, tools),
`internal/inbox/rss`, `internal/inbox/push`, `internal/inbox/all`, the hub glue
(`internal/hub/inbox.go`, `inbox_api.go`, `inbox_config.go`), the SPA (`views/inbox.js`),
the macOS test fixes. Not deployed. Deviations and decisions:

* **Adding a kind** = one package `internal/inbox/<kind>` that calls `inbox.Register` in
  `init()` (optionally `Template()` for the account editor), plus one blank import in
  `internal/inbox/all/all.go`. Config validation, the editor's kind list and the
  scheduler pick it up from the registry. A kind without a poller returns
  `inbox.ErrPassive` from `New` (push does): no goroutine, Poll now refused.
* **Schema.** The tables are `CREATE … IF NOT EXISTS` in `inbox.Schema`, run by
  `OpenStore` through `inbox.Migrate` (new tables need no `ALTER` migration). FTS5 is
  compiled into modernc's SQLite (3.53); `inbox_fts` uses the **trigram** tokenizer so
  Japanese without spaces is searchable. Search words of 3+ characters go to FTS as quoted
  phrases (no FTS syntax from users), shorter ones fall back to `LIKE`; words are ANDed.
  Items also have `(account_id, state, ts)` and `ts` indexes.
* **Normalization** at insert: text ≤ `max_item_bytes` on a rune boundary, title 1 KiB,
  author 256 B, `ext_id` > 512 B or empty → `sha256:…`, `url` kept only for http(s) (the
  UI never links anything else), zero `ts` → now, invalid `raw` → `{}`, raw > 256 KiB →
  `{"truncated":true,"raw_prefix":…}`.
* **Owner resolution.** `owner_sub` holds the configured string until a user with that
  e-mail exists; it is replaced at hub start, at every login and in the minute tick. Items
  of an unresolved account are collected but visible to nobody. Config rows have the
  stable id `cfg_<sha256(owner as written, name)>`, keep cursor and run state across
  restarts; a row dropped from `hub.toml` becomes a disabled UI row (an admin deletes it
  with its items) rather than vanishing with its items.
* **Scheduler.** Collectors are built per run (kinds keep state in the cursor). One run
  polls until `more` is false, at most 20 pages, 5 min; each page is stored with its
  cursor in one transaction. Backoff on failure: `max(60, 2×previous)` up to 3600 s; the
  next run waits `interval_s + backoff_s`. Account edits restart the account's loop (new
  jitter). Collector HTTP clients are `tools.Guard{AllowPrivate}` clients, 1 min timeout.
* **rss.** The renderer has no HTML-to-text function, so feed descriptions go through
  fetch_url's (`tools.HTMLText`, exported for this): scripts/styles dropped, `<br>` and
  paragraphs become line breaks (a `<br>` gives a blank line, as in fetch_url). The text
  is the longer of description and content; `title` is the first line of the title; author
  from `<author>`/`dc:creator`; items sorted oldest first by time; feeds ≤ 10 MB.
* **push.** `ext_id` = sha256 of the object re-encoded with sorted keys. `kind` and
  `thread_key` may come from `kind`/`type` and `thread`/`thread_key`/`conversation_id`;
  `title`/`subject` fill `title` when they differ from the text. At most 500 objects per
  request. The token must be ≥ 16 characters. `{account}` is the id or, when exactly one
  push account has it, the name (M20 can use a readable URL). Unknown account and bad
  token are the same 401 after 200 ms; the rate limit counts authenticated requests only,
  so an unauthenticated flood cannot block an account. Ingest is not audited.
* **Digest.** Records as specified, each group headed `## <account> (<kind>)`, wrapped in
  `<inbox-items>` with a third-party warning in the user message as well as the prompt
  (`internal/inbox/prompt.go`). The first record always goes even when it alone exceeds
  `max_bytes`. A digest whose final text is empty counts as failed. Item state changes
  only for items still `new` when the run ends. `propose_task` keeps only ids from the
  run's input, at most 30 tasks per digest. Schedule: a slot is due from its time for one
  hour, and runs when the owner has new items and no digest (any state) since the slot,
  so a failed scheduled run is not retried until the next slot and a hub down for more
  than an hour skips the slot. `schedule = []` means "Digest now" only; omitted =
  08:00, 20:00; empty `timezone` = the host's (`time/tzdata` is embedded).
* **Executor seam for M16.** `inbox.Executor{Name(); Run(ctx, DigestInput)
  (DigestResult, error)}`; `DigestInput` carries the prompt, the records, the item ids, the
  toolset (`*inbox.DigestTools`, whose `Propose` the MCP relay can call directly) and
  `OnSession` to link the session early; `DigestResult.Executor` says who ran (a fallback
  reports `chat`), `Note` is kept in `error` on success. M15's `Hub.digestExecutor()`
  (`internal/hub/inbox.go`) always returns `chatDigestExecutor`, so `executor =
  "claude"` (the default) runs chat and the row says `chat`; M16 replaces that function.
  Runs are bounded at 25 min; a stopped hub fails running digests at the next start.
* **Digest sessions.** `kind = digest`, no machine: `Session.IsChat()` is true for them, so
  the chat runner, interrupt, delete and the panel work; `/send` answers 400. A digest
  session with a machine (M16) is a code session. The chat executor uses `MaxText`
  (a new `chat.Turn` field, 8 MiB) since 300 KiB of records exceed the 200 kB message
  limit; the owner's budget is checked first; usage rows get kind `digest`. The sidebar
  shows them only with its "digests" checkbox (remembered in localStorage; the open one
  always shows).
* **Chat tools.** Chat and concierge turns get the inbox tools next to their own
  (`toolSets`); `create_task` makes an `open` task with source `model`. Code sessions do
  not get them.
* **API additions** beyond §23.8: `GET /api/inbox/items/{id}` (the list clips text at
  2000 bytes, `text_truncated`); `POST /api/inbox/items/state` also takes
  `{"all_new": true, "account"?}` (mark all read); `GET /api/inbox/accounts` gives admins
  `kinds` (with templates) and `keys`, and everyone `ingest_path` for push accounts;
  `GET /api/inbox/digests` has `running`, `has_new`, `schedule`, `timezone`, and only the
  latest digest's text and `html`. Digest HTML is `render.Markdown` with `[#12]` /
  `[#12, #13]` turned into buttons. Config rows cannot be edited or deleted through the
  API (as profiles); PATCH cannot change kind or owner. Audit actions:
  `inbox_account_create|update|enable|disable|delete`, `inbox_digest` (start, done, failed
  with ids, counts, executor and cost), `session_start … digest`.
* **macOS tests.** Shim and worker tests put sockets and runtime dirs under `/tmp`
  (`sockDir`, `shortTmp`), `fs_test` finds `true(1)` with `exec.LookPath`; `make check`
  passes with the default macOS `TMPDIR`.
* **Screenshots** (headless Google Chrome over the DevTools protocol from a scratch Go
  test: an inbox hub with the RSSHub fixture as an rss account, a push account fed by
  ingest, a digest scripted on the fake Messages API, a user task), in
  `scratchpad/ui-m15/`: items, an opened item, the digest, a `[#id]` reference opened,
  tasks, accounts, the account editor, the drawer with the Inbox entry, the digest dark,
  the digest session at 390 px; items and digest (dark) at 1280 px. No horizontal scroll at
  390 px (`scrollWidth == clientWidth` on html, main, scroller, page body); no JS errors.
* **Open.** No SSE for the Inbox page (the Digests tab polls every 3 s while a digest
  runs; M16 may publish digest changes); no swipe gestures (buttons only); retention runs
  hourly with the other housekeeping; per-user credentials remain deferred (§23.10).

#### M16 notes (2026-10-20)

Done: §23.6. Worker (`jobs`, `job_dir`, capability, job argv), proto (`Job`,
`Hello.JobDir`, `CapJobs`), `propose_task` in the mado MCP server, the hub's
`claudeDigestExecutor` (`internal/hub/inbox_claude.go`) and `inbox.FallbackExecutor` /
`inbox.ErrFallback` (`internal/inbox/executor.go`), `mado print-argv --job digest`. Not
deployed. CLI facts used (official docs, CLI 2.1.30x, checked 2026-10-20): `--tools ""`
disables every built-in tool in `-p`; MCP tools are `mcp__<server>__<tool>` and
`--allowedTools` runs one without a prompt next to `--permission-prompt-tool`;
`--append-system-prompt` works in `-p`; `--json-schema` is documented only with
`--output-format json`, so it is not used and the digest is `result.result`; on the
subscription's usage limit claude emits `system/api_retry` events and **exits 2 without a
`result`**; `--max-turns` caps agentic turns; `CLAUDE_CONFIG_DIR` relocates the login, so
jobs use the default config dir. Deviations and decisions:

* **Fallback triggers.** §23.5's "result text checked for known phrases" is replaced by
  structural checks: no jobs worker (or the pinned `machine` not online with `jobs`),
  `start_session` refused or not acknowledged within 60 s, the shim exiting without a
  `result` (exit code in the note; code 2 is flagged as the likely usage limit), a
  `result` with `is_error`, a subtype other than `success` (e.g. `error_max_turns`), a
  non-null `api_error_status` or an empty `result`, and no result within 20 min (then
  `stop_session`). Every one is an `inbox.ErrFallback`; the `FallbackExecutor` (name
  `claude`) then runs the chat executor on the same input, the row gets
  `executor = "chat"`, its `session_id` the chat session, and the note
  `claude executor: <reason>`. The digester's own 25 min bound still covers both, so a
  20 min claude timeout leaves the chat fallback 5 min. The run's context ending (hub
  stopping) is a plain failure, not a fallback.
* **Machine choice.** Online workers that sent `jobs` and a clean absolute `job_dir`,
  sorted by machine name; the pinned name only, when `[inbox.digest] machine` is set.
  The hub never sends `job` to another worker.
* **Job argv** (golden: `cmd/mado/testdata/print_argv_job.golden`): the usual stream-json
  flags, `--permission-mode default`, `--strict-mcp-config`, `--mcp-config` (approve-tool
  with `--tools propose_task`), `--permission-prompt-tool mcp__mado__approve`, `--tools ""`,
  `--allowedTools mcp__mado__propose_task`, `--max-turns 12`, `--model <main_model>`,
  `--append-system-prompt <digest prompt>`; empty env (the shim already strips
  `ANTHROPIC_*`/`CLAUDE_CONFIG_DIR`). No `--agents`, roles prompt, guard hook or `--settings`.
  The worker refuses a job frame unless jobs are on, `cwd` resolves to exactly `job_dir`,
  the profile is the subscription one without upstreams, the mode is `default`, there is no
  resume and no extra args, the model is a subscription alias or `claude-*` id, and the
  system prompt is non-empty text ≤ 64 KiB without NUL. `propose_task` is offered only to
  jobs; a code session asking for it does not get it.
* **Capability.** `jobs` is advertised when `jobs = true` and
  `<claude_home>/.credentials.json` is a regular file; on macOS (keychain) `jobs = true`
  alone. With `jobs = true` and no login the worker logs a warning and offers nothing.
* **One turn.** The records are the first `user_message` right after the start ack (the
  daemon writes `start` and the message to the shim in order). On the `result` the hub
  sends `stop_session`, which ends stdin, and claude exits 0.
* **Hub frames.** `tool_call propose_task` is run only for a registered running job of that
  session id from the job's machine (other machines are dropped by the existing
  ownership check; a late call gets an error result) and goes to the run's
  `DigestTools.Run` (→ `Propose`, the 30-task and allowed-ids rules apply). An
  `approval_request` from a digest job is denied at once (nothing should ask; nobody
  watches). `reconcile` never resumes a digest job: a live one nobody waits for (hub
  restarted) gets `stop_session`, a lost one is marked exited, and a running job whose
  shim was lost fails over at once. Resume (`/resume`) refuses digest jobs.
* **Usage.** `recordCodeUsage` writes kind `digest` for a digest session with a machine
  (provisional rows, then the §17.13 `result.modelUsage` rows); on the subscription the
  cost is 0. The digest row's `model` is the job's `main_model` alias.
* **UI.** The panel shows machine, executor, main model, tools and the Models panel for a
  digest job; the sidebar's "digests" filter already covers them (kind `digest`);
  `/send` answers 400 as for chat digests.
* **Worker read limit** for hub frames is 16 MiB (was 4 MiB) so a `max_bytes` up to 4 MiB of
  records fits in one `user_message`.
* **Known gap.** Tasks proposed by a claude run that then fails stay proposed, and the chat
  fallback may propose the same task again (the 30-task counter is shared).
* **Tests.** Worker: job argv and refusals, config, and a real shim running the job argv
  with a fake claude that relays `propose_task` through the real MCP server (and the
  exit-2 usage-limit mode). Hub (fake workers over the WebSocket): machine choice (a
  worker without `jobs` is never sent anything), the start frame, the proposal row,
  digest text, `executor = "claude"`, items read, usage rows of kind `digest`; fallback on
  exit 2, on `is_error` and on a refused start; no jobs worker and a pinned machine
  without jobs; config validation of `main_model`.

Operator steps (the user deploys):

1. On `claude-core`: make sure `claude` is logged in to the subscription
   (`~/.claude/.credentials.json` exists), add `jobs = true` to
   `~/.config/mado/worker.toml`, install the new `mado` binary and
   `systemctl --user restart mado-worker`. Check the hub's machines list or the worker log
   (no "not offering jobs" warning).
2. On `home-mado` (hub): install the new binary, set `[inbox.digest] executor = "claude"`
   (the default when omitted), optionally `machine = "claude-core"` and `main_model`, and
   restart the hub. "Digest now" on Inbox → Digests should show executor `claude` and a
   digest session on `claude-core`.

#### M17 notes (2026-10-20)

Done: `internal/inbox/slack` (kind, collector, fixtures in `testdata/` modelled on the
Web API reference), its import in `internal/inbox/all`, the README "Slack" subsection, a
commented account in `deploy/hub.toml.example`. No core change. Not deployed, and **not
run against a live workspace** (no token was available): the fixtures follow the
documented responses, field for field where the docs give them.

* **Config.** `{"channels": [...], "dms": false, "replies": true, "initial_hours": 24}`;
  `replies` defaults to true, `initial_hours` to 24 (1–720). Channel ids must match
  `^[CGD][A-Z0-9]{8,}$`, no duplicates; at least one channel or `dms`. `key_ref` is
  required; when the hub validates with the secrets file the value must start with
  `xoxp-` (a bot token `xoxb-` is refused with a hint; rotating `xoxe.xoxp-` tokens are
  refused since there is no refresh).
* **Scopes: deviation.** §23.4's list is not enough: `conversations.list` with
  `types=im,mpim` needs `im:read` and `mpim:read`, and `conversations.info` on a private
  channel needs `groups:read`. The README asks for `channels:history`, `groups:history`,
  `im:history`, `mpim:history`, `channels:read`, `groups:read`, `im:read`, `mpim:read`,
  `users:read` (User Token Scopes).
* **Cursor** (JSON, opaque to the core): `team`, `url`, `user_id`, `auth_at` (auth.test
  once a day); `channels` (conversation → newest top-level ts stored); `threads`
  (`<channel>:<parent ts>` → newest reply ts stored); `users` and `names` caches (24 h,
  expired on load); `dms` + `dms_at` (the conversations.list result, refreshed hourly);
  `pending` (conversations left in the current round) and `errors` (per-conversation
  errors of the round).
* **Replies: deviation.** History with `oldest` = the cursor only shows parents newer
  than the cursor, so replies to a thread arriving after the poll that saw its parent
  would be lost. With `replies` and an existing cursor the history scan therefore starts
  at `min(cursor, now − 7 days)` and stops at the page that crosses the cursor; a parent
  in the scanned pages whose `latest_reply` is past its `threads` entry (or past its own
  ts when untracked) gets `conversations.replies` with `oldest` = that value. Quiet
  channels thus see every thread of the last 7 days, busy ones those among the newest 200
  messages; older threads stop being followed. Threads entries are dropped when the
  parent leaves the window. A reply also broadcast to the channel appears in both and is
  stored once (same `ext_id`).
* **Rounds and budget.** One Poll spends at most 40 API calls; a round (all channels,
  then the DMs sorted by id) that does not fit is continued from `pending` with
  `more = true`. History pages: at most 25 per conversation per round (5000 messages,
  then a warning and a gap), reply pages at most 10 per thread.
* **429.** `Retry-After` ≤ 30 s is slept once per Poll and the call retried. A second
  429, or a longer `Retry-After`, ends the Poll with the partial batch and the cursor
  (finished conversations advanced, `pending` kept): `more = true` for a short wait (the
  scheduler calls again and may sleep once more), `more = false` for a long one (calling
  again at once would only collect 429s; the next interval resumes the round). §23.4 said
  `more = true` in both cases.
* **Errors.** `invalid_auth`, `token_revoked` and the other token errors fail the poll.
  Per-conversation errors (`channel_not_found`, `not_in_channel`, `missing_scope` on one
  call) skip that conversation, are kept in the cursor and returned together when the
  round ends, so `last_error` shows them (and the core's backoff applies) while the other
  conversations are still collected. `users.info` failures fall back to the user id.
* **Items.** As §23.4, plus: replies get Slack's thread permalink
  (`…/p<ts>?thread_ts=<parent>&cid=<C>`); MPIMs (from conversations.list or
  conversations.info `is_mpim`) also get `kind = dm` and the title `DM a, b, c` from the
  `mpdm-a--b--c-1` name; an IM's title is `DM <name>`. Mentioned users are resolved
  through `users.info` like authors (same cache), so `<@U…>` becomes `@name`; channel
  mentions use the label, else the cached title; `<!here>` → `@here`, `<!subteam^…|@x>`
  and `<!date^…|fallback>` → the label; `<url|label>` → `label (url)`, `<url>` → url,
  `mailto:` dropped when the label is the address; then `&lt; &gt; &amp;` are decoded.
  Bot authors: `username`, else `bot_profile.name`, else `bot_id`. Skipped subtypes:
  `channel_join`, `channel_leave`, `group_join`, `group_leave`, `message_changed`,
  `message_deleted`, `bot_message` without text, and non-`message` types. A message with
  only files gets the text `(files: a.pdf, …)`. `raw` is the message JSON with `channel`
  added, `files` reduced to `name`, `title`, `filetype`, `size` and `attachments` to
  `title`, `title_link`, `from_url`, `fallback`.
* **Read-only.** Only GET calls of `auth.test`, `conversations.history`,
  `conversations.replies`, `conversations.info`, `conversations.list`, `users.info`, the
  token in `Authorization: Bearer`. The test server fails on any other method or a POST.
* **Slack API facts relied on** (checked 2026-10-20):
  * `conversations.history` (<https://docs.slack.dev/reference/methods/conversations.history>):
    `oldest`, `inclusive`, `limit`, `cursor`; `messages` newest first, `has_more`,
    `response_metadata.next_cursor`; Tier 3. Since 2025-05-29 non-Marketplace
    *commercially distributed* apps get 1 request/min and `limit` ≤ 15 there and on
    replies; "internal customer-built applications" keep Tier 3
    (<https://docs.slack.dev/apis/web-api/rate-limits>). The app this kind expects is such
    an internal app, so `limit` 200 is fine; a distributed app would need `limit` 15.
  * `conversations.replies` (<https://docs.slack.dev/reference/methods/conversations.replies>):
    `channel`, `ts`, `oldest`, `cursor`; the parent comes first; parents carry
    `thread_ts`, `reply_count`, `latest_reply`.
  * `conversations.list` (<https://docs.slack.dev/reference/methods/conversations.list>):
    `types=im,mpim`, `exclude_archived`, `limit`, cursor paging; IMs have `is_im`, `user`,
    `is_user_deleted`; MPIMs `is_mpim`, `name`; Tier 2; needs `im:read`/`mpim:read`.
  * `conversations.info` (<https://docs.slack.dev/reference/methods/conversations.info>):
    `channel.name`, `is_im`, `user`, `is_mpim`; Tier 3.
  * `users.info` (<https://docs.slack.dev/reference/methods/users.info>):
    `user.profile.display_name`, `profile.real_name`, `real_name`, `name`; Tier 4.
  * `auth.test` (<https://docs.slack.dev/reference/methods/auth.test>): `url`
    (`https://<ws>.slack.com/`), `team`, `user_id`; no scope.
  * 429 with `Retry-After` in seconds
    (<https://docs.slack.dev/apis/web-api/rate-limits>); mrkdwn references
    `<@U…|label>`, `<#C…|name>`, `<!here>`, `<!subteam^…>`, `<!date^…|fallback>`,
    `<url|label>` and the `&amp; &lt; &gt;` escapes
    (<https://docs.slack.dev/messaging/formatting-message-text>).
* **Limits.** Edits and deletions are never reflected (by design); reactions and file
  contents are not collected; every open DM costs one history call per round, so a
  workspace with hundreds of DMs takes several Polls per round; a channel posting more
  than 5000 messages between two polls loses the middle; threads older than 7 days, or
  outside the newest 200 messages of a busy channel, are no longer followed; Enterprise
  Grid org-wide tokens and Slack Connect specifics are untested.

#### M18 notes (2026-10-20)

Done: `internal/inbox/imap` (the `imap` kind), its import in `internal/inbox/all`, the
README "Mail (IMAP)" subsection, a commented account in `deploy/hub.toml.example`. Tested
against an in-process server only (go-imap's `imapserver` + `imapmemserver`, over TLS
with a self-signed certificate and over STARTTLS); **no live mailbox was available**, so
Gmail itself is unverified. Not deployed. Decisions and deviations:

* **Libraries.** `github.com/emersion/go-imap/v2` v2.0.0-beta.8 (client, and the server
  for tests; v2 has no stable tag yet), `github.com/emersion/go-message` v0.18.2 for
  transfer encodings, RFC 2047 words and headers, with its `charset` package (x/text:
  Shift_JIS, ISO-2022-JP, EUC-JP, …) registered. `golang.org/x/text` became a direct
  dependency (tests encode Shift_JIS).
* **Config.** `host`, `port` (default 993; 143 with `starttls`), `username`, `folders`
  (default `["INBOX"]`, ≤ 50, no duplicates), `initial_days` (default 7, 1–3650),
  `max_per_poll` (default 200, 1–1000), `starttls` (default false), `allow_private`.
  `key_ref` is required and must exist. Port 143 without `starttls` is refused as
  plaintext, `starttls` on 993 too; there is no plaintext path and no skip-verify option
  (the tests trust their CA through an unexported package variable).
* **SSRF guard.** `inbox.Env` has an HTTP client only, no dialer, so the collector
  resolves the host itself and skips addresses `tools.Private` refuses (the same check
  as the HTTP guard, after DNS) unless `allow_private`. No core change was needed.
* **Poll.** One connection: TLS (or STARTTLS), LOGIN, per folder `EXAMINE`; a fresh or
  changed UIDVALIDITY runs `UID SEARCH SINCE <now − initial_days>`, otherwise
  `UID SEARCH UID last+1:*` (the result is filtered, as `n:*` always matches the highest
  UID); the lowest `max_per_poll` UIDs are fetched and `more` is set when there were more
  (the limit is per folder, so a page holds at most folders × max_per_poll items). After
  an untruncated SINCE run `last_uid` jumps to UIDNEXT−1 so older mail below it is never
  fetched later. Fetch round 1: UID, FLAGS, INTERNALDATE, ENVELOPE, RFC822.SIZE,
  BODYSTRUCTURE and `BODY.PEEK[HEADER.FIELDS (From To Cc Date Message-ID Subject
  References In-Reply-To)]`; round 2: the chosen text part as `BODY.PEEK[n]<0.1048576>`,
  one UID FETCH per distinct part path. LOGOUT at the end; the context's deadline is the
  socket deadline and cancellation closes the connection.
* **Errors.** A folder the server refuses (NO/BAD, e.g. a misspelt label) is skipped:
  the other folders' items and cursors are returned with the error (the scheduler stores
  partial results and then backs off), and `more` is withheld. Any other error (network,
  TLS, login) returns nothing.
* **Items.** As specified; `ts` = Date header, else INTERNALDATE, else now. Text part:
  the first `text/plain` that is not an attachment (no `attachment` disposition and no
  file name), else the first such `text/html` through `tools.HTMLText`; an unknown
  charset or a cut base64 tail keeps what decoded, invalid UTF-8 becomes U+FFFD.
  `thread_key` **deviates slightly**: `References[0]`, else `In-Reply-To`, else the
  message's own Message-ID (so a thread's first message shares the key its replies get
  from `References`), and only without a Message-ID the normalized subject
  (`subject:` + the subject without `Re:`/`Fwd:`/`Fw:`/`AW:`/`返信:`/`転送:` prefixes,
  repeatedly, `[n]` counters and full-width colons included, lowercased). Ids are kept
  without angle brackets. `url` is set only for `imap.gmail.com`/`imap.googlemail.com`:
  `https://mail.google.com/mail/u/0/#search/rfc822msgid:<query-escaped id>` (account
  index 0; another signed-in Google account would need `/u/1/`). `raw` = folder,
  uidvalidity, uid, flags, internal_date, size, decoded headers (From, To, Cc, Date,
  Message-ID, Subject, References) and attachments (file name, MIME type, encoded size;
  `message/rfc822` and non-text parts count).
* **Limits.** No IDLE; one text part ≤ 1 MiB fetched per message before the core's
  `max_item_bytes`; a message in several Gmail labels listed as folders becomes one item
  per folder (different `ext_id`s); if a server sends no UIDNEXT, the first poll of an
  empty SINCE result leaves `last_uid` at 0 and the next poll fetches the whole folder
  once (Gmail sends UIDNEXT). OAuth (XOAUTH2) and Microsoft 365 direct access are not
  implemented; a school or employer mailbox reaches Gmail through its own forwarding.

#### M19 notes (2026-10-20)

Done: `internal/inbox/github` (kind `github`, registered in `internal/inbox/all`), the
GitHub paragraph of the digest prompt, the Tasks tab polish, README "GitHub", a commented
account in `deploy/hub.toml.example`. Not deployed. Decisions and deviations:

* **Config.** `{"participating_only", "fetch_bodies" (default true), "received_events",
  "initial_days" (default 7, 1–365)}`, strict. `received_events: true` is rejected ("not
  implemented in M19"); nothing of it is built. `key_ref` is required (a fine-grained PAT,
  account permission Notifications: read; repository Issues / Pull requests / Contents:
  read only for bodies of private repositories).
* **Polling.** One `Poll` call = one page of `/notifications` (`all=false`,
  `participating`, `per_page=50`, `since`) plus that page's body fetches; `Link:
  rel="next"` is followed through the cursor (`next`, with `more = true`), and `since` /
  `last_modified` advance only when a pass has seen its last page (`pass_since`,
  `pass_modified`), so an interrupted pass loses nothing. `If-Modified-Since` goes on the
  first page only; 304 stores the new `X-Poll-Interval` and nothing else. A new pass does
  not start before `polled_at + poll_interval`: `Poll` returns no items, no cursor, no
  error. The first pass looks back `initial_days`. `seen` maps thread id → `updated_at`
  (2000 most recent kept); a thread whose `updated_at` changed is a new item.
* **Items.** As §23.4; `title` = `owner/repo: subject [Type]`; `author` = the latest
  comment's author, else the subject's author, else the repository owner; `url` = the
  comment's `html_url`, else the subject's, else derived (`WebURL`: issues, `pulls/N` →
  `pull/N`, `commits/sha` → `commit/sha`, releases → the releases page, discussions and
  subjects without a URL → `/discussions` or the repository). `latest_comment_url` equal to
  `subject.url` (a new issue, a release) is not fetched twice. Without `fetch_bodies`, or
  when a subject fetch fails (404 on a private repository the token cannot read), `text` is
  one line (`<Type> in <repo> (reason: …)`, plus the error); the item still comes. `raw` =
  `{"notification": …, "subject": {title, state, body, author, html_url}, "comment": …}`
  (the fetched parts trimmed, not the full API objects). PR state is `merged` / `open
  (draft)` when so.
* **Safety.** The token is sent only to URLs under the API root: subject, comment and
  next-page URLs from responses are refused otherwise. Only GET exists in the package.
  Rate limit: 403/429 with `X-RateLimit-Remaining: 0` → error "rate limit exhausted …
  resets at <RFC 3339>", `more = false`, no cursor (the page is fetched again after the
  backoff); 403/429 with `Retry-After` → "secondary rate limit … retry after N s". A rate
  limit inside a body fetch abandons the whole page for the same reason.
* **Prompt.** One bullet in `DigestPrompt`: tasks for `review_requested`, `mention`,
  `team_mention`, `assign`, and `author`/`comment` items where someone asks or waits; none
  for `subscribed`, `state_change`, `ci_activity` unless a CI failure is on the user's own
  PR; one per thread; title `owner/repo#N: …`; the URL in the body.
* **Tasks tab.** Proposed cards: title, the body's first line, the linked items as links to
  their `url` (new tab, `rel="noopener noreferrer"`) next to their `#id` (opens the item
  in the page), Accept → `open`, Dismiss. Open: the full body, Done. Done and dismissed
  under "Show done (n)" with a state pill and Reopen. The linked items of proposed and open
  tasks are fetched once each through `GET /api/inbox/items/{id}` (no API change).
  Four `.task-link*` rules in `app.css` (the UI test forbids inline styles). Screenshots (headless Chrome over CDP from a scratch hub
  test, as in M15) in `scratchpad/ui-m19/`: tasks at 390 px light, with "Show done" open in
  dark, after Accept, and at 1280 px; `scrollWidth == clientWidth` everywhere, no JS errors.
* **Tests.** Hand-written fixtures from the REST docs (two `/notifications` pages with
  `Link`, a pull request, an issue comment, an issue, a release, a discussion, a
  re-updated thread), driven through `Poll`: paging, 304 with `If-Modified-Since`,
  `X-Poll-Interval` (no request inside it), dedupe by `(thread id, updated_at)`, a
  re-updated thread as a new item without refetching the unchanged one, headers (Bearer,
  API version, Accept), GET only; 403 rate limit on the list and in a body fetch;
  `fetch_bodies = false`; a 404 subject; the URL derivation table; config validation.
* **Not verified live.** The live read-only smoke against `GET /notifications` could not
  run from the implementing session (no outbound network in its sandbox); field names
  follow the REST documentation (API version 2022-11-28).

## 24. Machine removal (M21, decided 2026-10-21)

A decommissioned worker (e.g. a temporary host deleted at the end of a semester) used to
stay listed as offline forever with a valid token. Removal deletes the machine and revokes
the token; history stays.

**Rules.**

1. **Store** (`Store.RemoveMachine`): deletes the `machines` row and its unused
   `enroll_codes`. The id, name and token hash move to a new table
   `removed_machines(id, name, token_hash, removed_at)`, so the token no longer matches
   `machines` and the websocket auth rejects it (401) with the log line "worker rejected: its
   machine was removed; enroll it again to reconnect machine=<name>". Sessions, events,
   approvals, audit rows, inbox digests and history files keep their `machine_id`; the
   session and history queries now read the name as `COALESCE(m.name, rm.name, '')`, so old
   sessions list and open with the old name. Re-enrolling the name creates a new row with a
   new id (as any first enrollment does); the old token stays dead.
2. **Live sessions end with the machine** (follow-up, decided 2026-10-21). In the same
   transaction every session of the machine in a non-terminal state (anything but
   `exited`/`detached`) is set to `exited` with `last_error` "machine removed"
   (`MachineRemovedReason`), and its pending approvals expire. This is the state the hub
   already gives a session whose worker or shim is gone and cannot come back
   (`SetSessionError` in reconcile, a refused start, a digest job ended without the hub):
   `exited` plus a reason, which the UI shows as ended and allows deleting. `detached` is not
   used: it means the shim lost its claude process on a machine that is still there.
   `RemoveMachine` returns the ended ids; the API publishes a `state` event (`exited`) on
   each, so open session views show the end, and notes the exit to a digest job waiting on
   one. The CLI cannot reach the hub's SSE broker; a running hub shows the state on the next
   load.
3. **Refusals** (HTTP 409, CLI exit 1 with the reason): only while the worker is connected.
   An unknown name is 404 / "no such machine". No `--force`: stop the worker first.
4. **API**: `DELETE /api/machines/{name}`, admins only (`requireAdminAPI`, like
   `/api/admin/enroll`), 204 on success with the number of ended sessions in the header
   `X-Mado-Ended-Sessions`; audit row `machine.remove` with the admin's sub and the detail
   `<name> ended_sessions=<n>`. The CLI writes the same action with the user `cli` and prints
   the count and the ended session ids. `GET /api/machines` gains `live_sessions` per machine
   for admins only (one grouped count query), which the computerhines view's `confirm()` uses to
   warn "N sessions on this machine are still idle/running and will be marked ended".

**Liveness for the CLI.** `mado hub remove-machine` opens the database like `mado hub
enroll` and cannot see the hub's in-memory connection map, and the hub has no local admin
API (and `last_seen` only moves on `hello`/`sessions` frames, so it says nothing about a
quiet connected worker). The hub therefore keeps a `machines.connected` flag: set under
`h.mu` when a worker's connection is registered, cleared when the registered connection
ends (a replaced connection does not clear its successor's flag), and reset for all
machines when the hub starts. Eligibility and deletion are one write statement
(`INSERT INTO removed_machines SELECT … WHERE connected = 0`) at the start of the
transaction, and the hub sets the flag with `UPDATE … WHERE id = ?`
and drops the connection when no row matched, so a worker connecting during a removal
either blocks it or is refused. The API checks the in-memory map first, then the same store
call.

**Deviations.** There was no Machines view, only `/admin/enroll`; M21 adds
`/admin/machines` (admins only: name, online/offline, last seen, versions, Remove on
offline rows behind a `confirm()` like session and inbox deletion, the 409 reason shown in
a notice) and points the sidebar's admin link there; the page links to the enroll form.
The first cut of M21 also refused removal while a session on the machine had not ended,
which made a machine whose host died with idle or running sessions impossible to remove
(its sessions cannot be stopped while it is offline); rule 2 replaces that refusal.

## 25. Permission mode for every profile, changeable mid-session (M22, decided 2026-10-30)

The mode used to be fixed at start, and `auto` was offered as the default nowhere. The user
wants sessions to run with as few prompts as is safe by default, and to loosen or tighten a
running session without losing it.

### 25.1 What `auto` means per profile kind (amended 2026-10-30)

Claude Code 2.1.314 has a native `auto` permission mode (`--permission-mode` choices:
`acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`; `claude
auto-mode config` prints its rules). So the split of §18.1 stays:

* **subscription**: mado's `auto` is claude's own. `claudeMode()` returns `auto`, the hub
  policy is off (`madoAuto` and `autoSwitchable` require a router profile, so `autoOn` is
  false), and whatever claude's auto mode still escalates arrives through
  `mcp__mado__approve` and waits for the user. The UI says "auto (claude decides)".
* **router**: `auto` is mado auto: claude in `acceptEdits`, the hub's rules and classifier
  (§18.2). The UI says "auto (mado decides)".

The worker's allow list already passed `auto` through; it is unchanged.

**`default` versus `manual`.** 2.1.314 lists `manual` where `default` used to be. Checked
without a model call (start `claude -p --permission-mode <m> --input-format stream-json
--output-format stream-json --verbose`, write only a control request, close stdin):
`default` is still accepted (only an unknown name such as `bogus` is refused with "Allowed
choices are acceptEdits, auto, bypassPermissions, manual, dontAsk, plan"), and claude
reports the mode back as `default` even when started with `manual`. mado keeps passing
`default`; no mapping in the worker.

### 25.2 Default mode

`auto` is the default wherever one is chosen: the Code tab of the New session form,
`newSession` when the request has no mode (`POST /api/sessions`, `/new` without a mode,
"Resume in mado" from history), the concierge (`defaultMode`, the `propose` schema and its
prompt). Resume keeps the stored mode. A hub without `[auto] classifier` still works:
mado auto asks the user whenever no rule decides.

### 25.3 Changing the mode of a running session

`POST /api/sessions/{id}/mode` with `{"mode": "default"|"acceptEdits"|"auto"}`, owner only,
code sessions only (chat and digest sessions: 400).

1. The hub stores `permission_mode`; `auto` also sets `auto_policy = 1` (the §18.1 switch
   keeps turning the policy off and on within `auto`). Audit `session_mode` with
   `<id> <old> -> <new>`.
2. If the session is `idle`, `running` or `waiting_approval`, the hub sends the worker
   `set_permission_mode {session_id, request_id, mode}` with mode = `claudeMode()` of the
   new mode (router `auto` gives `acceptEdits`, subscription `auto` gives `auto`); the
   frame id equals the request id. The worker passes `set_mode` to the shim, which writes
   `{"type":"control_request","request_id":"<uuid>","request":{"subtype":"set_permission_mode","mode":"<mode>"}}`
   to claude's stdin (a mutex keeps stdin lines whole) and waits up to 5 s for the
   `control_response` with that id on stdout. Control responses are never spooled. The
   outcome travels back as `mode_result {session_id, request_id, mode, ok, error}`; the
   API waits up to 8 s.
3. The answer is `{"applied": true}` when claude switched; `{"applied": false, "restart":
   true, "error": …}` when it could not be told (no shim, claude not running, claude's own
   error, no answer, or an older worker, whose error ack for the unknown frame is matched
   by the frame id); `{"applied": false, "restart": false}` for a stopped session, whose
   next Resume starts in the new mode. Every answer carries `session` (the view) and, for
   a router session, `auto`.
4. The hub publishes the session view as the SSE event `session`; the page merges it.

**Verified on 2.1.314** without a model call: the request is answered at once with
`{"type":"control_response","response":{"subtype":"success","request_id":"…","response":{"mode":"acceptEdits"}}}`,
followed by a session event `{"type":"system","subtype":"status","status":null,"permissionMode":"acceptEdits",…}`
(spooled like any event); an unknown mode gives
`{"type":"control_response","response":{"subtype":"error","request_id":"…","error":"Cannot set permission mode: must be one of acceptEdits, auto, bypassPermissions, default, dontAsk, plan","error_code":"invalid_mode"}}`.
The shim refuses anything but `default`, `acceptEdits` and `auto` before writing.

### 25.4 UI

The panel's permissions row is a select: `default`, `acceptEdits`, and `auto (mado
decides)` on a router profile or `auto (claude decides)` on a subscription one (the session
view gains `profile_kind`). A change toasts "permission mode: acceptEdits"; with `restart:
true` a "Restart to apply" button appears, which sends Stop, waits until the session is
resumable and then runs Resume (§21.6 keeps the list to one row). The AutoPanel switch
stays for router sessions. The composer's label reads "auto (mado decides)" or "auto
(claude decides)". Chat sessions have no mode.

### 25.5 Notes

* The §18.6 `PATCH {"auto": true}` on a router session in `default` or `acceptEdits` still
  does not tell claude; choosing `auto` in the select does (claude goes to `acceptEdits`).
* Tests: the API (store, frame and its claude mode per profile kind, refusal, an old
  worker's error ack, a stopped session, bad modes, chat, another owner, the SSE event, the
  audit) and in `internal/shim` the exchange against a fake claude that answers control
  requests like 2.1.314, its error, the shim's own refusals, and a timeout against `cat`.

## 26. Rate-limit auto-resume (M23, decided 2026-10-30)

On the subscription a turn can end with "You've hit your session limit · resets 5:30pm
(UTC)". Until now the user had to come back after the reset and type again. mado sees the
limit in the stream and can do that itself.

**What Claude Code 2.1.x sends** (seen on the production hub): a top-level
`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"|"allowed_warning"|"rejected","resetsAt":<unix s>,"rateLimitType":"five_hour"|"seven_day","unifiedWindows":{"five_hour":{"utilization":1.01,"resetsAt":…},"seven_day":{…}}}}`
on every turn; on a rejection it is followed by a synthetic assistant message (`"model":
"<synthetic>"`) and a `result`, and the user message that started the turn is consumed. A
message sent while still limited gives `system/init`, the rejection, the synthetic message
and a `result` again. A rejection can also land mid-turn, between tool calls.

### 26.1 Detection

`rateLimitEvent` runs on every stored code event (next to `recordCodeUsage`). For a
`rate_limit_event` of a **subscription** code session:

* `sessions.rate_limit` (TEXT, JSON) gets the `rate_limit_info`, overwritten each time.
* `rejected` with a `resetsAt`: `limited_until` (unix seconds), `limited_type`, and, on the
  first rejection only, `limited_seq`: the seq the re-sent messages start after. The hub
  walks back from the rejection: if it meets the user's echo before any answer, tool
  result or `result`, the turn was started by that message and claude did nothing with it,
  so `limited_seq` is the seq before the echo (the lost message is re-sent too, a small
  widening of the brief, which named only the messages after the rejection); otherwise
  (mid-turn) the rejection's own seq. The hub remembers that this turn was rejected,
  audits `session_rate_limited` and arms the timer.
* `allowed`/`allowed_warning` while limited: the window has room again (the user's own
  message went through), so the limited state and the timer go.
* Every such event publishes the session view (SSE `session`).

A `result` whose turn had no rejection resets `resume_attempts` to 0.

### 26.2 The resume

Columns `auto_resume` (default 1) and `resume_attempts`; `POST /api/sessions/{id}/
auto_resume {"on": bool}` flips the switch (code sessions only; audit
`session_auto_resume_switch`), arming or cancelling the timer.

`internal/hub/ratelimit.go` holds a timer per session on an injectable `Clock`
(`Options.Clock`). A timer fires 90 s after `limited_until` plus 0..30 s of jitter; when
that moment is already past (hub start, `armRateLimits` over every session with
`limited_until > 0` and `auto_resume = 1`), 10..40 s from now. When it fires:

1. Reload. Nothing to do if `limited_until` is 0. If the limit moved later, re-arm. If the
   switch is off, the session is a chat or digest, or the profile is not a subscription,
   clear `limited_until` and stop. After 6 consecutive attempts, clear it and leave the
   note `rate_limit_gave_up` ("Automatic resume stopped after 6 attempts in a row; send a
   message to continue.") and audit `session_auto_resume <id> gave_up`.
2. The text: the user's own messages (the shim's `"mado":"echo"` events) after
   `limited_seq`, verbatim, joined by blank lines (reason `messages`); with none, "The usage
   limit has reset. Continue the work you were doing when the limit was hit; do not repeat
   finished steps." (reason `continue`).
3. `idle` and the machine online: the text goes as a `user_message` like the composer's,
   carrying `note: "rate_limit_resume"`, so the shim stores the note
   `{"type":"user","mado":"note","mado_note":"rate_limit_resume",…}` ("Resumed
   automatically after the usage limit reset.") just before the echo; the transcript shows
   it as a mado notice. `running`, `waiting_approval` or `starting`, or the machine
   offline: re-arm in a minute. `exited`/`detached` and resumable with no other live
   session on the same claude conversation: `resumeSession` (the code behind `apiResume`,
   now shared, called with the owner's sub), the new session inherits the attempt count,
   and the text goes to it once it reports `idle` (polled for up to 3 minutes, else a
   `rate_limit_gave_up` note on the old session). Otherwise clear and stop.
4. `limited_until` is cleared, `resume_attempts` incremented, audit `session_auto_resume
   <id> <reason>` (`-> <new id>` after a resume). A further rejection re-arms through
   §26.1.

Notes on a running session go through the shim (`append_note` hub frame, local `note`), so
seqs stay the shim's; on a stopped one the hub stores the note after the last seq.

### 26.3 UI

* A banner above the transcript, beside the §21.6 one: "Usage limit reached (5-hour
  window). Resumes automatically at 02:30 (JST) · in 2 h 14 m", in the browser's time
  zone, the 7-day window with the date ("on Nov 3 09:00"), refreshed every 30 s, with the
  switch "Resume automatically when the limit resets". With the switch off it says when the
  window resets and that automatic resume is off.
* The panel's "Limits" block for subscription sessions with a `rate_limit`: `5 h: 26 % ·
  resets 11:30`, `7 d: 53 % · resets Nov 3 09:00` (utilization × 100), and the same switch.
* The composer stays usable; what is typed meanwhile is what §26.2 re-sends.
* The session view carries `rate_limit` (raw), `limited_until`, `limited_type` (both
  always present, so a merged view clears them) and `auto_resume`.

### 26.4 Scope and notes

* Router (AI&) sessions are never handled (their budget is §17.6's); chat sessions are out
  of scope. The windows are stored only for subscription sessions.
* Which turn was rejected is kept in memory; after a hub restart the first `result` resets
  the attempt counter even if its turn was rejected. The cap still bounds the loop within
  a hub run, since every attempt is counted.
* Tests (fake clock, fake worker): the user's messages re-sent after the reset (including
  the one that hit the limit and one typed meanwhile, the timer at reset + 90 s, the
  banner fields, the audit, the counter reset), the mid-turn continue text, the cap note,
  the switch off and on, an `allowed` event clearing the limit, router sessions ignored,
  chat refused, a stopped session resumed and the text sent to the new one once idle, and
  the arming at hub start.

## 27. File paths in the transcript and the file viewer (M24, decided 2026-10-30)

Claude and the user name files all the time (`~/work/notes/HANDOFF_NOTES.md`,
`internal/hub/api.go`); reading one meant opening a terminal on the machine. In a code
session such a path is now a link, and the file opens in the right panel: Markdown
rendered, other text as is, anything else offered as a download.

### 27.1 What is linked (`internal/render/paths.go`)

The goldmark transformer that already wraps tables also wraps path-like tokens in text
nodes and inline code spans as `<a class="fpath" href="#" data-path="…">`. Links, autolinks,
images, raw HTML and fenced or indented code blocks are left alone (code blocks have no text
nodes). Adjacent text nodes are joined first, because an unmatched `~`, `_` or `*` splits
the text and `~/a_b/c.md` would otherwise be three tokens. The renderer is the same for
`GET /api/sessions/{id}/blocks`, SSE blocks, chat answers and inbox digests, so the links
are everywhere in the HTML; the SPA makes them live only in code sessions (a session with a
machine) and in the viewer itself, elsewhere they look like plain text. The renderer stays
safe by construction: `data-path` is HTML-escaped, and a path contains no quote anyway.
goldmark's code span renderer assumes its children are text, so `mdRenderer` replaces it
with one that also writes path nodes.

A token is a run without white space, split also at CJK text (U+3000 and above), because
Japanese prose puts a particle right after a path (`DESIGN.mdを更新`). The leading
`([{<"'` and backtick and the trailing `.,;:)]}>"'` and backtick are stripped and stay
outside the link, and so is a `:line`, `:line:col` or `:line-line` suffix
(`internal/hub/api.go:123`). What remains is linked when it contains a `/` but no `//`
and matches one of

    ^(?:/|~/|\./|\.\./)[\p{L}\p{N}._+@%~/-]+$
    ^[\p{L}\p{N}._+@%-]+(?:/[\p{L}\p{N}._+@%-]+)*/[\p{L}\p{N}._+@%-]*\.[A-Za-z][A-Za-z0-9]{0,7}$

that is, it starts with `/`, `~/`, `./` or `../`, or it is `segment/…/name.ext` with an
extension of 1 to 8 letters or digits that starts with a letter. No `:` is allowed, so URLs
(`https://…`), `mailto:` and Windows drives never match; `a/b`, `and/or`, `1.2/3`,
`v1.2/3.4` and `10/30` have no extension that qualifies. Paths with spaces, Windows
paths and file names in CJK script are out of scope.

### 27.2 Where files may be read from

The hub asks the session's machine with a new frame `fs_read {request_id, session_id,
path, cwd, max_bytes, download}` and waits for `fs_result` through `fsWaits`, like the
concierge's `fs_inspect` (`fsRequest` returns the failed result too, `fsCall` wraps it).
`fs_result` gains `code` (`not_found`, `forbidden`, `too_large`, `not_file`); a
`too_large` result carries `{"size"}` in `data`. The worker offers the capability
`fs_read`, and the hub requires it rather than `fs`: an M12 worker has `fs` but would
ignore the frame, and the viewer would wait 20 s for nothing.

The worker (`internal/worker/fsread.go`) expands the path: `~` and `~/…` go below the
worker user's home (`~user` is refused), a relative path is joined to the session's cwd
(which the hub takes from the session row, never from the browser), then `filepath.Clean`.
The result must lie inside a project root, the session cwd or the home directory. The check
runs twice: lexically against those directories and their resolved forms before anything
is looked up, so a path outside them is refused without saying whether it exists, and again
after `EvalSymlinks`, so a symlink in the cwd that points to `/etc` is refused. Directories,
devices, sockets and FIFOs are refused ("a directory, not a file", "not a regular file").
Anything below a `.git/objects` directory is refused; other files under `.git` such as
`config` or `HEAD` are shown like any file, with no special casing beyond the size cap. The
home directory includes `~/.ssh` and `~/.claude`; this grants nothing new, since the
session's owner can already ask claude to `cat` them on that machine.

Sizes: the view reads at most 1 MiB (`proto.FSViewMax`), the download 16 MiB
(`proto.FSDownloadMax`, also the worker's cap whatever the hub asks). The file is read
through a `LimitReader` one byte past the cap, so a file growing after the stat is refused
too. The reply data is `{"path": resolved, "size", "modified": RFC3339, "mime":
http.DetectContentType, "text"}` when the bytes are valid UTF-8 and the type is text-like
(`text/*`, JSON, XML, JavaScript, or a `.md`/`.markdown` name), `{"binary": true}`
otherwise, and for a download `"data"` with the bytes (base64 in JSON: 16 MiB become about
21.4 MiB, below the hub's 64 MiB read limit, so no limit changed).

### 27.3 API

Both routes are owner only (404 otherwise, like every session route), for sessions with a
machine (code sessions and the claude executor's digest jobs; 400 for chat), and need the
machine online with `fs_read` (503 "the machine is offline" / "too old to show files").

* `GET /api/sessions/{id}/file?path=<p>` → `200 {path, size, modified, mime, kind, html?,
  text?}`. `kind` is `markdown` for a `.md`/`.markdown` text file (`html` is
  `render.Markdown`, so its paths are links too), `text` for other UTF-8 text (`text` is
  the raw content), `binary` otherwise (neither). Errors: 404 "no such file", 403 "outside
  the allowed directories" (or the worker's more precise refusal), 413 "too large to view
  (N KB); download instead" with `size`, 400 for a directory or special file or a missing
  path, 503 offline, too old or no answer within 20 s.
* `GET /api/sessions/{id}/file/download?path=<p>` → the bytes as
  `application/octet-stream`, `Content-Disposition: attachment; filename=<base name>`,
  `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`. Same rules, 16 MiB cap,
  60 s wait.

Audit rows `file_view` and `file_download` under the viewer's sub, with the detail
`<session id> <resolved path>`.

### 27.4 UI

`Markdown` (transcript.js) takes the opener from a `PathContext` that the session view
provides for sessions with a machine, or from an `onPath` prop; a click on `a.fpath` is
always caught, so a link without an opener does nothing. Opening a path pushes it on a back
stack of up to 20 entries and shows the panel (the sheet below 900 px, the panel column on
desktop even when the user had hidden it, without changing the saved preference). The panel
then renders `FilePanel` instead of the details: a bar with Back (the previous file, or the
details when the stack has one entry), Download and ×, the resolved path in full (it
wraps), size and modification time, and for a relative path a muted line saying it was
resolved on the machine relative to the session's cwd. The body is the transcript's
`Markdown` for `markdown` (a relative path inside the file is joined to the file's
directory, as a Markdown link would be, before it is opened), a `<pre>` without wrapping
and with horizontal scroll for `text`, and a notice with the Download link for `binary` and
for 413. "Loading…" while the request runs; API errors are shown inline in the body. On
desktop the panel widens to `min(720px, 48vw)` while it shows a file (`:has(.file-panel)`).
`.fpath` is the accent colour with a dotted underline inside live Markdown and plain text
elsewhere.

## 28. Editing hub.toml entries in the browser (M25, decided 2026-10-30)

AI& retires and renames models every few weeks (`deepseek-ai/deepseek-v4.1-flash` ends
2026-11-06; its successor is `deepseek-ai/DeepSeek-V4.1-Flash-1017`). Until now a profile or
chat model from `hub.toml` was read-only in the editor, and `[[prices]]`, `[auto]
classifier` and `[concierge] model` lived only in the file, so every rename meant an edit
on the hub's host and a restart. The admin can now make these changes on `/profiles`.

### 28.1 Overrides, not write-back

The hub never writes `hub.toml`. A browser edit of a `hub.toml` entry is stored in the DB
as an **override** (source `ui`), and the file's version is kept next to it:

* `profiles` and `chat_models` have a `config_json` column, and the new `prices` table
  (`model`, `json`, `source`, `config_json`) has the same shape. `config_json` always holds
  the current `hub.toml` version of that name, `""` when the name is not in `hub.toml`.
* **Seeding** at every start, per table, in one transaction: each `hub.toml` entry writes
  `config_json`, and writes `json` and `source = config` only when the row is absent or its
  source is `config`. A `ui` row keeps its `json`, so an override survives restarts and a
  later change of the same entry in `hub.toml` (which only moves `config_json`). A row
  whose name left `hub.toml` is deleted when its source is `config` and kept, with
  `config_json` cleared, when it is `ui` (it is then an ordinary editor entry). `builtin`
  rows (the `subscription` profile) are not touched. Profiles follow the same rule as chat
  models now, so a profile removed from `hub.toml` disappears at the next start (before
  M25 it stayed in the DB as a stale `config` row).
* **What is editable**: every profile, chat model and price except the built-in
  `subscription` profile. Saving a `config` row turns it into an override; a row that is
  `ui` and has a non-empty `config_json` shows "overrides hub.toml" (and whether it
  differs) and a **Revert to hub.toml** button: `DELETE /api/admin/profiles/{name}/override`,
  `DELETE /api/admin/chat-models/{name}/override`, `DELETE /api/admin/prices/override?model=`
  (price model ids contain `/`, so prices take the id as a query parameter). The row goes
  back to `config_json` with source `config`. Renames stay unsupported. A price added in the
  editor can be deleted (`DELETE /api/admin/prices?model=`); profiles and chat models still
  have no delete.
* **Secrets stay in the file.** The JSON still has `key_ref` only, which must name a key in
  the secrets file; a new key needs the secrets file and a restart, as before.
* `GET /api/admin/profiles` rows (profiles, chat models, prices) carry `source`, `editable`,
  `overrides_config`, `differs_from_config` and `config_json` (the `hub.toml` version in the
  editor's JSON form, `""` when none). Profile rows also list their `upstreams` (`name`,
  `base_url`, `model`, `key_ref`, `priced`); chat model rows carry `base_url`, `key_ref` and
  `priced`. The response adds `prices`, `settings`, `currency` and `price_template`.

### 28.2 Prices

`[[prices]]` is seeded into `prices` by the rule above. `rates()` reads the table through an
in-memory cache that a price save, revert or delete drops (and with it the once-only
"no `[[prices]]` entry" log, so a model whose price went away is logged again). The editor's
**Prices** section lists one row per model id with its numbers in `[budgets] currency` per
million tokens; the JSON uses the field names of `[[prices]]`
(`{"model":"…","input":45,"output":90,"cache_read":3,"cache_write":0}`, unknown fields
rejected) and is validated by `usage.Prices.Validate` on the single entry. A chat model or a
router upstream whose `model` has no price row shows "no price: cost recorded as 0".

### 28.3 Settings

A `settings(key, value, config_value, source)` table holds exactly `auto.classifier` and
`concierge.model`, seeded from `[auto] classifier` and `[concierge] model` with the same
override rule (a `ui` value is kept; `config_value` follows the file). The editor's
**Settings** section has a select over the chat model names for each, `""` meaning none
(no classifier; the concierge on the chat default), and Revert. `POST /api/admin/settings`
`{"key","value"}` rejects a value that is not a chat model name;
`DELETE /api/admin/settings/{key}/override` reverts.

* `auto.classifier`: a save or revert rebuilds the classifier the way `setupAuto` does
  (model, base URL and key read from the chat model); so does every chat model save or
  revert, since the classifier copies its model's fields. The next classifier call uses it.
  At start a `hub.toml` classifier that is not a chat model still fails the start; an
  override naming a model that no longer exists is logged and leaves auto mode without a
  classifier.
* `concierge.model`: read whenever a concierge session (or a chat-executor digest) picks
  its model; a name that is no longer a chat model falls back to the chat default.

### 28.4 Live effect

* **Profiles.** The router resolves alias → upstream → `Model` per request
  (`internal/router` writes `up.Model` into the forwarded body), but the worker only learns
  a profile at session start or at a reconnect. A profile save or revert therefore sends the
  same `start_session` with `refresh: true` that a reconnect sends, to every live code
  session on that profile (state running, idle or waiting for approval); the worker
  re-registers the session's router token and the next request goes to the new upstream
  model. Aliases are baked into claude's environment at start (§21.6), so alias changes
  still need a Resume; the editor says so under the profile JSON.
* **Chat models.** Every chat turn reads its model by name, so the next turn uses the new
  JSON. Sessions keep their model *name*: changing the `model` id inside a name (for
  example to the `-1017` successor) is the migration path for existing chats.
* **Audit**: `profile_save`, `profile_revert`, `chat_model_save`, `chat_model_revert`,
  `price_save`, `price_revert`, `price_delete`, `setting_save` (`key=value`),
  `setting_revert`, each with the name or key.

### 28.5 Drift visibility

The editor's first line reads "N entries override hub.toml" (profiles, chat models, prices
and settings together) when N > 0, so the admin knows the file is not the whole truth. There
is no `mado hub check-config`; a drift report on the command line is left out.

### 28.6 Upstream model lists

`GET /api/admin/upstream-models?base_url=<url>&key_ref=<ref>` (admins) fetches
`<base_url>/v1/models` (or `/models` when the base URL already ends in `/v1`, as an OpenAI
root does) with a 10 s timeout and returns `{"models":[{"id","name","deprecated_at",
"input","output","cached_input","currency"}]}`, each field but `id` only when the server
sent it. The normaliser takes a bare array or `data`/`models`/`result`, skips entries
without a string `id`, and reads prices from the entry or a nested `pricing` object, so a
server that returns only ids yields only ids. AI& answers without a key and adds a display
`name`, top-level `input_per_1m`, `output_per_1m` and `cached_input_per_1m` as strings with
`"currency": "usd"`, and `deprecation_date`, `null` or a date such as
`2026-11-05T15:00:00.000Z` (a captured answer is `internal/hub/testdata/aiand-models.json`). Only a `base_url` and `key_ref` pair that a stored profile
upstream or chat model uses is fetched, with that entry's key and auth header rule, so the
endpoint cannot send a key to an arbitrary URL. Answers are cached in memory for 10 minutes
per pair; failures are not cached and come back as 502.

The editor asks once per pair it shows. Next to every upstream and chat model id it puts a
red "retires <date>" (the browser's local date and time, so 2026-11-06 00:00 in JST) when
the list has a `deprecated_at` for it, an amber "not in the
upstream's model list" when the list loaded and the id is missing (ids are compared without
case: the successor is written `deepseek-ai/DeepSeek-V4.1-Flash-1017` in the
announcement and `deepseek-ai/deepseek-v4.1-flash-1017` in the list), and nothing when the list could not be
loaded. The top line adds "N models retiring" when any are.
