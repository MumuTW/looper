# Looper lifecycle refactor — plan & tracking

Goal: kill the structural instability + collapse the accidental complexity (the codebase
"doesn't feel as pure as synclo"). Grounded in the 2026-07-15 architecture review.

North star: **branch is truth, worktree is a disposable cache; no infra fault is a resting
terminal state; one lifecycle engine, four roles are plugins; notifications are a tracked
step, not a fire-and-forget side effect.**

## Collaboration model (decided 2026-07-15 with the owner + boss-aligned) — FIRST PRINCIPLE for the whole HITL/notify layer

**Plane + GitHub are the system of record. Feishu is a thin, ONE-WAY, targeted delivery
channel — a doorbell, never the meeting room.** Collaboration does NOT live in a chat group
(groups get flooded → people mute → HITL silently dies, exactly the 0-cards failure of the
07-15 batch).

- **Decisions & artifacts live in the system of record**: spec / approval / product-answer →
  Plane (pages + work-item state + comments); code / review / acceptance → GitHub (PR + review).
- **The human ACTS in the system of record, not in chat.** Approve a spec = comment on the
  Plane page; answer an ambiguity = a Plane/GitHub comment; review = on the PR. Feishu never
  captures the decision.
- **Feishu = one-way push only. Bidirectional is DEPRECATED.** Delete the inbound machinery
  (looper-feishu.nexu.space CF inbox poll, humanInbox thread-reply mirror, detectHumanAsk /
  awaiting_human-resumed-by-thread-reply). This is a big complexity + fragility win (it's the
  infra that broke on the 07-15 network change).
- **Every card is a targeted, one-owner nudge with DEEP LINKS that jump straight to the exact
  spot to act** — not "here's the PR" but the specific NEW review comment / the specific Plane
  page section / the exact unresolved thread. Clicking the link lands the human on the precise
  place they need to respond. Card = @named-owner + one thing to do + a deep link.
- **The "two-way" doesn't vanish — it RELOCATES**: the human acts in Plane/GitHub, and the
  agent WATCHES the system of record for that action via the blocked-condition reconciler
  (P1-B2) — {product-spec appeared, review updated, comment answered, approval given}. Notify
  one-way → poll the source of truth for the response.
- **Observability = a queryable dashboard (loop × Plane × PR state), not chat scrollback.**
  Cards are for ACTIONS; the dashboard is for "what's happening." (07-15 pain: visibility was
  wrongly pinned on group cards.)
- **Mid-run interrupt ("@bot stop") moves off chat** → `looper` CLI / dashboard control
  (cleaner than a chat @-mention).

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
- [x] **A2. Disk-aware backpressure.** ✅ done+tested (commit 77835fe). New `internal/infra/disk` (statfs, df-style used%, walks up to nearest existing ancestor, unix build-tag + fail-open stub). `daemon.diskBackpressure` config (enabled/path/high 85%/hard 93%, default on). Scheduler claim gate `diskBackpressureClamp` (scheduler.go, in `executeClaimPhase`): at/above high watermark clamps this phase's available slots to 0 — governs NEW claims only, in-flight loops untouched. Fails OPEN (disabled/unsupported/unreadable never wedges scheduler). High => warn, hard => error (emergency). `diskUsageStat` seam → deterministic tests (7 clamp cases + disk pkg). Also greened the config suite (defaults+parity fixtures for 19c4183's worktreeCleanup change, left red).
- [ ] A3. Reclaim-on-rest: on a loop reaching rest with branch pushed, evict its worktree; stop the GC protecting paused/failed (service.go:269). Unify the two protected-status sets (service.go:269 vs runtime/worktree_cleanup.go:452).

