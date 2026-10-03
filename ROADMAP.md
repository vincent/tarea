# agentd: Roadmap

A tiny scheduler for LLM jobs (OpenRouter), per-job MCP tools, file-based memory, Telegram sink, Svelte overview panel. One static Go binary.

**Status legend:** `[ ]` todo, `[x]` done. Tick as you go; each phase ends with acceptance criteria.

---

## Current status

Scaffold complete through phases 0-7 (see boxes below). Verified in the build sandbox (Go 1.22, no network for some modules): `go vet`, `go test -race` for every package, `golangci-lint` 0 issues, `svelte-check`, `eslint`, `prettier`, `vitest` and the Vite production build; the binary was started with the built panel embedded and answered the API and SPA routes.

**Not verified yet (do these first on your machine):**
1. `go mod tidy` (creates `go.sum`) and `go build ./...` with Go 1.24+. `internal/mcpx/sdkdial` (the only file importing the official MCP SDK) was written from the v1.0.0 API and could not be compiled in the sandbox; expect a small field/type fix at most.
2. `make lint` with the real `sdkdial` included.
3. A real end-to-end run: `agentd run --dry-run gigs` against OpenRouter and one real MCP server.

**Open from phase 8:** per-job/global budget view, log redaction, Dockerfile, goreleaser, panel auth.

---


## 0. Goals and non-goals

**Goals**
- Run recurring jobs defined as YAML files: schedule + prompt + model + MCP servers + sinks.
- Per-job MCP tool connections (stdio and HTTP), with allow-lists.
- Memory and state as plain files, no database.
- One binary, cross-compilable (`CGO_ENABLED=0`), embedded Svelte panel.
- Cost and token tracking per run, with per-job budget caps.
- Path to a standalone, shareable project (plugins for sinks/providers, job files as the unit of sharing).

**Non-goals (for now)**
- No multi-agent orchestration, no planner/graph framework.
- No database, no auth, no multi-user.
- No sinks other than Telegram (but the interface must allow them).
- No SPA server rendering: the frontend is static files.

## 1. Design principles

1. **Code does the deterministic parts, the LLM does the judgment.** Fetching, dedupe, formatting and budget enforcement are code.
2. **Interfaces live at the consumer, not the producer.** Define small interfaces where they are used (`agent` defines `Provider` and `ToolHost`), so fakes are trivial.
3. **One critical block, fully isolated:** the agent loop has zero I/O of its own. Everything it touches is injected.
4. **Files are the database:** every write is atomic (temp + rename); every job has a lock.
5. **Boring Go:** stdlib first (`net/http`, `log/slog`, `encoding/json`, `embed`, `context`). Add a dependency only if it saves real code.
6. **No premature plugin system.** Interfaces now, dynamic loading never (compile-time registry is enough).

## 2. Repository layout

```
agentd/
├── cmd/agentd/main.go            # CLI: serve | run <job> | validate | version
├── internal/
│   ├── config/                   # load + validate YAML, env expansion
│   ├── llm/                      # Provider interface + OpenRouter client
│   ├── mcpx/                     # MCP client manager (connect, list, filter, call)
│   ├── agent/                    # THE loop: LLM <-> tools, budget, steps
│   ├── memory/                   # memory.md + seen.jsonl stores
│   ├── runlog/                   # run files + runs.jsonl index + rotation
│   ├── sink/                     # Sink interface, registry, telegram/
│   ├── runner/                   # wires one job run end to end
│   ├── scheduler/                # cron, locks, run-now
│   ├── api/                      # JSON HTTP handlers + static embed
│   └── fsx/                      # atomic write, file lock, safe paths
├── web/                          # SvelteKit (static adapter)
├── testdata/                     # golden files, fixtures, fake MCP server
├── .golangci.yml
├── Makefile
├── .github/workflows/ci.yml
└── README.md
```

Runtime data dir (configurable via `--data` or `AGENTD_DATA`):

```
data/
├── jobs/gigs.yaml
├── state/gigs/
│   ├── memory.md
│   ├── seen.jsonl
│   ├── .lock
│   └── runs/2026-10-03T080000Z.json
├── runs.jsonl
└── .env                          # secrets, never in job files
```

