# Spec: Decompose `internal/api/handler.go` into route-group files (#385)

- **Issue:** MumuTW/looper#385 — `refactor(api): decompose handler.go into route-group handlers`
- **Base branch:** `main`
- **Type:** Maintainability refactor of `internal/api` (no behavior change)
- **Source:** Code Tomography Scan RC-001 / UM-001

## Problem

`internal/api/handler.go` is **7,988 lines** with **246 top-level functions** (83 of them `*Handler` methods) covering eight unrelated concerns in one file: health, status/version, config, loops (start/stop/retry/discard/logs), webhooks (forward + stats), projects, pull requests, runs/active-runs, workers/planners, and HITL/Feishu. The `Context` struct carries 15+ function fields and the `Handler` struct is the single sink for every route.

The cost is concrete, not aesthetic:

- **Navigation.** A change to webhook forwarding forces opening a 7,988-line file and scrolling past health/status/config/loops to reach `buildWebhookForwardResponse` (line 585) and `buildWebhookStatusResponse` (line 1369). Reviewers cannot scope a diff to one concern.
- **Coupled review surface.** Any route-group edit lands in the same file as the admission gate, the `serveHTTP` router, and the lock helpers, so a trivial config-response tweak re-opens the most security-sensitive code in the package for review.
- **Test co-location pressure.** `handler_test.go` is already 8,832 lines and `loop_retry_discard_test.go` is 2,051 lines; the single-file production code pushes tests toward the same monolith because there is no narrower unit to attach them to.

The repo already started decomposing this file by concern — `loop_retry_discard.go` (617 lines), `loop_logs_stream.go` (473 lines), `issue_occupancy.go`, `pull_request_lookup.go`, `config_patch.go`, `request_decode.go`, `bootstrap_routes.go`, `browser_guard.go` — all in **`package api`**, same package, no new types. That pattern works; `handler.go` is simply the part that has not yet been split.

## Goals

- Reduce `handler.go` to the router plus the cross-cutting machinery it must own: the `Context`/`Handler` types, `NewHandler`, `ServeHTTP`/`serveHTTP`, authorization/admission, lock helpers, request-ID, and the shared `apiError`/`writeJSON` plumbing.
- Move each route group's response builders and route handlers into a dedicated file named after the concern, so a reviewer can scope a diff to one concern.
- Keep the decomposition **mechanical and behavior-preserving**: no route moves, no JSON shape changes, no status-code changes, no lock-ordering changes, no test-expectation changes.
- Preserve the existing same-package file-split convention already established by `loop_retry_discard.go` et al.
- Land in reviewable slices, each independently green (`go build ./...`, `go vet ./...`, `go test ./...`).

## Non-goals

- **No new `internal/api/handlers/` subpackage and no per-concern handler structs.** See *Approach* for why the subpackage + struct-per-concern design from the issue is rejected in favor of same-package file splits.
- No change to the `Context` struct field set, the `Handler` method set, or the public `NewHandler` signature. `cmd/looperd/main.go:156` is the only production caller and must not move.
- No re-routing, no middleware reordering, no admission-gate logic changes.
- No test rewrite. Tests construct `api.Context{...}` directly (163 sites in `handler_test.go`, 9 in `handler_hitl_test.go`); the refactor must not force those call sites to change.
- No extraction of the lock helpers (`lockLoopRetry`/`lockLoopTarget`/`lockLoopTargetForStatus`) or the admission machinery out of `handler.go` — they are shared by multiple route groups and moving them is a separate decision.

## Authority

> **What is the authority for this action, and why is it not the agent's own structured output?**

