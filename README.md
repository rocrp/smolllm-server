# smolllm-server

OpenAI-compatible HTTP front-end for [smolllm-go](../smolllm-go). Routes requests
across 41 providers via short alias names, runs locally on macOS under launchd.

## Layout

```
cmd/server/         entrypoint
internal/config/    YAML loader, env-file loader, alias resolution
internal/auth/      bearer token middleware
internal/llm/       OpenAI ↔ smolllm adapter (request build + response shapes + failure mapping)
internal/modelspec/ fallback-chain grammar incl. the `!effort` suffix, shared by config and adapter
internal/server/    HTTP wiring: chat / embeddings / models / health
internal/apierr/    OpenAI-style error envelope
launch/             LaunchAgent plist + install/reload/uninstall script
```

`go.mod` carries `replace github.com/rocry/smolllm-go => ../smolllm-go` while
`WithLegReasoningEffort` is unreleased; it becomes a `v0.3.2` pin once that is
tagged.

One `smolllm.Client` is built at startup and shared by every request: it owns the
API-key and endpoint balancer, so a per-request client would restart key rotation
on every call. Credentials still resolve per leg at call time, so hot-reloading
the env file reaches it without a restart.

## Configuration

Default config path: `~/.config/smolllm-server/config.yaml`. Override with
`--config <path>` or `$SMOLLLM_SERVER_CONFIG`.

```yaml
server:
  bind: 0.0.0.0:11435
  access_key: change-me       # REQUIRED. SMOLLLM_SERVER_ACCESS_KEY env wins if set
  env_file: ~/.env.smolllm    # provider keys: ${PROVIDER}_API_KEY etc.
  log_level: info

aliases:
  fast: cerebras/qwen-3-235b-a22b-instruct-2507,groq/qwen/qwen3-32b!none,gemini/gemini-flash-latest
  translator: ollama/frob/hy-mt1.5:latest,gemini/gemini-flash-lite-latest
```

Aliases pass straight through to smolllm-go's `WithModel("a,b,c")`, which tries
each in order and falls back on error.

### Per-leg reasoning effort

Each leg of a chain may carry `!effort`:

```yaml
fast: groq/openai/gpt-oss-120b!low,gemini/gemini-flash-latest!none,deepseek/deepseek-v4-flash
```

`!effort` is **smolllm-server config syntax, not smolllm-go's.** The library
dropped the suffix in v0.3 so a wire model name could contain any punctuation.
The server keeps it because a fallback chain wants a different reasoning budget
per leg, which the library's chain-wide setting cannot express. `internal/modelspec`
parses each leg into a spec and an optional effort, and the adapter hands
smolllm-go the bare specs plus one `WithLegReasoningEffort(spec, effort)` per
suffixed leg. The suffix never reaches a provider, and everything after the first
`/` and before the `!` is the wire model name, sent verbatim.

**Precedence.** A leg's own `!effort` wins for that leg. Legs without one use the
request's `reasoning_effort` field, which applies to the chain as a whole. The
two live in different library fields, so the outcome never depends on the order
the options were applied.

```
config:  fast: mock/alpha!none,mock/beta
request: {"model": "fast", "reasoning_effort": "high"}
         → alpha runs at "none"   (its own suffix wins)
         → beta  runs at "high"   (no suffix, so the request's value applies)
```

Two constraints:

- **One spec, one effort per chain.** The override is keyed by model spec, so
  `gemini/pro!high,gemini/pro!low` is rejected at config load rather than
  silently collapsed to whichever came last.
- **The provider still has to accept the value.** smolllm-go allows `none`,
  `minimal`, `low`, `medium`, `high` and `xhigh` (Ollama takes a narrower set),
  and a leg whose provider rejects its effort fails so the chain advances.
  `gpt-oss` rejects `none`, for instance, so it takes `!low`.

## Install / run

```bash
just install      # build, seed config, link plist, bootstrap agent
just reload       # rebuild + kickstart (full restart; needed for bind changes)
just uninstall    # bootout + remove symlink (binary & config preserved)
just logs         # tail ~/Library/Logs/personal.smolllm-server.log
```

### Hot reload

Just edit the YAML — the server picks it up automatically (~200 ms debounce
via fsnotify). Hot-reloadable: `aliases`, `server.access_key`,
`server.log_level`, and `server.env_file` contents (re-sourced with overwrite
so rotated provider keys take effect). Invalid YAML is rejected and the
previous snapshot is retained.

For `server.bind` changes, or to force a clean restart, run `just reload`.

The agent runs at `0.0.0.0:11435` (all interfaces, LAN-accessible) and reads
`~/.env.smolllm` itself on startup — no wrapper script.

## Endpoints

| | |
|---|---|
| `GET /healthz`           | Public liveness probe. |
| `GET /stats`             | In-memory per-attempt token usage for the last ~31 UTC days, bucketed by day × requested alias × served provider/model. Columns follow smolllm-go's usage: `input_tokens` excludes `cache_read_tokens`, and `output_tokens` includes `reasoning_tokens`. Auth required. |
| `GET /v1/models`         | Lists configured aliases. Auth required. |
| `POST /v1/chat/completions` | OpenAI Chat Completions. Auth required. Streaming via `stream: true`. |
| `POST /v1/embeddings`    | OpenAI Embeddings. Auth required. |

Auth: `Authorization: Bearer <access_key>` (the bare `<access_key>` form is also
accepted for clients that omit `Bearer`).

### Examples

