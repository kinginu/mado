# Claude Code facts used by mado

Verified 2026-10-18 against Claude Code **2.1.302** and the docs below. Items marked
"experiment" were confirmed with one real `claude -p` run (the recorded fixture
`internal/render/testdata/write_tool.jsonl`; the MCP traffic is quoted below).

## 1. Model alias env vars

Source: https://code.claude.com/docs/en/model-config and https://code.claude.com/docs/en/env-vars

| Alias | Env var |
|---|---|
| `opus` (also `opusplan` in plan mode) | `ANTHROPIC_DEFAULT_OPUS_MODEL` |
| `sonnet` (also `opusplan` outside plan mode) | `ANTHROPIC_DEFAULT_SONNET_MODEL` |
| `haiku` (also background functionality) | `ANTHROPIC_DEFAULT_HAIKU_MODEL` |
| `fable` | `ANTHROPIC_DEFAULT_FABLE_MODEL` (**exists**) |

* `best` resolves to whatever `fable` resolves to where Fable is available, otherwise to `opus`.
* `ANTHROPIC_SMALL_FAST_MODEL` is deprecated; `ANTHROPIC_DEFAULT_HAIKU_MODEL` replaces it.
* `ANTHROPIC_MODEL` / `--model` set the session model; `--model` wins.
* `CLAUDE_CODE_SUBAGENT_MODEL` exists (subagent model).
* `CLAUDE_CODE_ATTRIBUTION_HEADER=0` omits the attribution block from the system prompt.
* `API_TIMEOUT_MS`: default 600000, max 2147483647. The 3000000 in DESIGN §3.2 is valid.
* `ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer <value>`;
  `ANTHROPIC_API_KEY` as `X-Api-Key`. The router therefore checks the bearer.

mado sets all four `ANTHROPIC_DEFAULT_*_MODEL` vars, including FABLE.

## 2. `--input-format stream-json` user message (experiment)