Not applicable — this is a pure refactor with no new gate, validation, persisted field, or "verify before acting" check. No new authority is introduced; the existing admission authority (ADR-0015 / #575) and lock authorities (`looperdruntime.LockLoopRequeue`, `LockLoopTarget`) stay exactly where they are. Recorded only because the design guidelines require the question to be answered when touching agent-driven action paths, and the honest answer is "nothing new is enforced."

## Approach

Same-package file decomposition, matching the convention already established by `loop_retry_discard.go`, `loop_logs_stream.go`, `issue_occupancy.go`, `pull_request_lookup.go`, `config_patch.go`, `request_decode.go`, `bootstrap_routes.go`, and `browser_guard.go`. Each new file stays in `package api`, keeps the `*Handler` receiver, and moves only the functions and types that belong to one route group.

### Why not the issue's `internal/api/handlers/` subpackage + per-concern structs

The issue proposes a new `internal/api/handlers/` package where each handler is its own struct holding only its specific dependencies, and the main `Handler` becomes a router. That is a new layer and a new concept (per-concern handler structs with their own dependency surfaces), so per AGENTS.md it must answer the two trade-off questions:

> **Delete this six months from now — what breaks?**

Nothing the file split alone does not already prevent. The pain being fixed is "one 7,988-line file." A subpackage does not make the file shorter than same-package splits do; it adds an import boundary and a per-concern struct on top of the split that already solves the navigation problem.

> **What does it still not catch?**

It does not catch cross-concern coupling through the `Context` struct (15+ function fields shared by every route group), because each new struct would still receive a slice of the same `Context`. It also does not catch the real coupling — the shared lock helpers and admission gate — which the subpackage would either duplicate or re-import, trading one import cycle risk for another.

The simpler move — same-package file splits — solves the stated problem (file size, review scoping, test co-location) without:

- **A new cross-package API surface.** A subpackage forces every moved function to either export or be wrapped, and forces `Handler` to hold/construct one struct per concern. Same-package splits keep the unexported `buildHealthResponse`/`buildStatusResponse`/etc. unexported and called directly from `serveHTTP`.
- **Test churn.** Tests build `api.Context{...}` directly in 163+ sites. A subpackage would either change the construction path or require a parallel test context type. Same-package splits leave every test construction untouched.
- **A new abstraction to keep in sync.** Per-concern handler structs are a concept that must be taught, reviewed for correct dependency injection, and kept aligned as `Context` evolves. The existing `Handler` already *is* the router via the `serveHTTP` switch; splitting files does not require re-deriving that.

Recorded per "Prefer deletion over another layer": the layer-adding direction (subpackage + structs) was considered and rejected because the layer-removing direction (split the file, keep the package) achieves the maintainability goal with strictly less surface. The conclusion is "no subpackage."

### File plan

All new files live in `internal/api/`, `package api`. Each moves only the functions/types listed; `handler.go` keeps the router, types, and shared plumbing. Order is chosen so each slice compiles and tests green independently — health/status/config/webhook first (leaf response builders with few cross-references), loops last (most entangled with locks, retry/discard, logs).

1. **`internal/api/health.go`** — `healthResponse`, `storageHealth`, `migrationHealth`, `buildHealthResponse`. (~30 lines moved)
2. **`internal/api/status.go`** — `statusResponse`, `statusService`, `statusTriage`, `statusBinary`, `versionResponse`, `versionBinaryResponse`, `statusStorage`, `statusScheduler`, `statusAgent`, `statusAgentTimeouts`, `statusAgentRoleTimeouts`, `statusWebhook`, `statusLoopType`, `statusLoops`, `statusSafety`, `statusNotifications`, `statusTools`, `buildStatusResponse`, `buildVersionResponse`, `daemonBinaryStatus`, `daemonExecutablePath`, `buildWorktreeCleanupStatusResponse`, `buildNetworkStatusResponse`, `storageState`, `loadStorageState`, `startedAtISO`, `schemaVersion`, `countLoops`, `sumStatusCounts`, `buildWebhookStatusResponse`, `summarizeWebhookStatus`, `serverBaseURL`, `normalizeRecoverySummary`, `recoveryWithOutstanding`, `statusDegradedReasons`. (~600 lines moved)
3. **`internal/api/config.go`** — `ConfigFieldMetadata`, `ConfigMetadata`, `configResponse`, `configRolesResponse`, `configServerResponse`, `configAgentResponse`, `configDaemonResponse`, `configPackageResponse`, `handleConfigRoute`, `buildConfigResponse`, `buildConfigMetadata`, `sortedMapKeys`, `cloneAgentProfiles`. (~250 lines moved)
4. **`internal/api/webhook.go`** — `buildWebhookForwardResponse`, `isLoopbackRequest`, `hasForwardingProxyHeaders`, `isLoopbackRemoteAddr`. (~80 lines moved)
5. **`internal/api/loops.go`** — the loop route builders and handlers that are not already in `loop_retry_discard.go` / `loop_logs_stream.go`: `loopsListResponse`, `maxLoopsListLimit`, `loopResponse`, `loopLogsResponse`, `loopLogsRunResponse`, `loopLogsAgentPayload`, `retryLoopRequest`, `retryLoopResponse`, `buildLoopsRouteResponse`, `parseLoopsListOptions`, `buildLoopRouteResponse`, and the loop start/stop/retry/discard route entry points that remain in `handler.go` today. `retryLoop` and the discard path stay in `loop_retry_discard.go` (already extracted); this file collects the *route/response* layer for loops. (~700 lines moved)
6. **`internal/api/projects.go`** — `projectsListResponse`, `projectResponse`, `projectValidationResponse`, `discoveryResponse`, `createProjectResponse`, `projectService`, `buildProjectsRouteResponse`, `buildProjectRouteResponse`, `buildProjectDiscoverResponse`, `projectDiscoverResponse`. (~250 lines moved)
7. **`internal/api/pull_requests.go`** — `pullRequestsListResponse`, `pullRequestResponse`, `pullRequestStatusResponse`, `pullRequestLoopStatus`, `pullRequestActionability`, `pullRequestIdentity`, `buildPullRequestsRouteResponse`, `buildPullRequestRouteResponse`, `buildPullRequestStatusResponse`, `findPullRequestLoops`, `serializePullRequestListItem`, `derivePullRequestActionability`, `pullRequestSnapshotDetail`, `checksBlockMerge`, `checksPending`, `reviewBlocksMerge`, `reviewPending`, `boolPtrIfPresent`, `stringFromMap`, `boolPtr`, `snapshotString`, `pullRequestKey`, `groupPullRequestLoops`, `dedupeLatestSnapshots`, `collectPullRequestIdentities`, `findLatestLoopStatus`, `pullRequestLoopStates`. (~450 lines moved)
8. **`internal/api/runs.go`** — `runsListResponse`, `runResponse`, `activeRunsListResponse`, `activeRunsQuery`, `activeRunView`, `activeRunTarget`, `activeRunAgent`, `activeRunWorktree`, `buildRunsRouteResponse`, `buildRunRouteResponse`, `buildActiveRunsResponse`, `buildActiveRunRouteResponse`. (~400 lines moved)
9. **`internal/api/events.go`** — `eventsListResponse`, `entityEventsResponse`, `eventResponse`, `buildEventsRouteResponse`, `buildEntityEventsRouteResponse`, `serializeEvent`. (~120 lines moved)
10. **`internal/api/workers.go`** — `workerCreateResponse` and related types, `buildWorkersCreateResponse`, `buildPlannersCreateResponse`, and the issue-claim dispatch helpers not already in `issue_occupancy.go`. (~600 lines moved)
11. **`internal/api/hitl.go`** — `handleFeishuCardActionRoute` and the HITL card-action helpers. (~remaining HITL lines moved)

After all slices land, `handler.go` should contain only: package/imports, the `const`/`var` block of shared constants and regexps, `RuntimeState`, `activeRunExecutionVerifier`, `Context`, `PullRequestTarget`, `TakeoverResult`, `Handler`, `effectiveConfig`, `NewHandler`, the lock helpers, `ServeHTTP`/`serveHTTP` (the router), `apiError`/`asAPIError`/`internalServerError`/`assertMethod`/`isMutatingHTTPMethod`/`isAdmissionExemptMutationPath`/`admissionMutationDenial`/`admissionStateString`, `authorizeRequest`/`isDirectLoopbackConfigMutation`/`isLoopbackRemoteAddr`-if-not-moved, `normalizePath`, `writeSuccess`/`writeError`/`writeJSON`, `generateRequestID`, and the small shared helpers (`hasValue`, `homeDirOrEmpty`, `currentLooperdTarget`). Target: under ~1,200 lines.

### Slice ordering and verification

Each slice is one commit, ordered so no slice leaves a dangling reference:

1. health → 2. status → 3. config → 4. webhook → 5. events → 6. projects → 7. pull_requests → 8. runs → 9. workers → 10. loops → 11. hitl.

Loops and hitl are last because they have the most cross-references into locks, retry/discard, and the Feishu handshake; moving them first would create temporary import cycles within the package (Go forbids cycles, so this is a compile-time guard, but ordering minimizes churn). After each commit run:

```
gofmt -l .
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 -tests=false -checks='U1000,SA1006,SA4004,SA4006' ./...
go test ./...
go build ./...
```

## Risks

- **Splitting a function that is shared across route groups.** Several helpers (`sortedMapKeys`, `boolPtr`, `stringFromMap`, `snapshotString`, `serverBaseURL`) are used by more than one group. Mitigation: a helper used by ≥2 groups stays in `handler.go` (shared plumbing) or moves to the file of its *primary* user with the other caller importing nothing new (same package). Decide per-helper during the slice; record the decision in the commit body.
- **Accidentally moving a `*Handler` method that the router still calls.** Because everything stays in `package api` with the same receiver, moving a method is purely a file relocation — the call site in `serveHTTP` is unchanged. The only failure mode is forgetting to move an associated type, which `go build` catches immediately.
- **Import drift / unused imports.** Each new file needs its own `import` block. Mitigation: `goimports`/`gofmt` per slice; `go vet` flags unused.
- **Test-file growth smell.** This refactor adds **no new tests** and **no new production state** — it only relocates code. Per the test-growth guideline, the right framing is: this is neither propping up nor covering; it is a zero-test mechanical move, so there is no test-growth signal to answer. If a reviewer asks, the answer is "no tests were added because no behavior changed; existing tests are the regression net."
- **Hidden behavior coupling in `serveHTTP`.** The router reads `h.context.Config` and re-snapshots it for `/config` in `ServeHTTP` before delegating. That logic stays in `handler.go` untouched; no slice touches it.
- **Second-fix signal.** This is the first decomposition PR for `handler.go`; there is no prior `fix:` to the same area to trigger the revert rule. If a follow-up `fix:` lands shortly after, treat it as a signal that a slice moved something it should not have, and prefer reverting the offending slice over stacking a patch.

## Validation

- **Per slice:** `gofmt -l .` clean, `go vet ./...` clean, staticcheck (production checks) clean, `go test ./...` green, `go build ./...` green.
- **Whole refactor:** `handler.go` under ~1,200 lines; no new files outside `internal/api/`; `cmd/looperd/main.go` unchanged; `git diff --stat` shows net line movement (production lines roughly conserved, no meaningful net growth).
- **Behavior parity (the regression net):** the existing test suite is the contract. Specifically:
  - `handler_test.go` (8,832 lines) and `handler_hitl_test.go` exercise the full route table and must pass unchanged.
  - `loop_retry_discard_test.go`, `loop_logs_stream_test.go`, `loop_start_stop_gate_test.go`, `loop_retry_stop_gate_test.go`, `worker_reuse_stop_gate_test.go` cover the lock/retry/discard invariants and must pass unchanged.
  - `config_routes_test.go`, `config_projection_test.go`, `config_patch_test.go` cover config response shape and must pass unchanged.
  - `non_mutating_http_test.go`, `wildcard_auth_test.go`, `config_auth_test.go` cover admission/auth and must pass unchanged.
  - `root_handler_test.go`, `server_test.go` cover routing and must pass unchanged.
- **No new test files.** If a slice cannot compile without a new test, the slice is wrong, not the tests.
- **CI `verify` job** (`.github/workflows/ci.yml`) is the final gate: dashboard → gofmt → go vet → staticcheck → go test → go build.

## Out of scope (explicit)

- Splitting `handler_test.go` / `handler_hitl_test.go` into per-concern test files. Tempting and consistent, but it is a separate, test-only refactor with its own churn risk; doing it in the same PR would hide production moves behind test moves. Track as a follow-up.
- Reducing the `Context` struct field count or introducing a dependency-injection container. That is a real coupling reduction but a behavior-adjacent change; this PR is strictly mechanical.
- Moving the lock helpers or admission gate out of `handler.go`. They are shared; moving them is a separate decision with its own trade-off write-up.