## 3. Service split

Ten small packages. The rule: a package exists only if it has its own failure modes or needs its own fake.

| Package | Responsibility | Depends on | Test style |
|---|---|---|---|
| `fsx` | Atomic write, flock, path-traversal-safe join | stdlib | Real temp dirs, crash-simulation test |
| `config` | Parse job YAML, expand `${ENV}`, validate (cron, models, allow-lists) | `fsx` | Table tests + golden error messages |
| `llm` | `Provider` impl for OpenRouter: chat, tool calls, fallbacks, usage and cost | stdlib | `httptest.Server` with recorded responses |
| `mcpx` | Start or connect MCP servers, list tools, filter by `allow`, convert to OpenAI tool schema, call | `go-sdk` | Tiny fake MCP server binary in `testdata/` |
| `agent` | Loop: call LLM, run tool calls, enforce `max_steps`, budget, cancellation, memory tools | interfaces only | **Pure unit tests with fakes** (most important) |
| `memory` | Read/append/replace `memory.md`, size cap, compaction trigger; `seen.jsonl` append + lookup | `fsx` | Temp dirs, concurrency test |
| `runlog` | Write per-run JSON, append `runs.jsonl`, rotate/prune, query for panel | `fsx` | Temp dirs, golden files |
| `sink` | `Sink` interface, registry by `type`; Telegram impl (chunking, escaping) | stdlib | `httptest.Server`, table tests for chunking/escaping |
| `runner` | Build one run: config -> mcpx -> agent -> sinks -> runlog; lock handling | all of the above via interfaces | Integration test with all fakes |
| `scheduler` | Cron registration, hot-reload of jobs dir, run-now, no-overlap | `runner` interface | Fake clock, fake runner |
| `api` | `/api/*` JSON + serve embedded `web/build` | `runlog`, `scheduler` interfaces | `httptest` handlers |

### Key interfaces (defined in the consumer package)

```go
// agent/agent.go
type Provider interface {
    Chat(ctx context.Context, req llm.Request) (llm.Response, error)
}
type ToolHost interface {
    Tools() []llm.ToolDef
    Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}
type Memory interface {
    Read() (string, error)
    Append(note string) error
    Replace(content string) error
}
type Seen interface {
    Has(key string) bool
    Add(keys ...string) error
}

// sink/sink.go
type Sink interface {
    Send(ctx context.Context, m Message) error
}

// runner/runner.go
type Runner interface {
    Run(ctx context.Context, job string, trigger Trigger) (runlog.Summary, error)
}
```

`agent.Run(ctx, Job, Deps) (Result, error)` takes everything via `Deps`. No globals, no `time.Now()` inside (inject `Clock`), no package-level HTTP clients.

### The agent loop (spec)

1. Build messages: system prompt + memory + job prompt.
2. Call `Provider.Chat` with tools (MCP tools + built-ins `memory_append`, `memory_replace`, `seen_check`, `seen_add`).
3. If no tool calls: finish and return the final text.
4. Execute tool calls (sequential at first; parallel opt-in later), append results, truncate oversize tool output.
5. Stop on: no tool calls, `max_steps`, `budget_usd` exceeded, `ctx` cancelled, or tool error threshold.
6. Always return a `Result` with transcript, tokens, cost, stop reason.

## 4. Dependencies (keep this list short)

| Need | Choice | Why |
|---|---|---|
| Cron | `github.com/robfig/cron/v3` | Standard, tiny |
| YAML | `gopkg.in/yaml.v3` | Standard |
| MCP | `github.com/modelcontextprotocol/go-sdk` | Official |
| CLI | stdlib `flag` + subcommands | 4 commands do not need cobra |
| Logging | stdlib `log/slog` | Structured, no dep |
| Assertions | stdlib only (decided) | One less dependency; table tests are readable without it |
| Leaks | `go.uber.org/goleak` | Catch leaked MCP subprocesses and goroutines |

Rule: no HTTP framework, no ORM, no DI container, no LangChain-like library.

## 5. Tooling: golangci-lint golden standard

