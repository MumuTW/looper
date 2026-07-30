# Performance budgets

This document defines Looper's representative performance budgets: what each budget is, why it exists, and how to measure against it. Budgets are targets for a local daemon on developer hardware; the executable checks assert ceilings at 10x each budget so genuine regressions trip without flaking on shared or slow machines.

## Budgets

### API read latency (per endpoint, sequential)

- **Budget: p95 ≤ 50ms** per request against a local `looperd`.
- Surfaces: `GET /api/v1/healthz`, `GET /api/v1/status`, `GET /api/v1/events?limit=100`.
- Rationale: these are the hot read endpoints the CLI and dashboard poll. `/status` is the most expensive (storage migration status + loop/run/queue counts); `/events` reads the newest 100 rows of the event log. 50ms is far above the measured baseline (~1ms) and far below anything a human or poll loop can perceive.

### API read throughput (8 concurrent readers)

- **Budget: ≥ 100 req/s aggregate** per endpoint with 8 concurrent readers.
- Rationale: the daemon serves a handful of local clients (CLI, dashboard, webhook forwarder health checks). 100 req/s is an order of magnitude above realistic load; the measured baseline is ~2.7k–18k req/s depending on endpoint.

### Event-log append

- **Budget: ≤ 5ms per append** into the SQLite `event_logs` table.
- Rationale: every role run, queue transition, and API-visible state change appends events; append latency sits on the daemon's write hot path. Measured baseline: ~28µs.

### Event-log read (newest 100 of 10k rows)

- **Budget: ≤ 10ms** to list the newest 100 events from a 10,000-row log.
- Rationale: 10k rows approximates a busy daemon's recent history; the `/events` endpoint serves exactly this query. Measured baseline: ~1.2ms.

### Live loop-log streaming (8 visible tabs)

- **Concurrency budget: 8 SSE connections**, one combined stdout/stderr connection per visible Logs pane. Hidden tabs close their connection.
- **State budget: one refresh per connection per second**. A refresh is exactly one Loop, latest Run, and latest AgentExecution read, so 8 tabs produce at most 24 repository reads/s rather than the previous dual-stream ~80 full refreshes/s.
- **File-read budget: 5 offset reads/s per stream**, at most 80 ordinary read attempts/s across 8 tabs. Reads return only bytes appended since that connection's cursor; persisted history is not reread during state refresh.
- **Memory/backpressure budget:** the initial snapshot retains at most 200 KiB per stream, and each incremental read/event is at most 64 KiB. A blocked client blocks its own read loop instead of accumulating an unbounded queue; terminal drain keeps the same 64 KiB chunk bound.
- **Fallback:** executions without persisted log paths refresh inline bounded output at 1 Hz. Typed SSE `error` and `end` events plus the dashboard's 1s/2s/5s reconnect backoff keep disconnect, terminal, and error behavior visible.
- Rationale: log bytes are filesystem state, while loop/run/execution records are lifecycle state. They need different polling rates. One connection per pane removes duplicate stdout/stderr lifecycle reads, and offset cursors make cost depend on new output rather than total log size.

## How to measure

The API load test is opt-in (skipped in normal `go test ./...` runs so CI stays fast):

```sh
LOOPER_PERF_BUDGETS=1 go test ./internal/api -run TestPerfBudgets -v
```

The storage benchmarks never run during plain `go test`; run them explicitly:

```sh
go test ./internal/storage -run '^$' -bench BenchmarkEvents -benchtime 200x -v
```

Both enforce the 10x ceilings (`internal/api/perf_budget_test.go`, `internal/storage/perf_budget_test.go`) and log the measured p95/throughput/ns-per-op so results can be compared against the budgets above.

The always-on combined-stream contract test opens four concurrent tabs over 1 MiB stdout/stderr histories, enforces state/file-read counters and 64 KiB chunks, and verifies exact incremental delivery. Run it directly with:

```sh
go test ./internal/api -run TestCombinedLoopLogsStream -count=1
```

## Measured baseline (2026-07-29, Apple M5, darwin/arm64)

| Surface | Measurement | Budget | Baseline |
| --- | --- | --- | --- |
| `GET /api/v1/healthz` | sequential p95 | ≤ 50ms | < 1ms |
| `GET /api/v1/status` | sequential p95 | ≤ 50ms | < 1ms |
| `GET /api/v1/events?limit=100` | sequential p95 (1k events seeded) | ≤ 50ms | ~1ms |
| `GET /api/v1/healthz` | 8 concurrent readers | ≥ 100 req/s | ~18.3k req/s |
| `GET /api/v1/status` | 8 concurrent readers | ≥ 100 req/s | ~12.4k req/s |
| `GET /api/v1/events?limit=100` | 8 concurrent readers | ≥ 100 req/s | ~2.7k req/s |
| event-log append | ns/op | ≤ 5ms | ~28µs |
| event-log list 100/10k | ns/op | ≤ 10ms | ~1.2ms |