The docs list the flag (https://code.claude.com/docs/en/cli-reference) but do not spell the
line format out. Confirmed by experiment: one JSON object per line on stdin,

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}
```

is accepted. When stdin reaches EOF after the last turn, `claude -p` finishes the turn,
prints the `result` line and exits 0. Without `--replay-user-messages` the user text is
**not** echoed on stdout, so the shim synthesises a local `user` echo event for the spool.
Output requires `--verbose` with `--output-format stream-json`. Event types seen:
`system/init`, `assistant`, `user` (tool_result), `rate_limit_event`, `result`.
`session_id` is on every line; `system/init` carries `session_id`, `model`, `permissionMode`,
`claude_code_version`, `apiKeySource`.

## 3. `--permission-prompt-tool` contract (docs + experiment)

* Flag: `--permission-prompt-tool mcp__<server>__<tool>`, server from `--mcp-config`
  (https://code.claude.com/docs/en/cli-reference). The tool name form `mcp__<server>__<tool>`
  is in https://code.claude.com/docs/en/permissions. Claude Code waits up to `MCP_TIMEOUT`
  (30 s) for that server before the first turn.
* `--mcp-config` stdio shape (https://code.claude.com/docs/en/mcp):
  `{"mcpServers":{"mado":{"type":"stdio","command":"/abs/bin","args":["..."],"env":{}}}}`.
  We also pass `--strict-mcp-config` so the user's other servers are not loaded.
* The probe run used the server name `open_cc` (the project's earlier name); mado now uses
  server `mado`, i.e. `--permission-prompt-tool mcp__mado__approve`. The M1 smoke test ran the
  real CLI with that name through a router profile and a local fake upstream: Allow ran the
  Write, Deny reached claude as an error tool_result with our message.
* Input (experiment): `tools/call` with
  `{"name":"approve","arguments":{"tool_name":"Write","input":{...},"tool_use_id":"toolu_..."},"_meta":{"claudecode/toolUseId":"toolu_...","progressToken":2}}`.
* Output: a text content block whose text is a JSON string. Shape from the SDK
  `PermissionResult` type (https://code.claude.com/docs/en/agent-sdk/typescript and
  https://code.claude.com/docs/en/agent-sdk/user-input):
  allow = `{"behavior":"allow","updatedInput":{...}}` (updatedInput optional since v2.1.207;
  we always echo the input), deny = `{"behavior":"deny","message":"..."}`.
  Experiment: returning allow with `updatedInput` made the Write run; the stream's
  `tool_result_meta` showed `permission_decision: {decision:"accept", source:"user_temporary"}`.
* Client also sends `server/discover` (protocol 2026-07-28 probe) before `initialize` with
  2025-11-25; a server must answer it with a JSON-RPC error (-32601) or result.
* An allow for an MCP tool marked `anthropic/requiresUserInteraction` is converted to deny.
* **Permission mode matters.** Since 2026-w32 auto mode can be the default starting mode
  (https://code.claude.com/docs/en/headless "Auto-approve tools"). mado always passes
  `--permission-mode default` unless the hub asks for another allowlisted mode, so prompts
  reach the tool.

## 4. `CLAUDE_CONFIG_DIR`

Sources: https://code.claude.com/docs/en/env-vars, https://code.claude.com/docs/en/authentication,
https://code.claude.com/docs/en/sessions, https://code.claude.com/docs/en/claude-directory

* Replaces `~/.claude`: settings, session history (transcripts under
  `$CLAUDE_CONFIG_DIR/projects/<cwd-mangled>/<session-id>.jsonl`), plugins, auto memory,
  every `~/.claude` path in the directory reference.
* Credentials: on Linux `~/.claude/.credentials.json` (0600); with `CLAUDE_CONFIG_DIR` set
  the file lives under that dir instead. So a per-profile dir has **no** subscription login.
* Auth precedence: cloud provider > `ANTHROPIC_AUTH_TOKEN` > `ANTHROPIC_API_KEY` >
  apiKeyHelper > `CLAUDE_CODE_OAUTH_TOKEN` > profiles > `/login` OAuth. With
  `ANTHROPIC_AUTH_TOKEN` set, the OAuth login is not used; combined with a separate
  `CLAUDE_CONFIG_DIR` it is not even readable by that process, which answers DESIGN §6.
* Not settable from project/local settings, only shell/user/managed. Use an absolute path.

## 5. `--resume` with `-p` and stream-json

https://code.claude.com/docs/en/headless "Continue conversations" shows
`claude -p "..." --resume "$session_id"`; https://code.claude.com/docs/en/cli-reference says
`--resume` accepts an ID, a name, or an absolute `.jsonl` path and searches every project on
the machine. Nothing restricts it to text mode, so mado passes `--resume <id>` together with
the stream-json flags.

**Verified 2026-10-18 (M2 smoke, Claude Code 2.1.302, subscription login, 2 real runs):**
`claude -p --input-format stream-json --output-format stream-json --verbose --permission-mode
default --strict-mcp-config --mcp-config … --permission-prompt-tool mcp__mado__approve
--resume cb296546-…` started after the first process (turn "reply with the single word ok" →
`ok`) was ended by SIGINT + stdin EOF when its tmux server was killed. The resumed process:

* prints nothing until the first stdin message (no `system/init` at start);
* keeps the **same** session id (`system/init.session_id` = the resumed id; no fork);
* has the earlier context: asked "What single word did you reply with in your previous
  answer?", it answered `ok`;
* the same holds for a router profile (`CLAUDE_CONFIG_DIR` per profile): the request after the
  resume carried the whole history (23 messages).
`--fork-session` creates a new ID instead. SIGTERM exits 143 leaving the turn unfinished;
SIGINT ends the turn (headless "Stop a run with SIGTERM").

## Other relevant facts

* `ANTHROPIC_BASE_URL` to a non-first-party host disables MCP tool search by default and
  disables Remote Control (the gap mado fills).
* `-p` runs project hooks and `.mcp.json` without a trust dialog. `--strict-mcp-config`
  ignores `.mcp.json`; hooks still run. Consider `--setting-sources` in M2.
* `--session-id <uuid>` lets the caller choose the Claude session id up front.

## Learned in the M1 smoke test (Claude Code 2.1.302)

* Requests go to `/v1/messages?beta=true`. Upstreams must route on the path, not path+query.
  The router forwards the query string unchanged.
* With a custom model name (e.g. `oc-strong`) Claude Code prints a
  `[claude-code:unrecognized_model] {"model":"oc-strong","query_source":"sdk"}` diagnostic on
  stderr and assumes a 200k context window. Harmless; silence it with `modelOverrides`
  (https://code.claude.com/docs/en/model-config) and set `CLAUDE_CODE_MAX_CONTEXT_TOKENS` per
  upstream in M2.
* An upstream 404 surfaces as "There's an issue with the selected model (...)" with
  `api_error_status: 404` in the result line.
* In stream-json input mode, `system/init` is emitted again at the start of every turn.
* A fresh `CLAUDE_CONFIG_DIR` logs `Failed to save config with lock: ENOENT .claude.json` once;
  the run proceeds. `apiKeySource` is `none` with `ANTHROPIC_AUTH_TOKEN` set.
* tmux: a new `tmux -L <socket>` server reads `~/.tmux.conf`. On this machine that loads
  tmux-continuum with restore-on-start, which restored the user's 16 saved sessions (about 10
  `claude -r` processes) into the mado server. The worker now always passes `-f /dev/null`.

## Learned in M2 (Claude Code 2.1.302)

Sources: https://code.claude.com/docs/en/env-vars, https://code.claude.com/docs/en/model-config
("Correct the window for a gateway or custom model ID", "Override model IDs per version"),
https://code.claude.com/docs/en/settings-reference (`modelPicker`, `modelOverrides`).

* `CLAUDE_CODE_MAX_CONTEXT_TOKENS`: the context window Claude Code assumes for the active
  model. For an ID it does not recognise that does not start with `claude-` and has no `[1m]`
  suffix it applies on its own; for recognised or `claude-` IDs only together with
  `DISABLE_COMPACT`. One value per process.
* `CLAUDE_CODE_MAX_OUTPUT_TOKENS`: max output tokens; for an unknown model ID the default is
  32000 and the cap 128000.
* Related: `CLAUDE_CODE_AUTO_COMPACT_WINDOW` (100000..1000000),
  `CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1` (compact only after the API says
  too long, v2.1.223+), `ANTHROPIC_DEFAULT_*_MODEL_NAME` / `_DESCRIPTION` /
  `_SUPPORTED_CAPABILITIES` (picker display and `effort,thinking,…` capabilities).
* `modelPicker` rows have only `model`, `label`, `description`, `behavesAs`; there is no
  per-model context-window field anywhere in settings.
* Experiment: with `CLAUDE_CODE_MAX_CONTEXT_TOKENS=65536` and `CLAUDE_CODE_MAX_OUTPUT_TOKENS=8000`
  in the child's env (checked in `/proc/<pid>/environ`) the
  `[claude-code:unrecognized_model] {"model":"oc-fake","query_source":"sdk"}` stderr line still
  appears on every turn. The docs: "To stop the `[claude-code:unrecognized_model]` diagnostic
  line for an ID such as a gateway alias, add [a `modelOverrides`] entry with that ID as its
  value" — which makes the ID behave as that Anthropic model. mado does not do that.
* A router request cut by a daemon restart is retried by Claude Code: the stream shows
  `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":586,…}`
  and the turn completes once the router is back.