`.golangci.yml` (golangci-lint **v2** format). Strict, but each linter earns its place.

The authoritative config is [`.golangci.yml`](.golangci.yml) in the repo root (golangci-lint v2.5.0, pinned in the Makefile and CI). Summary of what it enforces:

- **Correctness:** errcheck (incl. type assertions), govet (all analyzers except fieldalignment/shadow), staticcheck, unused, bodyclose, noctx, contextcheck, nilerr, errorlint, exhaustive, gosec, copyloopvar, intrange.
- **Errors:** errname, wrapcheck.
- **Style:** revive, gocritic, godot, misspell, unconvert, unparam, prealloc, perfsprint, nolintlint (every `//nolint` needs a linter name and a reason).
- **Complexity:** cyclop (max 14), funlen (80 lines / 50 statements).
- **Tests:** testpackage, paralleltest, tparallel.
- **Architecture:** depguard forbids `net/http`, `os`, `os/exec` in `internal/agent`.
- **Formatters:** gofmt, goimports, gci (standard, default, then `github.com/vincent/agentd`).

Deviations from the first draft: `gofumpt` became `gofmt` (fewer surprises across editors); `cmd/` is exempt from `testpackage` because `main_test.go` must call `run()`.

Notes:
- The `depguard` rule is the cheap architectural guardrail: it keeps `agent` free of `net/http` and `os`.
- `testpackage` forces black-box tests (`package foo_test`); drop it for a specific package if you must test internals.
- Start with this config on day one. Never ratchet up later.

### Makefile

See [`Makefile`](Makefile): `tools lint test cover web build dev-api dev-web release clean`. `make build` builds the panel, copies it into `internal/webui/dist/` and compiles with `CGO_ENABLED=0`.

## 6. Frontend (Svelte)

**Stack:** SvelteKit + `adapter-static` + TypeScript strict, plain CSS (no UI framework needed). Output in `web/build`, embedded with `//go:embed all:web/build`. No Node at runtime.

**Pages**
- `/` overview: one card per job (status, last run, next run, 7-day cost, last output snippet, "Run now").
- `/jobs/[name]`: run history table, run detail (transcript, tool calls, tokens, cost, error), memory viewer (read-only first), raw YAML viewer.
- `/costs`: 30-day cost per job, simple SVG bars (no chart lib).

