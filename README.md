# tarea

A small scheduler for LLM jobs. Each job is one YAML file: a cron schedule, a prompt, the MCP tools it may use, a memory file and one or more sinks (Telegram today). One static Go binary, no database, with an embedded Svelte panel for an overview of every job.

It is deliberately **not** an agent framework. Code does the fetching, deduplication, budgeting and delivery; the model only does the judgment in between.

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
bin/tarea validate             # checks every job file
bin/tarea run --dry-run gigs   # runs once, prints the result instead of sending it
bin/tarea serve                # scheduler + panel on http://127.0.0.1:8080
```

Without the panel: `go build ./cmd/tarea` works too; `/` then answers with a hint, the API is unaffected.

The first time on a fresh clone run `go mod tidy` to create `go.sum`.

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
schedule: "0 8 * * *"        # 5-field cron, server local time
model: anthropic/claude-sonnet-4.5
fallbacks: [openai/gpt-5-mini]   # OpenRouter tries these if the model fails
budget_usd: 0.10             # hard cap per run (default 0.10)
max_steps: 8                 # LLM calls per run (default 8); the last one cannot call tools
enabled: true                # default true
prompt: |
  ...
mcp: [...]                   # see below
memory: {file: memory.md, max_kb: 64}
sinks:
  - {type: telegram, chat_id: "${TG_CHAT}"}
```

- Unknown keys are errors. All problems in a file are reported at once.
- `${VAR}` is expanded in MCP commands/env/url/headers and sink options, from the process environment first and `data/.env` second. An undefined variable is an error. The prompt is never expanded.
- The final model message (the one without tool calls) is the digest sent to the sinks. If it is exactly `NOTHING_NEW`, nothing is sent; the run is still logged.
- A run stopped by budget or step limit is delivered with a `(partial run, stopped: ...)` notice and shown as `partial` in the panel.

### Built-in tools

When a job has memory and a seen-set (always), the model gets `memory_append`, `memory_replace`, `seen_check` and `seen_add`. Deduplication is done by code through `seen_*`, not by asking the model to remember. If memory reaches `max_kb` the write fails with a message telling the model to compact it with `memory_replace`.

## MCP guide

```yaml
mcp:
  - name: library                     # becomes the tool prefix: library__top_artists
    command: ["./mcp-library", "--readonly"]
    env: {LIBRARY_TOKEN: "${LIBRARY_TOKEN}"}
    allow: [top_artists]
  - name: events
    url: https://example.com/mcp      # streamable HTTP
    headers: {Authorization: "Bearer ${EVENTS_KEY}"}
    allow: ["*"]
```

- Exactly one of `command` (stdio) or `url` (HTTP) per server.
- `allow` is mandatory. Use `["*"]` to expose everything. Naming a tool the server does not offer fails the run loudly, so a typo can never silently remove a tool.
- Servers are started per run and closed afterwards. Stdio servers only inherit `PATH`, `HOME`, `USER`, `LANG`, `TMPDIR` plus the `env` you list, so job secrets do not leak to other servers.
- Each tool call has a 60 s timeout; output is truncated to 16 KB; three consecutive tool errors end the run.

## Sinks

`telegram`: `chat_id` per job, bot token from `TELEGRAM_BOT_TOKEN` (or a `token` option). Messages are plain text, split at paragraph/line/word boundaries to stay under Telegram's limit, with retry on 429. New sinks implement `sink.Sink` and register in `cmd/tarea/wire.go`.

## HTTP API

The panel uses this; it is also handy with `curl`. There is **no authentication**: the server binds to `127.0.0.1` by default. Put a reverse proxy with auth in front before exposing it.

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
- A job never overlaps itself: a cron tick during a run is skipped; `run` / the panel's button answer "already running". A stale lock (> 2 h) is taken over after a crash.
- Run history is pruned daily to the newest 500 runs per job (`--keep-runs`).
- `deploy/tarea.service` is a hardened systemd unit.

See `ROADMAP.md` for what is done and what is next.