* Killing the tmux server delivers SIGHUP to the shim (the pane's session leader), not to the
  `claude` child (own process group, pipes for stdio).

## Learned in M4: subagents and the role split

Sources: https://code.claude.com/docs/en/cli-reference, https://code.claude.com/docs/en/sub-agents
(read 2026-10-18).

* `--agents '<json>'`: "Define custom subagents dynamically via JSON." Each top-level key is
  the agent's name (must not start with `-`), each value a definition with `prompt` (the
  system prompt) and the frontmatter fields `description`, `tools`, `disallowedTools`,
  `model`, `permissionMode`, `mcpServers`, `hooks`, `maxTurns`, `autoCompactWindow`,
  `skills`, `initialPrompt`, `memory`, `effort`, `background`, `omitClaudeMd`, `isolation`
  (`color`, `experimental` are ignored). Example from the docs:
  `claude --agents '{"reviewer":{"description":"Reviews code","prompt":"You are a code reviewer"}}'`.
  With `--print` the value may instead be a path to a JSON file (v2.1.281+). Claude Code
  validates it at startup and exits on an invalid value (v2.1.242+).
* Priority when names collide: managed settings > `--agents` > `.claude/agents/` >
  `~/.claude/agents/` > plugins. So mado's `implementer`/`researcher`/`simple` win over a
  project's agents of the same name.
