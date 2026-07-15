# Looper lifecycle refactor — plan & tracking

Goal: kill the structural instability + collapse the accidental complexity (the codebase
"doesn't feel as pure as synclo"). Grounded in the 2026-07-15 architecture review.

North star: **branch is truth, worktree is a disposable cache; no infra fault is a resting
terminal state; one lifecycle engine, four roles are plugins; notifications are a tracked
step, not a fire-and-forget side effect.**

Env: isolated worktree `/Users/elian/Documents/looper-refactor` @ `refactor/lifecycle-engine`.
Test in the `~/.looper-test` sandbox (never production) before any prod deploy. Production
keeps running the current binary off `feat/looper-auto-flowchart-runtime`.

## Root causes (from the review)
- RC1 no single authoritative loop state (loop.Status × run.Status × queue × durable markers, hand-synced, already drifted)
- RC2 no "recoverable infra fault" class → every fault is blind-infinite-retry (transient) or park-forever (manual_intervention); no bridge back from paused
- RC3 worktree unbounded + disk-blind; GC time-based and protects the wrong trees (paused/failed); recovery treats worktree object (not branch) as truth
- RC4 notifications fire-and-forget → HITL surface silently no-ops (0 cards)
- RC5 four role runners re-implement one lifecycle (~10-12k accidental lines); intended `infra/worktree` pkg is an empty stub
- RC6 no per-loop concurrency owner → correctness leans on restart timing + optimistic SQL + PID heuristics

## Phased execution (stability first, then the big simplification)

### P0 — worktree becomes a recreatable cache  (days)  [kills #1 disk-leak-stall, #3 stale-death]
- [x] **A1. Recreate-on-missing.** ✅ done+tested `recoverWorkerWorktree` (worker/runner.go:2034): when RestoreWorktree finds no registered worktree, fall through to `CreateWorktree(branch)` (branch is durable — committed locally/pushed) instead of `staleWorkerWorktreeError`→manual_intervention. Only truly-unresolvable branch → NeedsHuman.
- [ ] A2. Disk-aware backpressure: `statfs(worktreeRoot)`; scheduler claim-eligibility gate "don't start new runs above high-watermark (85%)"; hard-stop (93%) refuses claims + emits health signal.
- [ ] A3. Reclaim-on-rest: on a loop reaching rest with branch pushed, evict its worktree; stop the GC protecting paused/failed (service.go:269). Unify the two protected-status sets (service.go:269 vs runtime/worktree_cleanup.go:452).

### P0.5 — worker resume is PR-aware  [kills the "PR already exists" re-run death]
- [ ] A4. On open-PR, persist `prNumber` into the loop IMMEDIATELY (710's loop had `prNumber:None` despite PR 5469 existing → on resume it re-tried open-PR and died "a pull request for this branch already exists"). On resume, if the branch already has an open PR, ADOPT it (transition to shepherding that PR) instead of failing. Surfaced 2026-07-15 when recreate-from-branch (A1) let 710/787 resume — they'd already opened PRs 5469/5543 days ago. Operationally hot-fixed by hand-flipping those two loops to shepherding; the code must do it.

### P1 — failure taxonomy + blocked-condition reconciler  (1-2 wks)  [kills #2, hardens #1/#3]
- [ ] B1. Add `RecoverableInfra` class in failureclass; map missing/stale worktree, ENOSPC, "no such file", fork/exec-under-pressure here → runtime self-heals + resumes (not manual/blind-transient).
- [ ] B2. Generalize `reconcileAwaitingProductSpec` (awaiting_product_spec.go:63) into a condition-reconciler registry: {product-spec, disk-recovered, ci-settled, review-updated, human-answered}. `paused` may only rest on a named, self-clearing condition or true `dead`.
- [ ] B3. Attempt budget that escalates, not silences: cap the `-1` infinite retry with a wall-clock budget → `blocked(infra)` + health signal. Cheap-gate infra retries (worktree/git-local failure must NOT respawn the agent).

### P2 — notification outbox  (~1 wk)  [kills #4 invisible-work, #5 loop_id leak]
- [ ] C1. Persist a pending anchor-card intent; retry to delivered/failed; expose status. Make the run-less coordinator anchor card actually render.
- [ ] C2. Replace raw-`loop_id` fallback (notify/gateway.go:1167) with "post a real title or nothing, never the id."
- [ ] C3. @product spec card actually posts to Feishu (not just a Plane comment) — closes the feature-card gap.

### P3 — one lifecycle engine, roles as plugins  (weeks)  [kills #6, makes P0-P2 single-copy]
- [ ] D1. `internal/loops/engine`: generic step loop / classify / checkpoint / resume / claim-lock / persist. Roles supply {step list, executeStep, checkpoint shape, step→boundary map, HITL policy}.
- [ ] D2. Collapse loop-status space: 13 → 4 phases (running / blocked(condition) / done(outcome) / dead(reason)); durable marker becomes THE authority, not a parallel one.
- [ ] D3. Single-owner reconcile lease (generalize the shepherd lock) so two reconciler goroutines can't double-act; idempotent restart.
- [ ] D4. Drop the 16 DTO adapter structs in scheduler.go; roles consume infra types directly. Promote loopError + QueueFailureKind + classify wrapper into failureclass (one copy).

## Rule: every change ships behind a build + the relevant package tests, smoke-tested in ~/.looper-test, never deployed to prod until the whole phase is validated.
