# tarea

A small scheduler for LLM jobs. Each job is one YAML file: a cron schedule, a prompt, the MCP tools it may use, a memory file and one or more sinks (Telegram today). One static Go binary, no database, with an embedded Svelte panel for an overview of every job.

It is deliberately **not** an agent framework. Code does the fetching, deduplication, budgeting and delivery; the model only does the judgment in between.

![tarea panel](https://raw.githubusercontent.com/vincent/tarea/main/screenshot.png)

```
cron ─► runner ─► agent loop ─► OpenRouter
                     │  ▲
                     ▼  │ tool calls
                  MCP servers (per job, allow-listed)
        │
        └─► sinks (Telegram) · run log · panel
```

## Quickstart

Requirements: Go 1.24+, Node 22+ (only to build the panel).

```sh
make build                      # builds the panel, embeds it, writes bin/tarea
cp data/.env.example data/.env  # then fill in the keys
bin/tarea validate              # checks every job file
bin/tarea run --dry-run jobs-hunt  # runs once, prints the result instead of sending it
bin/tarea serve                 # scheduler + panel on http://127.0.0.1:8080
```

Without the panel: `go build ./cmd/tarea` works too; `/` then answers with a hint, the API is unaffected.

## Data directory

```
data/
  .env                    OPENROUTER_API_KEY, TELEGRAM_BOT_TOKEN, anything ${VAR} in jobs
  jobs/<name>.yaml        one file per job (the file name is the job name)
  state/<name>/           created at runtime
    memory.md             notes the model maintains
    seen.jsonl            dedupe keys, managed by code
    runs/<id>.json        full transcript and output of each run
  runs.jsonl              one summary line per run (feeds the panel)
```

Everything is a plain file; delete `state/<name>` to reset a job. Set `--data` or `$TAREA_DATA` to move it.

## Job reference

```yaml
schedule: "0 8 * * *"           # 5-field cron, server local time
model: anthropic/claude-sonnet-4.5
fallbacks: [openai/gpt-5-mini]  # OpenRouter tries these if the model fails
budget_usd: 0.10                # hard cap per run (default 0.10)
max_steps: 8                    # LLM calls per run (default 8); the last one cannot call tools
max_tokens: 0                   # output cap per model reply (default 0 = provider default)
enabled: true                   # default true
prompt: |
  ...
mcp: [...]                      # see below
memory: {file: memory.md, max_kb: 64}
sinks:
  - {type: telegram, chat_id: "${TG_CHAT}", audio: true}   # audio is optional
```

- Unknown keys are errors. All problems in a file are reported at once.
- `${VAR}` is expanded in MCP commands/env/url/headers and sink options, from the process environment first and `data/.env` second. An undefined variable is an error. The prompt is never expanded.
- The final model message (the one without tool calls) is the digest sent to the sinks. If it is exactly `NOTHING_NEW`, nothing is sent; the run is still logged.
- A run stopped by budget, step limit, tool errors or a cut-off reply (`max_tokens`) is delivered with a `(partial run, stopped: ...)` notice, even if the model produced no final text, and shown as `partial` in the panel. A cut-off reply ends with `[truncated response]`.
- `memory_*` and `seen_add` changes are staged during the run and saved only after delivery succeeded (or there was nothing to deliver). If delivery fails, the run is an `error` and the next run sees the same items again. `--dry-run` saves nothing.
- A failed run sends a short failure notice to the job's sinks: on the first failure after a success, then at most once every 6 hours while it keeps failing. A successful run resets this. Runs cancelled by shutdown do not alert.

### Built-in tools

When a job has memory and a seen-set (always), the model gets `memory_append`, `memory_replace`, `seen_check` and `seen_add`. Deduplication is done by code through `seen_*`, not by asking the model to remember. If memory reaches `max_kb` the write fails with a message telling the model to compact it with `memory_replace`.

## MCP guide

```yaml
mcp:
  - name: library                     # becomes the tool prefix: library__top_artists
    command: ["./mcp-library", "--readonly"]
    env: {LIBRARY_TOKEN: "${LIBRARY_TOKEN}"}
    allow: [top_artists]
  - name: jobshunt
    url: https://mcp.dice.com/mcp     # streamable HTTP
    allow: [search_jobs]
```

- Exactly one of `command` (stdio) or `url` (HTTP) per server.
- `allow` is mandatory. Use `["*"]` to expose everything. Naming a tool the server does not offer fails the run loudly, so a typo can never silently remove a tool.
- Servers are started per run and closed afterwards. Stdio servers only inherit `PATH`, `HOME`, `USER`, `LANG`, `TMPDIR` plus the `env` you list, so job secrets do not leak to other servers.
- Each tool call has a 60 s timeout; output is truncated to 16 KB; three consecutive tool errors end the run.

## Sinks

`telegram`: `chat_id` per job, bot token from `TELEGRAM_BOT_TOKEN` (or a `token` option). Messages are plain text, split at paragraph/line/word boundaries to stay under Telegram's limit, with retry on 429. New sinks implement `sink.Sink` and register in `cmd/tarea/wire.go`.

**Audio:** set `audio: true` on a sink (off by default) and, after the text of a successful run is delivered, it is also synthesized with OpenRouter (`fish-audio/s2.1-pro-free:free`, MP3, first 4000 characters) and sent as an audio message to that sink if it implements `sink.AudioSink` (Telegram does). Speech failures are logged and never fail the run. Partial runs and `NOTHING_NEW` get no audio.

## HTTP API

The panel uses this; it is also handy with `curl`. See [Security](#security) for access control.

### Security

Transcripts contain system prompts, memory and raw tool output, so the API is guarded in two ways:

- **Loopback (default, no token):** requests whose `Host` is not `localhost`, `127.0.0.1` or `::1` are refused (DNS rebinding), as are browser requests carrying a foreign `Origin` or `Sec-Fetch-Site: cross-site` (CSRF). `curl` and scripts send none of those headers and work as usual. Add names with `--allowed-host` (repeatable), e.g. behind a local proxy.
- **Remote (token):** set `TAREA_TOKEN` (environment or `data/.env`, never a flag) and `/api/*` requires `Authorization: Bearer <token>`; the panel asks for it once per browser session. `GET /api/health` and the static panel stay public. `serve` refuses to start on a non-loopback `--addr` without a token, which includes the Docker image (`docker run -e TAREA_TOKEN=... -p 127.0.0.1:8080:8080 ...`).

The token is sent in clear text over HTTP: use TLS (a reverse proxy) beyond your own machine.

| Method | Path | |
|---|---|---|
| GET | `/api/health` | version, uptime |
| GET | `/api/jobs` | overview rows: next run, last run, 7-day cost |
| GET | `/api/jobs/{name}` | configuration with secrets removed (key names only) |
| GET | `/api/jobs/{name}/runs?limit=` | run summaries, newest first |
| GET | `/api/jobs/{name}/runs/{id}` | full run: output and transcript |
| GET | `/api/jobs/{name}/memory` | memory file |
| POST | `/api/jobs/{name}/run` | start now (202, or 409 if already running) |
| GET | `/api/runs?limit=` | all runs, for the cost page |

## Development

```sh
make tools      # installs the pinned golangci-lint
make lint test  # Go + web
make dev-api    # terminal 1: Go server on :8080
make dev-web    # terminal 2: Vite dev server, proxies /api
make release    # cross-compiles into dist/ (CGO off)
```

Layout:

```
cmd/tarea/             CLI wiring only (serve, run, validate, version)
internal/agent/        the loop: pure, all I/O injected (depguard forbids net/http and os here)
internal/runner/       one run: lock, deps, agent, delivery, run log
internal/scheduler/    cron, no-overlap, run-now, hot reload
internal/config/       YAML, env expansion, validation
internal/llm/          OpenRouter client
internal/mcpx/         per-job MCP host; sdkdial/ adapts the official Go SDK
internal/memory/       memory.md and seen.jsonl
internal/runlog/       run history
internal/sink/         sink interface, registry, telegram/
internal/api/          JSON API + static panel
internal/webui/        go:embed of the built panel
web/                   SvelteKit (static adapter)
```

Testing notes: every package takes its collaborators as interfaces, with fakes in `internal/agent/agenttest`. Tests use the standard library only, run in parallel, and the MCP tests use `goleak`.

## Operations

- A job file that becomes invalid is unscheduled (and logged) until it is fixed; valid jobs keep running. The jobs directory is polled every 5 s, no restart needed.
- A job never overlaps itself: a cron tick during a run is skipped; `run` / the panel's button answer "already running". The running process refreshes its lock every 30 s, so a lock left by a crash is taken over after 2 min. A run is cut off after 30 min.
- Run history is pruned daily to the newest 500 runs per job (`--keep-runs`).
- `deploy/tarea.service` is a hardened systemd unit.

See `ROADMAP.md` for what is done and what is next.