* `model` takes `sonnet`, `opus`, `haiku`, `fable`, a full model id, or `inherit`.
  Resolution order for a subagent: the Agent call's own `model` parameter, then the
  definition's `model`, then `CLAUDE_CODE_SUBAGENT_MODEL`, then the main model. Built-in
  Explore and Plan are not moved by `CLAUDE_CODE_SUBAGENT_MODEL` alone.
* Delegation is decided by the main model from the agents' `description`; "use proactively"
  in the description encourages it. The tool is `Agent` (renamed from `Task` in 2.1.63;
  `Task` still works as an alias).
* `--append-system-prompt <text>` appends to the default system prompt (also
  `--append-system-prompt-file`). The prompt is recorded on the conversation's first request
  and reused after `--resume` until compaction. `--append-subagent-system-prompt` exists for
  subagents (`-p` only); mado does not use it.
* `--forward-subagent-text` (v2.1.211+, needs `--print` and stream-json): subagent text and
  thinking blocks are emitted as `assistant`/`user` messages with `parent_tool_use_id` set.
  Without it, the text of a foreground subagent is omitted.
* `--permission-mode` accepts `default`, `acceptEdits`, `plan`, `auto`, `dontAsk`,
  `bypassPermissions`, and `manual` (alias of default). mado passes `default`,
  `acceptEdits` or (since M5) `auto`.
* Stream shapes seen with 2.1.302 (real aiand-pair session): `system/thinking_tokens`
  `{estimated_tokens, estimated_tokens_delta}` per streamed thinking chunk; a background
  Agent call answers at once with a tool_result `{"isAsync":true,"status":"async_launched",
  "agentId":…,"resolvedModel":"aiand-dsv4flash"}`, then `system/task_started`
  `{task_id, tool_use_id, subagent_type, is_backgrounded}`, `system/task_progress`
  `{description, usage:{total_tokens,tool_uses,duration_ms}, last_tool_name}`,
  `system/task_updated {patch:{status}}`, `system/task_notification {status, summary,
  output_file}` and `system/background_tasks_changed {tasks[]}`. The subagent's own events
  carry `parent_tool_use_id` = the Agent call's id and may continue after the main turn's
  `result`. The `[claude-code:unrecognized_model]` stderr line names the requester
  (`query_source":"agent:builtin:Explore"`).
* `result.modelUsage` is cumulative per process and keyed by the requested model name;
  `result.usage` is per turn. `assistant` events repeat `message.usage` for each content
  block of one response.


## Learned in M5: telling the main model from a subagent

Sources (read 2026-10-18): https://code.claude.com/docs/en/hooks ("Common input fields",
"PreToolUse decision control", "Command hook fields", "Hooks in subagents"),
https://code.claude.com/docs/en/sub-agents ("Supported frontmatter fields", "Available
tools"), https://code.claude.com/docs/en/cli-reference (`--settings`, `--disallowedTools`,
`--agent`), https://code.claude.com/docs/en/settings ("Lists merge instead of overriding"),
https://code.claude.com/docs/en/permission-modes ("Eliminate prompts with auto mode").

* **Hook input names the agent.** Docs: "`agent_id` and `agent_type` tell your script which
  agent a hook fired in"; `agent_id` is the subagent's id, `agent_type` its name, present
  "when the session uses `--agent` or the hook fires inside a subagent". Hooks from settings
  files "also run inside subagents ... the input carries the `agent_id` and `agent_type`".
  There is no `parent_tool_use_id` in hook input.
* **Experiment (2.1.302, scripted fake Anthropic upstream, `--settings` hook that logs its
  stdin):** the main model's `Write` and `Agent` calls arrive without `agent_id`/`agent_type`;
  the implementer's `Write` arrives with `"agent_id":"a2dac047ebae07259","agent_type":"implementer"`.
  So a PreToolUse hook can deny the main model's calls and let subagents through.
* **The permission layer cannot.** `--disallowedTools` removes a tool from the main
  conversation, and subagents inherit the main conversation's tools ("Subagents inherit the
  built-in tools and MCP tools available in the main conversation"), so it would block the
  implementer too. The `--permission-prompt-tool` request carries only `tool_name`, `input`
  and `tool_use_id` (§3), and `PermissionRequest` hook input has no `agent_id` either.
  `--agent <name>` would apply an agent's `tools` to the main thread, but it replaces the
  whole Claude Code system prompt with the agent's prompt; mado does not use it.