**API contract (JSON, defined first, mocked in frontend)**

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/jobs` | Jobs with status, next run, 7-day cost |
| GET | `/api/jobs/{name}` | Job config (secrets redacted) |
| GET | `/api/jobs/{name}/runs?limit=` | Run summaries |
| GET | `/api/jobs/{name}/runs/{id}` | Full run transcript |
| GET | `/api/jobs/{name}/memory` | memory.md |
| POST | `/api/jobs/{name}/run` | Trigger now (202) |
| GET | `/api/health` | Version, uptime |

Optional later: `GET /api/events` (SSE) for live run status.

**Frontend rules**
- Types generated or hand-mirrored in `web/src/lib/types.ts`; one `api.ts` fetch wrapper.
- Vitest for the wrapper and formatters; Playwright smoke test against the real binary.
- Dev proxy: Vite proxies `/api` to the Go server.
- Lint: `eslint` + `prettier` + `svelte-check` in CI.

## 7. Phased roadmap

### Phase 0: Skeleton (0.5 day)
- [x] `go mod init`, repo layout, `.golangci.yml`, Makefile, CI (lint, test, build matrix).
- [x] `main.go` with `version` and `validate` stubs; `slog` setup.
- [x] `fsx`: atomic write, file lock, safe join.

**Done when:** `make all` passes in CI on an empty-but-wired project; `fsx` crash-safety test passes.

### Phase 1: Config and LLM client (1 day)
- [x] `config`: YAML structs, `${ENV}` expansion, validation with precise error messages (job name, field, line). (job + field in errors; line numbers not yet)
- [x] `agentd validate` command over the jobs dir. (also rejects unknown sink types)
- [x] `llm`: OpenRouter chat with tool calling, `models` fallbacks, retry with backoff on 429/5xx, usage + cost extraction.
- [x] Recorded-response tests; context cancellation test.

**Done when:** `agentd validate` flags bad jobs; a fake-server test covers success, 429 retry, fallback, malformed JSON.

### Phase 2: The agent loop (1.5 days) [critical]
- [x] Define `Provider`, `ToolHost`, `Memory`, `Seen`, `Clock` in `agent`. (`Clock` is a `Now func() time.Time` in `Deps`)
- [x] Implement the loop per section 3 with built-in tools.
- [x] Budget enforcement from `usage.cost`, truncation of oversize tool output, bounded tool error retries.
- [x] Fakes in `agent/agenttest` (scripted provider, recording tool host).
- [x] Tests: no tools, multi-step tools, max_steps hit, budget hit, ctx cancel, tool error, malformed tool args, memory tools, seen tools.

**Done when:** coverage on `agent` >= 90%, `depguard` confirms zero I/O imports.

### Phase 3: MCP (1.5 days)
- [x] `mcpx.Manager`: connect stdio and HTTP/SSE servers, per-job lifecycle, `allow` filter, name-spacing (`server__tool`), schema conversion. (`mcpx.Open`/`Host`; the SDK adapter `mcpx/sdkdial` still needs a compile check against the real SDK)
- [ ] Timeouts per call, clean subprocess teardown (process group kill). (per-call timeout done; process-group kill is left to the SDK's CommandTransport, verify with a real server)
- [ ] Fake MCP server in `testdata/` used by tests. (tests use in-process fake `Session`s; a real stdio fake is still open)
- [x] `goleak` in package tests. (mcpx; add to other packages as they grow goroutines)

**Done when:** tools from the fake server show up, filtered, callable; no leaked processes after cancel.

### Phase 4: Memory, seen, runlog (1 day)
- [x] `memory`: read/append/replace, `max_kb` cap, compaction request (agent-level job step, not hidden magic). (compaction is model-driven: `ErrOverCap` tells it to `memory_replace`)
- [x] `seen.jsonl`: append-only, in-memory index at load, optional TTL pruning. (TTL pruning not built, not needed yet)
- [x] `runlog`: per-run JSON, `runs.jsonl` index, rotation by count/age, summary queries (last run, 7-day cost). (rotation by count via `Prune`; by age not built)

**Done when:** concurrent append test passes with `-race`; rotation test passes.

### Phase 5: Sink and runner (1 day)
- [x] `sink` interface + registry; Telegram: `sendMessage`, 4096-char chunking on paragraph boundaries, MarkdownV2 escaping, retry on 429 honoring `retry_after`. (**deviation:** plain text, no MarkdownV2; escaping everything adds nothing visible. Chunking is by runes at paragraph/line/word boundaries)
- [x] `runner`: lock, build deps, run agent, deliver to sinks, write runlog, always release resources.
- [x] `agentd run <job>` CLI with `--dry-run` (print instead of sending).
- [x] End-to-end test with all fakes.

**Done when:** `agentd run gigs --dry-run` works against OpenRouter and a real MCP server; Telegram message arrives.

### Phase 6: Scheduler and API (1 day)
- [x] `scheduler`: register cron from jobs, reload on file change (poll mtime, no fsnotify dep), no overlap, run-now, graceful shutdown (wait for in-flight runs with deadline).
- [x] `api`: handlers per contract, redaction of secrets, `go:embed` static serving with SPA fallback. (plus `GET /api/runs` for the cost page)
- [x] `agentd serve` wires everything; graceful SIGTERM.

**Done when:** a job fires on schedule in a fake-clock test; `curl /api/jobs` returns expected JSON.

### Phase 7: Svelte panel (1.5 days)
- [x] Scaffold SvelteKit, static adapter, TS strict, eslint/prettier. (SvelteKit 3: config lives in `vite.config.ts`, imports use `#lib/`)
- [x] Mock API, build overview page first, then job detail and run detail. (Vite proxies to the real Go server instead of a mock)
- [x] Run-now button with optimistic status; polling every 5s (SSE later). (polling, not optimistic)
- [x] Wire into `make build` and embed.

**Done when:** one binary serves the panel; Playwright smoke passes against it.