### P0.5 — worker resume is PR-aware  [kills the "PR already exists" re-run death]
- [ ] A4. On open-PR, persist `prNumber` into the loop IMMEDIATELY (710's loop had `prNumber:None` despite PR 5469 existing → on resume it re-tried open-PR and died "a pull request for this branch already exists"). On resume, if the branch already has an open PR, ADOPT it (transition to shepherding that PR) instead of failing. Surfaced 2026-07-15 when recreate-from-branch (A1) let 710/787 resume — they'd already opened PRs 5469/5543 days ago. Operationally hot-fixed by hand-flipping those two loops to shepherding; the code must do it.

### P1 — failure taxonomy + blocked-condition reconciler  (1-2 wks)  [kills #2, hardens #1/#3]
- [ ] B1. Add `RecoverableInfra` class in failureclass; map missing/stale worktree, ENOSPC, "no such file", fork/exec-under-pressure here → runtime self-heals + resumes (not manual/blind-transient).
- [ ] B2. Generalize `reconcileAwaitingProductSpec` (awaiting_product_spec.go:63) into a condition-reconciler registry: {product-spec, disk-recovered, ci-settled, review-updated, human-answered}. `paused` may only rest on a named, self-clearing condition or true `dead`.
- [ ] B3. Attempt budget that escalates, not silences: cap the `-1` infinite retry with a wall-clock budget → `blocked(infra)` + health signal. Cheap-gate infra retries (worktree/git-local failure must NOT respawn the agent).

### P2 — Feishu becomes a one-way, deep-linked delivery channel (was "notification outbox")  [kills #4 invisible-work, #5 loop_id leak; enacts the collaboration model]
Reframed by the 2026-07-15 collaboration decision: Feishu is push-only; humans act in Plane/GitHub.
- [ ] **C0. DELETE the inbound/bidirectional machinery.** Remove the CF inbox poll (looper-feishu.nexu.space), humanInbox thread-reply mirror, detectHumanAsk, awaiting_human-resumed-by-thread-reply. Human replies no longer come back through Feishu. (Big simplification; removes the infra that broke on the 07-15 network change.) The response is instead observed by the P1-B2 condition reconciler polling Plane/GitHub.
- [ ] **C1. Every card carries DEEP LINKS that jump to the exact spot to act** — the specific NEW review comment (`.../pull/N#discussion_rXXX` / unresolved-thread anchor), the exact Plane page section / comment (`.../pages/<id>#...`), not a bare PR/issue URL. Card = @named-owner + one action + a deep link that lands them precisely where they respond. This is the CORE of "targeted delivery" the owner asked for.
- [ ] C2. Delivery reliability + observability: persist a pending card intent, retry to delivered/failed, log every failure (kill RC4's silent no-op). Make the run-less coordinator anchor card actually render (07-15: coordinator threads = 0 ever — see [[project_looper_refactor_and_card_diagnosis_2026_07_15]]).
- [ ] C3. Replace raw-`loop_id` fallback (notify/gateway.go:1167) with "a real title or nothing, never the id."
- [ ] C4. Mid-run interrupt moves off chat → `looper` CLI / dashboard control (chat @bot-stop is deprecated with C0).
- [ ] C5. (stretch) A queryable dashboard for observability (loop × Plane × PR state) so humans don't rely on chat scrollback — cards are for actions, dashboard is for "what's happening."

### P3 — one lifecycle engine, roles as plugins  (weeks)  [kills #6, makes P0-P2 single-copy]
- [ ] D1. `internal/loops/engine`: generic step loop / classify / checkpoint / resume / claim-lock / persist. Roles supply {step list, executeStep, checkpoint shape, step→boundary map, HITL policy}.
- [ ] D2. Collapse loop-status space: 13 → 4 phases (running / blocked(condition) / done(outcome) / dead(reason)); durable marker becomes THE authority, not a parallel one.
- [ ] D3. Single-owner reconcile lease (generalize the shepherd lock) so two reconciler goroutines can't double-act; idempotent restart.
- [ ] D4. Drop the 16 DTO adapter structs in scheduler.go; roles consume infra types directly. Promote loopError + QueueFailureKind + classify wrapper into failureclass (one copy).

## Rule: every change ships behind a build + the relevant package tests, smoke-tested in ~/.looper-test, never deployed to prod until the whole phase is validated.
