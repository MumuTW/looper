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
