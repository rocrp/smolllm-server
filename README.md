# smolllm-server

OpenAI-compatible HTTP front-end for [smolllm-go](../smolllm-go). Routes requests
across 45+ providers via short alias names, runs locally on macOS under launchd.

## Layout

```
cmd/server/        entrypoint
internal/config/   YAML loader, env-file loader, alias resolution
internal/auth/     bearer token middleware
internal/llm/      OpenAI ↔ smolllm adapter (request build + response shapes)
internal/server/   HTTP wiring: chat / embeddings / models / health
internal/apierr/   OpenAI-style error envelope
launch/            LaunchAgent plist + install/reload/uninstall script
```

`go.mod` uses `replace github.com/rocry/smolllm-go => ../smolllm-go`, so local
edits to the library propagate without publishing.

## Configuration

Default config path: `~/.config/smolllm-server/config.yaml`. Override with
`--config <path>` or `$SMOLLLM_SERVER_CONFIG`.

```yaml
server:
  bind: 127.0.0.1:11435
  access_key: change-me       # REQUIRED. SMOLLLM_SERVER_ACCESS_KEY env wins if set
  env_file: ~/.env.smolllm    # provider keys: ${PROVIDER}_API_KEY etc.
  log_level: info

aliases:
  fast: cerebras/qwen-3-235b-a22b-instruct-2507,groq/qwen/qwen3-32b!none,gemini/gemini-flash-latest
  translator: ollama/frob/hy-mt1.5:latest,gemini/gemini-flash-lite-latest
```

Aliases pass straight through to smolllm-go's `WithModel("a,b,c")`, which tries
each in order and falls back on error.

## Install / run

```bash
make install      # build, seed config, link plist, bootstrap agent
make reload       # rebuild + kickstart (full restart; needed for bind changes)
make uninstall    # bootout + remove symlink (binary & config preserved)
make logs         # tail /tmp/personal.smolllm-server.log
```

### Hot reload (SIGHUP)

Send `SIGHUP` to swap config in-place without dropping in-flight requests:

```bash
# Local (PID file under launchd):
launchctl kill -SIGHUP gui/$(id -u)/personal.smolllm-server
# Or, if running in foreground:
kill -HUP $(pgrep -x smolllm-server)
```

Hot-reloadable: `aliases`, `server.access_key`, `server.log_level`,
`server.env_file` contents (re-sourced with overwrite, so rotated provider
keys take effect). Invalid YAML is rejected and the previous snapshot is
retained — the server keeps serving.

NOT hot-reloadable: `server.bind`. A bind change is logged as a warning and
ignored; use `make reload` to actually re-bind.

The agent runs at `127.0.0.1:11435` and reads `~/.env.smolllm` itself on
startup — no wrapper script.

## Endpoints

| | |
|---|---|
| `GET /healthz`           | Public liveness probe. |
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

## Not yet supported

`tools` / `functions` (function calling), `response_format` (JSON mode), `n>1`,
and `/v1/completions` (legacy text completion). Requests using these get a 400.

## Development

```bash
make test    # go test ./... -race
make vet
make build   # writes binary to ~/.local/bin/smolllm-server
```

The chat handler test (`internal/server/chat_test.go`) spins up an
`httptest.Server` posing as an OpenAI-style provider, points smolllm-go at it
via `MOCK_BASE_URL` / `MOCK_API_KEY`, and exercises both streaming and
non-streaming end-to-end without touching the network.