* **Deny format.** PreToolUse stdout `{"hookSpecificOutput":{"hookEventName":"PreToolUse",
  "permissionDecision":"deny","permissionDecisionReason":"..."}}`; the reason is shown to
  the model. Exit 2 also blocks, with stderr as the reason. Experiment: the model sees the
  tool_result `PreToolUse:Write hook error: <reason>`; the hook runs before the permission
  prompt, so a denied call never reaches `mcp__mado__approve`.
* **Exec form.** A command hook takes `command` + `args` (no shell); verified with
  `{"type":"command","command":"/abs/mado","args":["guard-tool","--enforce","edits"],"timeout":10}`.
* **`--settings '<json>'` hooks run in `-p`** (experiment) and merge with the user's and
  project's hooks, since lists merge across settings sources.
* **Agent `tools`.** An explicit list narrows the subagent's pool and drops MCP tools unless
  named. Experiment: `"tools":["Read","Grep","Glob","Bash","WebFetch"]` gave the researcher
  `Read, Bash, WebFetch`: on Linux the default tool set has no Glob/Grep (cli-reference,
  `--tools`), and names outside the pool are dropped without an error. `simple` with
  `Read, Edit, Write, Bash` got exactly those.
* **Auto mode** needs a supported model (Anthropic API: Opus 4.6+, Sonnet 4.6+, Haiku 5.5,
  Fable; gateways via `ANTHROPIC_BASE_URL` are asked by default). "When the flag ... selects
  `auto` but auto mode isn't available to the session, Claude Code starts the session in
  Manual instead." So a router profile with GLM as main runs `auto` as `default`.

### Real runs against AI& (2026-10-18, exact `mado print-argv` argv, GLM-5.3 main)

| run | prompt | main tools | Agent calls | models (turns) |
|---|---|---|---|---|
| 1 | dflash2-bench (ja), M5 prompt v1 | Bash 1 | researcher | glm-5.3 ×6, deepseek-v4-pro ×34 (sub) |
| 2 | internal/router (en), v1 | Bash 1, Read 1 | **none** | glm-5.3 ×6 |
| 3 | internal/router (en), final prompt | Bash 2 | researcher | glm-5.3 ×8, deepseek-v4-pro ×24 (sub) |
| 4 | "edit NOTES.txt yourself" | Read 2, Edit 1 (denied) | simple | glm-5.3 ×10, deepseek-v4-flash ×11 (sub) |
| 5 | "edit common.py yourself" | Read 2, Edit 1 (denied) | implementer | glm-5.3 ×11, deepseek-v4-pro ×9 (sub) |
| 6 | dflash2-bench (ja), final | Bash 1 | researcher | glm-5.3 ×6, deepseek-v4-pro ×21 (sub) |

Run 2 obeyed the v1 rule ("more than two file reads") with one Read of `router.go`; the final
prompt sends every question about what code does to the researcher, even for one file. In runs
4 and 5 the denial reached the model, it delegated, the subagent's Edit succeeded, and the main
model read the file back before answering.

## M16: job sessions (official docs, CLI 2.1.30x, checked 2026-10-20)

* `--tools ""` disables all built-in tools in `-p` mode (CLI reference). MCP tools stay.
* MCP tool names are `mcp__<server>__<tool>`; `--allowedTools mcp__mado__propose_task`
  runs it without a prompt and coexists with `--permission-prompt-tool`.
* `--append-system-prompt` works in `-p`; no documented length cap (mado caps the job
  prompt at 64 KiB because it goes into argv).
* `--json-schema` is documented only with `--output-format json`, not `stream-json`; not
  used. The digest is `result.result`.
* Subscription usage limit: claude emits `system/api_retry` events and exits with code 2
  **without** a `result` event. mado treats "ended, no result" (any code), `is_error`, a
  subtype other than `success` and a non-null `api_error_status` as failures.
* `--max-turns N` caps agentic turns in `-p` (tool calls count); jobs use 12.
* `--model fable|opus|sonnet|haiku` aliases work on the subscription.
* `CLAUDE_CONFIG_DIR` relocates `.credentials.json`, so jobs run with the default config
  dir.
