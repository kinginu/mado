# AI& (`https://api.aiand.com`) facts, verified 2026-10-18

Real requests against `/v1/messages` with `Authorization: Bearer <key>` and
`anthropic-version: 2023-06-01`. About 25 small probes. Re-verify when AI& changes models.

## Per model

| | glm-5.3 | glm-5.3-flash | deepseek-v4-pro | deepseek-v4-flash |
|---|---|---|---|---|
| Custom tool use (`tool_use` block, `stop_reason: tool_use`) | yes | yes | yes | yes |
| Streaming `input_json_delta` | yes | not run | not run | not run |
| Image input (base64 PNG) | 400 | yes | 400 | 400 |
| PDF `document` block | 400 | yes (pages rendered as images, 2737 input tokens for one line) | not run | not run |
| `/v1/models` capabilities | reasoning, tool_calling | + vision, video, document | reasoning, tool_calling | reasoning, tool_calling |
| context_window / max_output_tokens | 1048576 / 131072 | 1048550 / 131072 | 1048576 / 384000 | 1048576 / 384000 |
| reasoning efforts (default) | low, high, max (max) | same | none, high, max (high) | same |

Other vision models listed: deepseek-v4.1-flash (also document), qwen3.8-27b, qwen3.6-27b,
kimi-k3, kimi-k2.7-code, gemma-4-31b-it. Text only: glm-5.2, motif-3, gpt-oss-120b.

## Behaviour

* `usage` carries `input_tokens`, `cache_read_input_tokens`, `output_tokens` only;
  `cache_creation_input_tokens` never appears. Caching is automatic: a repeated 5.7k-token
  system prompt came back as `input_tokens: 8, cache_read_input_tokens: 5760`.
* Streaming: `message_start.usage.input_tokens` is wrong (72 vs final 168); the usage in
  `message_delta` is the reliable one.
* `tool_use.id` looks like `chatcmpl-tool-9928b27491265d9d_05mm6pg`, not `toolu_…`.
* Anthropic-defined tool `type`s (`web_search_*`, `bash_*`, `text_editor_*`, …) are rejected
  with 400; a custom tool named `web_search` without `type` works. `tool_choice` supports
  auto, any, none, tool.
* A model without `vision` rejects images and PDFs with 400 `invalid_request_error`,
  `code: invalid_value`, `param: messages`, message `Model 'X' does not support image input.
  Use a model with the 'vision' capability.` (or `… PDF input.`).
* `max_tokens` is only checked against the context window; 200000 is accepted for glm-5.3.
  A small `max_tokens` makes the hidden reasoning appear as the text answer with
  `stop_reason: max_tokens`. Keep `max_output` large (mado default 32000).
* Reasoning is hidden by default and counted in `output_tokens` ("hi" costs 42 to 80 output
  tokens). `thinking: {type: "enabled"}` returns `thinking` blocks without `signature`;
  `budget_tokens` is ignored; effort via `reasoning_effort` or `output_config.effort`.
  Replayed thinking blocks are dropped.
* Error bodies: `{"type":"error","error":{type,message,code,param},"request_id"}` on
  `/v1/messages`; some 404s use the OpenAI shape without the wrapper. A prompt over the
  context window is a 400 whose message starts with `prompt is too long`.
* No image generation: `POST /v1/images/generations` is 404. Video generation exists
  (`POST /v1/videos`, `minimaxai/minimax-h3`, 10 JPY per second at 768p).
* `/v1/models` entries carry `capabilities`, `context_window`, `max_output_tokens`,
  reasoning efforts and JPY prices per million tokens (input, output, cached input).

## Prompt caching with Claude Code-shaped requests (measured 2026-10-19)

A ~7.8k-token request (two system blocks, 20 tools, five turns with `tool_use` and
`tool_result`, `cache_control` on the system, the last tool and the last message,
`max_tokens: 64`, streaming), sent a few seconds apart. Usage as `input / cache_read /
output`; `message_start` said `10620 / 0 / 0` every time (11104 for the longer request).

| request | deepseek-v4-pro (`message_delta`) | glm-5.3 (`message_delta`) |
|---|---|---|
| first | 7791 / 0 / 64 | 7399 / 0 / 64 |
| same again | 111 / 7680 / 64 | 103 / 7296 / 64 |
| two more turns appended, `cache_control` moved to the new last message | 479 / 7680 / 64 | 421 / 7296 / 14 |
| same body, top-level keys sorted (what mado's router does) | 111 / 7680 / 58 | not run |
| one pair of tools swapped | 7791 / 0 / 64 | not run |
| no `cache_control` anywhere (first, then again) | 7791 / 0 / 64, then 111 / 7680 / 64 | not run |
| a body with nested keys sorted too (`input_schema` property order changes) | 7791 / 0 / 64 | not run |

* Caching works on both lanes for multi-turn tool requests, in blocks (7680 = 30 × 256,
  7296 = 57 × 128); the tail after the last whole block is uncached.
* The cache key is the rendered prompt: tool order, nested key order inside tools, and
  whether `cache_control` is present all count; top-level key order does not. A moved
  `cache_control` marker on the last message does not break the prefix.
* `message_start.usage` is a size estimate of the whole prompt with `cache_read 0` and
  `output 0`; only `message_delta` has the real split. Claude Code puts the `message_start`
  usage into its stream-json `assistant` events, so anything that reads those sees no
  caching and no output. Its `result.modelUsage` has the real counts (it sums
  `message_delta`). That was the whole "cache never hits" finding of 2026-10-19: one
  deepseek-v4-pro session had 16.1M input, 110.7M cache read and 2.51M output by
  `modelUsage` (87 % of the prompt cached), about ¥6,900 at 160 / 30 / 400, while mado's
  rows said ¥16,000 with almost no cache reads. Claude Code itself sends a stable
  prefix: `system`, `tools` and earlier messages were byte-identical across the calls of
  a turn (checked against a local fake upstream, Claude Code 2.1.303, attribution header
  off as mado sets it).