```bash
# Health (no auth)
curl -fsS http://127.0.0.1:11435/healthz

# Set your access key once (matches server.access_key in config.yaml)
export ACCESS_KEY=your-access-key

# Models
curl -fsS http://127.0.0.1:11435/v1/models -H "Authorization: Bearer $ACCESS_KEY"

# Non-streaming chat against an alias
curl -fsS http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"say hi in 3 words"}]}'

# Streaming
curl -N http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"translator","stream":true,"messages":[{"role":"user","content":"translate: hello"}]}'

# Direct provider/model also works (bypasses aliases)
curl -fsS http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gemini/gemini-flash-latest","messages":[{"role":"user","content":"hi"}]}'

# Embeddings
curl -fsS http://127.0.0.1:11435/v1/embeddings \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"ollama/qwen3-embedding:0.6b","input":["hi","there"]}'
```

### Pointing tools at it

- **Cursor / Open WebUI / Aider**: set base URL to `http://127.0.0.1:11435/v1`,
  API key to your access key, and pick `fast`, `translator`, or any
  `provider/model` string.

## Tool calling

`tools`, `tool_choice`, `parallel_tool_calls` and `response_format` are
pass-through fields: forwarded to the provider verbatim, never modeled here, so
a provider error about one surfaces unchanged. Assistant messages carrying
`tool_calls` and `tool` role messages replay as sent.

Streaming forwards tool calls fragment by fragment, the way OpenAI does: one
frame opens the slot with its `index`, `id`, `type` and function name, and later
frames carry argument text only. smolllm-go v0.3 streams those fragments rather
than withholding complete calls until the end, so an agent loop can render
arguments as they arrive. Any provider keys on the call (Gemini's thought
signature, say) ride along on the opening frame and replay losslessly.

Two things worth knowing before pointing an agent at an alias:

- **`finish_reason` is OpenAI-shaped here.** A turn carrying tool calls always
  reports `tool_calls`, even when the provider said `stop` (Gemini does). The
  libraries still surface the provider's string verbatim; only this
  OpenAI-compatible surface normalizes it, so agent loops that branch on the
  field work unchanged.
- **Not every leg supports tools.** A leg that rejects them 400s and the chain
  advances; a leg that *ignores* them answers in prose, which no agent loop can
  detect. Use the `agent` alias, whose legs are all verified tool-capable.

Live probe, 2026-08-27 (`~/.env.smolllm` credentials, both modes):

| leg | tools |
|---|---|
| `deepseek/deepseek-v4-flash`, `groq/openai/gpt-oss-120b`, `omlx/Qwen3.8-27B-*`, `jake/glm` | honoured |
| `gemini/gemini-3.5-flash-lite`, `gemini/gemini-flash-latest` | honoured (streams `finish_reason: stop`) |
| `groq/groq/compound`, `groq/groq/compound-mini`, `ollama/frob/hy-mt1.5` | 400, chain advances |
| `smolayer/antigravity/*`, `smolayer/codex/*` | honoured (verified live 2026-09-03) |

## Not yet supported

`functions` (the legacy function-calling API, superseded by `tools`), `n>1`,
and `/v1/completions` (legacy text completion). Requests using these get a 400.

## Errors and timeouts

smolllm-go v0.3 never reports an operational failure as a Go error: a call comes
back with a stop reason and a message naming **every** failed leg. It also
classifies each failure, and that classification picks the status:

| what happened | status |
|---|---|
| a leg rejected the request shape (400, 422) and aborted the chain | the upstream's own status, `invalid_request_error` |
| every leg advanced and the chain ran out (401, 403, 404, 413, 429, connection, EOF) | 502 `api_error` |
| the whole-call budget expired | 504 `api_error` |
| the client hung up mid-call | 499 |
| nothing was attempted (unusable request, empty chain) | 400 `invalid_request_error` |

The error message names every candidate that failed, not just the last one, so a
chain that dies everywhere says so.

A streamed call has already sent its status line by the time a failure lands, so
it reports one as a terminal frame carrying `finish_reason: "error"` plus an
`error` object, followed by `[DONE]`.

One caveat when a leg fails **after** it has already emitted output: the library
discards that leg's partial turn and starts the next candidate from scratch, but
bytes already on the wire cannot be recalled, so the client sees the abandoned
text (or tool-call fragments) followed by the winning leg's own. The server logs
`stream leg failed after partial output` when this happens. Most legs fail on
their first byte, where nothing has been sent yet.

The optional `timeout` request field (seconds) now bounds the **whole call** —
every fallback leg, every retry, the backoff waits between them, and the
consumption of the stream — not one upstream request. Omit it for the library
default of 600s; `0` disables the bound and leaves only the request context.

Usage follows the OpenAI shape: `prompt_tokens` counts the whole prompt, cached
or not, with the cached share repeated under `prompt_tokens_details.cached_tokens`
and reasoning tokens under `completion_tokens_details.reasoning_tokens`. The
details appear only when a provider reported them.

## Development

```bash
just test    # go test ./... -race
just vet
just build   # writes binary to ~/.local/bin/smolllm-server
```

The chat handler tests (`internal/server/chat_test.go`,
`internal/server/tool_calling_test.go`, `internal/server/failure_test.go`) spin
up an `httptest.Server` posing as an OpenAI-style provider, point smolllm-go at
it via `MOCK_BASE_URL` / `MOCK_API_KEY`, and exercise streaming, non-streaming,
tool calling and every failure-to-status mapping end-to-end without touching the
network.