### Phase 8: Hardening and release (1 day)
- [ ] Per-job and global budget caps surfaced in UI; kill-switch (disable job from UI writes `enabled: false`).
- [ ] Log redaction (tokens, bearer headers).
- [x] Cross-compile matrix: linux/amd64, linux/arm64, darwin/arm64, windows/amd64 via goreleaser. (Makefile + CI matrix; goreleaser not set up)
- [ ] Sample `data/` dir, Dockerfile (scratch/distroless), systemd unit. (sample data and systemd unit done; Dockerfile open: stdio MCP servers need their runtimes, so scratch images only suit HTTP-only setups)
- [x] README: quickstart, job reference, MCP guide.

**Done when:** a fresh machine goes from release download to first Telegram digest in under 10 minutes.

## 8. Testing strategy

| Layer | Approach | Gate |
|---|---|---|
| Unit | Fakes for every injected interface; table-driven; `t.Parallel()` | `-race -shuffle=on` |
| HTTP clients | `httptest.Server`, recorded OpenRouter and Telegram fixtures | no real network in CI |
| MCP | Fake server binary built in `TestMain` | goleak clean |
| Files | `t.TempDir()`, crash simulation (kill between write and rename) | no partial files |
| Golden | `testdata/*.golden` for config errors, Telegram chunking, run JSON | `-update` flag |
| E2E | Binary + fake OpenRouter + fake MCP + fake Telegram | one smoke test in CI |
| Live (manual) | Opt-in tag `//go:build live` hitting real OpenRouter with a tiny model | never in CI |

Coverage targets: `agent` >= 90%, `sink`/`llm`/`mcpx`/`memory` >= 80%, rest best-effort. Do not chase a global number.

## 9. CI (GitHub Actions)

- `lint`: golangci-lint (pinned action version) + web lint/check.
- `test`: `go test -race ./...` on linux; build-only on macos/windows.
- `web`: `npm ci && npm run build && npm test`.
- `build`: cross-compile matrix, upload artifacts.
- `release` (on tag): goreleaser with checksums.

## 10. Path to a standalone project

- [ ] Job files are the shareable unit: add `agentd jobs install <url|path>` and a `jobs/examples/` folder.
- [ ] `Provider` interface already allows a second backend (direct Anthropic/OpenAI) without touching `agent`.
- [ ] Additional sinks (ntfy, email, webhook, file digest): one file each, registered in `sink`.
- [ ] Optional auth on the panel (static bearer token first, then OIDC).
- [ ] Optional per-job env/secret scopes.
- [ ] Optional storage interface over files (so SQLite/Postgres is a drop-in if ever needed). **Do not build until needed.**
- [ ] Versioned config schema (`apiVersion: 1`) and a JSON Schema for editor completion.

## 11. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Runaway cost | Hard `budget_usd` per job, `max_steps`, global daily cap, visible in UI |
| Prompt injection via tool output | Allow-lists per job, no shell tools by default, tool output size cap, sinks send text only |
| Orphaned MCP subprocesses | Process groups, context-bound lifecycle, goleak tests |
| Corrupt state files | Atomic writes, per-job lock, run files immutable |
| Memory bloat | `max_kb` cap + explicit compaction step + visible in UI |
| `runs.jsonl` growth | Rotation and pruning from Phase 4 |
| Telegram formatting breakage | Escape centrally, fall back to plain text on parse error |
| Scope creep into a framework | Section 1 principles; any new package must justify its own fake |

## 12. Definition of done (v1)

- [ ] Gigs and software-news jobs run daily, deliver to Telegram, dedupe across runs.
- [ ] Panel shows status, next run, cost, last output, errors for every job.
- [ ] `golangci-lint` clean, `go test -race` green, coverage targets met.
- [ ] Single binary for 4 targets from CI, under ~25 MB.
- [ ] README lets a stranger run a job in 10 minutes.

## 13. Reuse checklist (for the next project)

- [ ] Copy `.golangci.yml`, `Makefile`, CI, `fsx`, and the Svelte static-embed setup.
- [ ] Rename module, data dir env var, and binary.
- [ ] Re-run the section 3 table: which packages does the new project actually need?
