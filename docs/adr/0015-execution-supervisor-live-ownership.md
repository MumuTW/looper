# ADR-0015: Execution Supervisor is live ownership Authority

**Status:** Proposed / Partially Implemented

This ADR is the phased contract for the live execution ownership program.
Status remains **Proposed / Partially Implemented** until every full-program
exit criterion below holds. Do not mark this ADR Accepted when only a subset of
implementation slices land.

**Numbering note:** ADR-0014 is reserved for config-file policy Authority. Do
not reuse 0014 for execution ownership. Related draft work under issue #572 used
an earlier “0014” filename in a draft branch; that draft is superseded by this
ADR and the sliced implementation graph (#575–#581). Keep #572 draft — do not
land it as a competing broad implementation.

## Context

Looper starts agent and subprocess work from many daemon and CLI paths. Live
ownership is currently split across `exec.Cmd` handles, a partial in-memory
`ActiveExecutionRegistry`, persisted `agent_executions` rows (including PID),
queue `running` claims, and PID/process-group probes during stop, shutdown, and
startup recovery.

Those representations can disagree during:

- queue claim before any process exists
- agent spawn and native-resume fallback
- mid-life heartbeat / terminal persistence
- loop stop and daemon shutdown
- startup recovery after crash

Concrete failures include: an unregistered process surviving stop; a stale
`running` write overwriting terminal state; a claimed queue item with no live
owner; recovery treating a reusable PID/PGID as identity and signalling or
marking work terminal without confirmed containment.

This is the HITL gate for the ownership program: runtime PRs must map to the
enforcement matrix below and keep the producer inventory reconciled.

## Decision

The in-memory **Execution Supervisor** is the Authority for live execution
ownership in a running daemon. It owns:

1. **Admission state** for the live daemon (`starting | ready | stopping | degraded`)
2. **Operation leases** that span durable queue claims until durable finalize
3. **Process containment handles** (configure / signal / wait / confirmed drain)
4. **Stop delivery and release** only after confirmed non-runnable ownership
5. **Ordered execution persistence** as durable observation, not a second live Authority

Callers reserve ownership before they claim or start work and cannot publish a
half-started execution. SQLite `agent_executions` rows and process inspection are
**recovery evidence** for operators and startup reconciliation; they do not
authorize a running daemon to release a live handle, start overlapping work, or
treat PID absence as confirmed-dead.

Backends that cannot confirm drain fail loud / degrade instead of inferring
success from signal delivery. A daemon crash loses in-memory Authority; startup
must classify durable observations conservatively before mutations are enabled.

## Authority

> The Authority for a live action is the Execution Supervisor reservation (admission
> + operation lease) and its owned containment handle. SQLite rows and PID/PGID
> probes are recovery evidence only — never live stop, terminal, requeue, or
> overlap Authority while the daemon is live, and never confirmed-dead Authority
> after restart solely because a PID is missing, an argv changed, a lease expired,
> or a leader exited.

Why this is not the agent’s structured output: agents do not own process
lifecycle, queue claims, or stop semantics. Why this is not SQLite alone:
persistence can lag the process and is an observation. Why this is not raw
PID/PGID: numeric IDs are reusable and probe-then-signal is not atomic.

## Trade-off

**Prevents:** unowned durable claims; late live writes after terminal state;
shutdown closing SQLite under an active finalizer; stop/recovery signals to a
reused PID/PGID; dual ready flags that disagree with admission.

**Costs:** one in-memory Supervisor lifecycle; explicit lease release; serialized
per-execution persistence; platform-specific containment adapters; sticky
`degraded` until process restart; more quarantine / manual-intervention during
uncertain recovery instead of aggressive auto-clean.

**Why simpler alternatives are insufficient:**

- Extending only `ActiveExecutionRegistry` registers some agents after spawn and
  does not own queue admission, terminal persistence, or non-agent producers.
- More PID/PGID validation is insufficient: IDs are reusable and
  probe-then-signal is not atomic.
- Trusting SQLite as live Authority is insufficient: observations lag and can
  regress if writers are not ordered.
- Landing one broad “Supervisor everywhere” PR (#572-style) without the phased
  safety floor re-opens dual kill paths and mid-rollout danger.

**Deletion attempt:** remove runtime PID fallback and independent kill paths
entirely when a Supervisor exists. PID inspection remains only as recovery
evidence and for paths still documented outside the Supervisor domain until
their cutover issue closes. Do **not** remove agent live PID fallback until
every in-scope agent producer is Supervisor-owned (#576).

## Phased consequences and enforcement matrix

Implementation order (dependency graph):

```
573 (this ADR) → 575 → 574 → 576 ─┬→ 577 ─┐
                                 └→ 578 → 579 → 580 → 581
                                           └────▲
```

Native GitHub `blocked_by` edges match this graph. Do not add `ready-for-agent`
to a slice while any open blocker remains.

| Order | Issue | Role | ADR consequences in this slice | Enforcement (as of this ADR) |
|------:|-------|------|--------------------------------|------------------------------|
| R0 | #573 | Contract + inventory | Authority statement, matrix, producer inventory, mid-state rules, exit criteria | **Enforced** (docs-only; this document) |
| R1 | #575 | Safety floor: one admission state; stop unsafe recovery PID action | Single admission Authority; no mutation/claim before ready; recovery no-act + quarantine; drain ingress before storage close | **Enforced** |
| R2 | #574 | Process containment handle with confirmed drain | Containment API; kill success = confirmed drain; no production removal of PID fallback yet | **Enforced** |
| R3 | #576 | Own all in-scope agent spawns at common executor boundary | Lease before `cmd.Start`; bind handle before return; stop-kill via handle; remove agent live PID fallback only after full agent coverage | **Enforced** |
| R4 | #577 | Migrate remaining Supervisor-owned non-agent subprocesses | Validation/shell and other in-scope non-agents on containment; no raw PID fallback inside Supervisor domain; shutdown order tested | **Enforced** |
| R5 | #578 | Execution persistence Authority + degrade on mid-life failure | Ordered writer; terminal immutability; hard persist failure degrades; no terminal status before confirmed dead | **Enforced** |
| R6 | #579 | Operation lease owns queue claims until durable finalize | No live `running` claim without lease; release only after durable finalize; finalize failure retains ownership + degrades | **Enforced** |
| R7 | #580 | Full non-mutating coverage when not-ready or degraded | Exhaustive mutation surface audit; scheduler pause; HTTP 503; no dual ready Authority | **Enforced** |
| R8 | #581 | Conservative startup recovery without PID Authority | confirmed dead / observed live / uncertain; uncertain cannot act; PID is evidence only | **Enforced** |

Update the **Enforcement** column as each issue closes (move the slice from
Deferred → Enforced). ADR Accepted only when the matrix has no deferred
in-scope items and full-program exit criteria hold.

### Admission state (enforced by #575)

Authoritative live states: `starting | ready | stopping | degraded`.

- Transitions are monotonic / legal only (e.g. `starting→ready`, `ready→stopping`,
  sticky `degraded` until process restart). Documented in `internal/runtime/admission.go`.
- HTTP mutation readiness and the work-producing **scheduler tick** (discovery,
  HITL, claims, stale-reconcile) are **projections** of this state, not a second
  Authority. Exhaustive non-claim mutation surface audit is enforced by #580
  (see “Full non-mutating coverage” below).
- Admission decisions must be atomic with the action they gate (single `Admission`
  mutex; no dual ready flag that can disagree).

**Admission concept trade-off (R1):**

| | |
|--|--|
| **Failure prevented** | Dual ready flags and claim-only gates that admit enqueue/mutate while starting or after `BeginShutdown`; recovery acting on reusable PIDs without a closed process-lifetime Authority. |
| **Costs** | Sticky `degraded`; no-op ticks while not ready; every new work-producing path must consult admission; more quarantine/`manual_intervention` during uncertain recovery. |
| **Why not simpler** | A boolean ready next to `ownershipAcquired` re-creates dual Authority. Gating only `ClaimNext*` still lets discovery/HITL/reconcile persist queue work. SQLite/PID probes lag and are not atomic with admission. |
| **Deletion attempt** | Drop separate readiness and trust process/agent signals alone — insufficient for multi-PR ownership rollout before Supervisor coverage. |

### Process containment handle (enforced by #574)

Library: `internal/processcontainment`. Additive only — producers still use
existing stop/kill paths until #576/#577. The handle owns configure (Setpgid),
signal, exactly-once leader wait/reap, descendant drain, and **confirmed-dead**
reporting. Kill/Drain success means the owned process group is confirmed
non-runnable and the leader is reaped; signal delivery alone is never success.
Timeouts fail loud (`ErrNotConfirmedDead`) rather than reporting false success.

**Containment concept trade-off (R2):**

| | |
|--|--|
| **Failure prevented** | Stop paths treating SIGTERM/SIGKILL delivery (or leader exit alone) as success while TERM-resistant children or background group members remain runnable. |
| **Costs** | Platform-specific process-group drain; stop latency when descendants resist TERM; callers must handle explicit drain timeout/failure. |
| **Why not simpler** | Reusing PID/PGID probe-then-signal without a wait/reap Authority reintroduces the reusable-ID and false-success failures this program forbids. |
| **Deletion attempt** | Remove dual kill paths immediately — blocked until #576/#577 migrate producers; this slice stays additive so mid-rollout does not create partial cutover. |

### Agent spawn ownership at common executor boundary (enforced by #576)

All **Supervisor-owned agent** producers (Planner, Reviewer, Fixer, Worker,
Coordinator triage, native-resume fallback) go through `agent.ConfiguredExecutor.Start`
with a `SpawnOwner` (the daemon `ActiveExecutionRegistry`):

1. **Admit spawn lease** before `cmd.Start` (also projects daemon `Admission.AllowClaim`).
2. **Configure + Start + Bind** `processcontainment.Handle` before returning to role code.
3. **Stop-vs-spawn / stop-vs-bind**: `BeginLoopStop` / `BeginShutdown` cancels pending
   leases; `BindHandle` kills and confirmed-drains before Start can return success.
4. **Stop/kill** uses the bound handle (confirmed drain). Live in-process SQLite PID
   fallback is removed when the registry is present — not the recovery PID action
   forbidden by #575.
5. **Cancellation** of the lease context prevents native-resume fallback from
   spawning a second process.

This is intentionally **not** the #572 approach (post-spawn register in four role
adapters with incomplete Coordinator coverage). Ownership is at the common executor
boundary so there is no worker-only registry escape hatch.

**Agent ownership concept trade-off (R3):**

| | |
|--|--|
| **Failure prevented** | `looper stop` missing unregistered agents (Coordinator/planner/reviewer/fixer); stop-vs-spawn returning a live unowned process; dual kill via SQLite PID while a handle exists. |
| **Costs** | Every agent Start path consults the registry; stop latency includes confirmed drain; unit tests need Owner nil or a registry; native-resume fallback must rebind handles. |
| **Why not simpler** | Post-spawn adapter Register (#572) leaves Coordinator and race windows unowned. Keeping PID fallback after full coverage reintroduces reusable-ID Authority. |
| **Deletion attempt** | Remove registry and trust only `exec.Cmd` + SQLite — fails stop when live handle is missing and reopens dual Authority. |

### Execution persistence Authority (enforced by #578)

SQLite `agent_executions` is a **durable observation**, not a second live
Authority. Each in-process execution serializes its own writes (`persistMu`);
there is no general global writer subsystem.

**Terminal immutability policy** (storage-level, `AgentExecutionsRepository.Upsert`):

| From → To | Allowed? |
|-----------|----------|
| active (`running`, `cancelling`) → active | yes |
| active → terminal (`completed`, `failed`, `timeout`, `killed`, legacy `success`) | yes |
| terminal → active | **no** (conflict) |
| terminal → different terminal | **no** (first terminal wins; conflict) |
| terminal → same terminal (field enrichment) | yes (e.g. native-resume metadata) |

Zero-row / rejected upserts return `ErrAgentExecutionConflict`, never success.

**Hard persistence failure policy:**

| Path | Behavior |
|------|----------|
| **Initial** ownership after spawn+bind | Fail `Start` loud; kill/confirmed-drain the process; do not leave unowned live process |
| **Heartbeat / output** mid-life | Surface error; **first hard failure** closes admission (`degraded`) |
| **Terminal** | Fail loud via `Wait` error; do not report successful completion; degrade if storage is broken |
| Soft/transient | One retry on SQLITE_BUSY/locked; pure cancel/context death and conflict-after-terminal-won do **not** sticky-degrade |

Terminal status is not published until containment is confirmed dead for owned
handles (`Drain` when needed after leader `Wait`).

**Operator recovery from degraded (persistence hard failure):**

1. Inspect daemon logs for `daemon admission degraded after agent execution persistence failure` and the underlying storage error.
2. Repair storage (disk space, permissions, SQLite integrity) under the configured runtime path (default `~/.looper/`).
3. **Restart `looperd`** — the only recovery path. Admission is sticky degraded until process restart; there is no in-process clear. `MarkDegraded` cancels work-producer contexts (scheduler, cleanup, recovery) but leaves webhook execute live so already-accepted/202 deliveries can still finish discovery; reopening admission without restart would leave a ready-looking daemon with dead scheduler/cleanup producers.
4. After restart, startup recovery classifies durable observations conservatively (#581); do not manually requeue uncertain work.

**Persistence concept trade-off (R5):**

| | |
|--|--|
| **Failure prevented** | Stale live writers regressing terminal rows; silent upsert “success” when zero rows changed; split-brain observations while admission keeps accepting work; terminal status before containment confirmed dead. |
| **Costs** | Sticky degrade stops new work until process restart; terminal conflict means first terminal wins even if a later writer had richer fields; per-execution serialization; hard fail paths kill on initial persist failure. |
| **Why not simpler** | Trusting raw Upsert success leaves #579 release able to free claims on silent no-op writes. Global writer queues add complexity without fixing per-execution ordering. Soft-degrade on cancel creates sticky noise without recovery signal. |
| **Deletion attempt** | Remove mid-life heartbeat persistence and keep only terminal — insufficient for operator progress and native-session capture while live. |

### Operation lease owns queue claims until durable finalize (enforced by #579)

While the daemon is live, a `queue_items.status=running` claim must hold a
Supervisor **operation lease**. One lease spans admit → durable claim → run →
durable complete/cancel/requeue; it is not a growing reservation state machine.

1. **AdmitOperation** before each durable `ClaimNext*` (projects the same admission
   Authority as spawns/claims).
2. Successful claim **BindClaim** returns an explicit **OperationPermit** or
   `ErrOperationLeaseCancelled` (context cancel alone is insufficient).
3. Cancelled lease **never** starts the queue processor / agent spawn; the claim is
   durable-requeued under retained ownership, then released only after requeue commits.
4. **No Release** until complete / cancel / requeue is **durably committed** (and
   verified not still `running`).
5. Claim miss/error **Releases immediately**.
6. Runner error still produces typed durable finalization (`Fail` recovery) before release.
7. Finalization persistence failure **degrades** (#578) and **retains ownership** —
   never pretends release succeeded.

No persisted reservation IDs: SQLite remains the durable observation/finalization
record; the lease is in-memory Supervisor Authority for the live daemon only.

**Operation lease concept trade-off (R6):**

| | |
|--|--|
| **Failure prevented** | Durable `running` claims with no live owner (including the pre-process window); stop/bind races starting a processor after cancel; silent finalize failure + Release leaving unowned claims. |
| **Costs** | Every scheduler claim path admits/binds a lease; stop/shutdown cancel pending binds; finalize failure sticks degrade and retains in-memory ownership until restart; extra GetByID verification after finalize. |
| **Why not simpler** | Compensating “reservation” flags and growing claim state machines reintroduce dual Authority. Trusting runner return alone leaves stranded `running` under a released lease. Releasing before verified durable finalize reopens the unowned-claim window #578 was ordered to close first. |
| **Deletion attempt** | Drop lease and trust process handles only — fails for the claim-before-process interval and for roles that finalize without a live agent handle. |

### Conservative startup recovery classification (enforced by #581)

After a daemon restart, pre-crash containment handles **no longer exist**. Startup
recovery classifies each durable `agent_executions` observation as one of:

| Class | Meaning | Recovery action |
|-------|---------|-----------------|
| **confirmed_dead** | Authority exists to treat the execution as non-runnable | Retire the worktree generation, then finalize the durable row |
| **still_owned_by_this_daemon** | This process holds a live supervisor handle for the execution | Leave it alone; park its claim; stay degraded until it finishes |

**Amended by #149.** The `uncertain` class is deleted, and with it the entire
PID-probe input to recovery. The class existed because PID absence cannot prove
descendant drain — which is still true. What changed is that recovery no longer
needs that proof: containment moved to the filesystem path and the git ref, where
it is enforceable without knowing anything about a process.

**Heartbeat lease (amended by #149).** The claim lease was written once at claim
time with a 5–10 minute TTL against agent timeouts of up to 7200s, so it expired
mid-run on every long agent step: `created_at == updated_at` on every held lock.
It was decorative, and `status='running'` was doing the real double-claim
prevention. The lease is now renewed by `storage.RefreshClaimLeaseForRun` on a
timer inside the executor's supervisor loop, so it is held for exactly as long
as the agent process exists and not one tick longer. Renewing on **output**
instead would tie the lease to chatter: a role whose configured idle timeout
exceeds the lease TTL can be legitimately silent and working, and losing its lock
would let `Locks.Acquire` hand the same target to a second owner. A silent agent
that has actually stalled is still killed by its own idle timeout. “This claim is
still being worked” is now a fact the system can act on; expiry still does not
authorize confirmed-dead on its own.

**Birth identity (amended by #149).** The operating-system birth identity the
common executor records after `cmd.Start` — absolute process start time on
non-Linux platforms, Linux `/proc` start ticks paired with the kernel boot ID —
is **no longer used for recovery classification**, because recovery no longer
inspects processes at all. It remains in use for **webhook forwarder identity**
(`internal/runtime/webhook_lifecycle.go`, ADR-0005), and
`Runtime.ExecutionMatchesProcess` remains available to the HTTP API for operator
inspection. Neither use is Authority for recovery.

The live scheduler performs this observation on each periodic full tick,
independently of available capacity. It never overrides a current-daemon-owned
execution, consumes the active execution row, releases a prior quarantine, or
requeues inherited work without confirmed containment. A later operator pause
therefore remains authoritative over the older quarantine reason.

**Authority for `confirmed_dead` after restart:**

| May authorize confirmed-dead | Must not authorize confirmed-dead |
|------------------------------|-----------------------------------|
| Durable terminal finalization already committed before crash | PID/PGID missing or not running |
| A **current-daemon** owned handle that has completed confirmed drain | Probe-then-signal on raw PID/PGID |
| **The execution's worktree generation has been durably retired** (containment by path divergence, not process proof) | Leader exit alone, taken as evidence that descendants drained |

**The third row is the amendment (#149).** Worktree generation lives on the
`worktrees` table (`generation`, `retired_at`, `checkout_key`; migration 0021)
because the contended thing is the checkout, not the process. Retiring a
generation is a durable, daemon-written, locally-provable fact. The generation is
carried in the directory name (`<checkout>[+g<G>]`, e.g.
`looper-fix-<project>-pr-<N>-detached+g2`), so the next claim of that checkout
lands on a different path: a surviving writer from the previous daemon keeps
writing into a directory no daemon will read, push from, or clean. Its writes
still succeed — we do not claim otherwise — they are simply invisible.

Two things the naive form of this gets wrong, and how each is closed:

- **A checkout is not a branch.** One project can hold an attached planner
  checkout and a detached PR checkout of the same branch at the same time, and
  two PRs can share a head branch. Generations are therefore allocated against
  `checkout_key` — the generation-1 directory name — and the live-row unique
  index is `(project_id, checkout_key) WHERE retired_at IS NULL`. Keying by
  branch collapsed two checkouts into one row, which lost the very path
  `retireExecutionWorktreeGeneration` needs to match against an execution's CWD.
- **A path is not a ref.** For an *attached* checkout — planner and worker loops
  — `git worktree add --force` permits two worktrees on one branch. A retired
  generation that is still alive keeps committing to that shared ref, and the
  live generation's HEAD is a symref to it: it would silently resolve to the
  stale commit and could publish it. Generations past the first therefore check
  out their own local ref (`looper-gen/<G>/<branch>`); the stored record's
  `branch` remains the logical branch, and `Push` still writes
  `refs/heads/<branch>` on the remote. Generation 1 keeps both the historical
  path and the branch itself, so no existing checkout moves.

The generation separator is `+`, which the branch/project sanitizer
(`[A-Za-z0-9._-]`) can never emit — so `feature+g2` is unambiguously generation 2
of `feature`, where a `-g2` suffix would also be a legal generation-1 name for a
branch called `feature-g2`.

Generation retirement is paired with mandatory leased pushes
(`internal/infra/git.Gateway.Push`): every push that updates an existing remote
branch carries a lease, derived from the current remote head when the caller has
no durable prior observation. A stale agent that pushed behind our back surfaces
as a typed `*RemoteHeadChangedError` instead of an opaque rejection.

The prior text — *“Running claims after restart have no live operation lease;
keep them quarantined until an operator or a future durable containment Authority
resolves them”* — is **resolved**: worktree generation retirement is that
Authority. Only `still_owned_by_this_daemon` rows are parked now, and they drain
by finishing rather than by waiting for a human.

Never consume the active row before all repair is durable. Mutations stay closed
until classification finishes (#575/#580).

**Startup recovery concept trade-off (R8):**

| | |
|--|--|
| **Failure prevented** | Two agents writing one checkout, or one PR branch, across a daemon restart. Also the failure the first version of R8 introduced: a park with no exit, which #149 observed surviving multiple restarts. |
| **Costs** | Two persisted columns on `worktrees` and a partial unique index; retired directories that are disk debt until a quiet period passes (reported as a skipped cleanup decision, never as blocked work); a push that now fails loudly where it previously failed opaquely; one one-shot startup reconciliation to release parks written before the fence existed. |
| **Why not simpler** | The simpler thing was tried first and shipped: quarantine on uncertainty. It is correct about what it does not know and has no exit, so the debt is permanent. The alternative simplification — a generation token checked in the git/gh gateways — does not work: those gateways are libraries the daemon calls, not a proxy the agent is forced through, so a token there fences the daemon against itself and leaves the agent's own shell untouched. |
| **Deletion attempt** | Succeeded, and it is the point of the change. Deleted: `classifyStartupProbeEvidence`, `assessExecutionLiveness`, `executionLivenessAssessment`/`Disposition`, `ContainmentObservedLive`, `ContainmentUncertain`, `classificationRequiresQuarantine`, `appendUncertainProcessIdentityEvent`, the two skip branches that made the state terminal, and the loop-pausing/queue-failing halves of `quarantineRecoveryEvidence`. `internal/runtime/recovery_classification.go` went from 218 to ~120 lines and the classification test matrix collapsed from 3 classes × 5 probe reasons to one table of five rows. |

### Full non-mutating coverage when not-ready or degraded (enforced by #580)

R1 landed the admission Authority and known HTTP/claim gates. After producer
cutover (#576–#579), R7 completes the **exhaustive mutation-surface audit** so
HTTP cannot be gated while the scheduler still discovers/enqueues (the dangerous
mid-state named in #580).

**Authority (no dual ready):** `Admission` is the only readiness Authority.
`AllowMutations` (HTTP + tunnel accept) and `AllowClaim` (scheduler tick,
durable claims, spawn leases, worktree cleanup, webhook **accept**) are
projections under the same mutex. Already-accepted webhook worker discovery is
a post-202 commitment and is not re-gated. `ownershipAcquired` is **not** a gate.

**Mutation surface matrix (not-ready / degraded / stopping):**

| Surface | Gate | Closed behavior |
|---------|------|-----------------|
| HTTP mutating methods (`POST`/`PUT`/`PATCH`/`DELETE` under `/api/v1/*`, `/webhook/forward`) | `Handler` → `AllowMutations` | Explicit **503** `SERVICE_UNAVAILABLE` (not silent no-op). Bootstrap mint/exchange exempt. Feishu `url_verification` handshake exempt; card actions gated. |
| Read-only HTTP (`GET` health/status/config/lists/…) | none (reads always allowed) | Available in starting / ready / stopping / degraded |
| Scheduler full tick (planner/coordinator/reviewer/fixer/worker discovery, HITL polls, claim phases, stale-reconcile) | `AllowClaim` at tick entry + mid-tick rechecks per project/lane; `MarkDegraded`/`BeginShutdown` cancel scheduler context | Entire work-producing tick no-ops (prefer pause over “read-only discovery”); in-flight ticks observe cancel |
| Durable `ClaimNext*` / operation-lease admit | `AllowClaim` immediately before each claim | No new claims |
| Agent spawn leases | registry `allowSpawn` → `AllowClaim` | No new agent starts |
| Webhook tunnel deliveries | `allowForward` → `AllowMutations` before Forward | **503** |
| Webhook forwarder accept + worker discovery | `AllowExecute` / `AllowExecuteWhile` at **accept only** (same claim projection); `BeginShutdown`/`Stop` call `CancelExecute`; sticky `MarkDegraded` does **not** cancel accepted discovery | New accepts refuse (**503**); already-accepted/202 queue still completes `CreateOrGetActiveByDedupe` after degrade (no GitHub retry for 202); process-exit aborts in-flight discovery |
| Worktree cleanup pass | `AllowClaim` before pass | No filesystem/DB cleanup mutations while closed |
| Config file hot-reload loop | not gated (policy Authority ADR-0014) | May refresh hot-safe fields; work-producing side effects still hit scheduler/HTTP gates |
| Deferred reviewer recovery requeue | `AllowClaim` before requeue; `MarkDegraded`/`BeginShutdown` cancel recovery context | No requeue after close |
| Shutdown order | `daemonRuntime.Stop`: `BeginShutdown` → HTTP `Server.Stop` drain → `Runtime.Stop` | Aligns with #577: admission → ingress → producers → handles; retain storage / fail loud on incomplete drain |

**Non-mutating coverage concept trade-off (R7):**

| | |
|--|--|
| **Failure prevented** | Partial #575: HTTP 503 while scheduler still discovers/enqueues; webhook Forward queueing work after admission closed; cleanup/spawn paths acting during degraded. |
| **Costs** | Every new work-producing path must call `AllowMutations`/`AllowClaim`; sticky degraded refuses operator mutations until process restart; cleanup and discovery pause during starting. |
| **Why not simpler** | Gating only HTTP leaves producer cutover paths free. Gating only claims leaves discovery/HITL free. Silent no-op HTTP hides unavailability from operators/CLI. |
| **Deletion attempt** | Remove per-surface gates and trust scheduler pause alone — insufficient for tunnel/HTTP/webhook worker and for direct service entrypoints after producer migration. |

### Shutdown order (enforced by #575 admission close + #577 drain/retain)

Drain **admission → ingress → producers → handles/finalizers** before SQLite
close. On timeout or confirmed-drain failure: **retain storage** and fail loud
— never report graceful success with undrained ownership. `Runtime.Stop` skips
`coordinator.Close` when `ActiveExecutionRegistry.BeginShutdown` returns a drain
error (agents **and** tracked Supervisor-owned non-agent handles); late
`ReportDrainFailure` from shell/trusted-review cancel paths is re-collected
after producer waits. `StorageRetained()` is the operator-visible signal.
Independent infra (webhook forwarder, network manager) still stop — they are
not Supervisor domain. Daemon stop order is also `BeginShutdown` → HTTP ingress
`Server.Stop` → `Runtime.Stop` so in-flight mutations drain or fail-loud before
storage close (#580 aligns with #577).

### Non-agent Supervisor-owned producers (enforced by #577)

| Producer | Spawn boundary | Containment |
|----------|----------------|-------------|
| **Worker / Fixer validation shell** | `internal/infra/shell.Run` (`Configure` + `Start` + `Bind`) + `LiveTracker` | Cancel/timeout → `Handle.Kill` confirmed drain; normal exit → `Handle.Drain`; track + `ReportDrainFailure` for retain-storage |
| **Other daemon `shell.Run` work steps** on inventory-listed role helpers | same package boundary | Same as validation when Supervisor-owned; short git/gh/tea remain independently lifecycle-owned (gateway Authority, Tracker nil) but still get group containment when they share `shell.Run` |
| **Trusted review-submit children** | `internal/forge/trusted_review_proxy.go` + `LiveTracker` | `Configure` + `Bind` after Start; cancel → `Handle.Kill`; success path `Drain`; track + report for retain-storage |

Raw PID signal-only stop is removed at these boundaries. Agent live SQLite-PID
fallback was already removed when the registry is present (#576). Non-agent
tracking registers only the live handle (not agent spawn leases) so short jobs
do not grow a second full registry while still feeding shutdown retain-storage.

## Process-producer inventory

Classification:

| Class | Meaning |
|-------|---------|
| **Supervisor-owned (in scope)** | Must eventually hold an operation lease + containment handle under the Supervisor while the daemon is live. Unowned paths block program exit. |
| **Independently lifecycle-owned** | Documented separate Authority; not Supervisor domain. Must not be “half-migrated” into Supervisor without reclassifying. |
| **Explicitly out of scope** | Not a daemon work producer for this program (tests, operator tooling outside looperd, etc.). |

### Supervisor-owned (in scope)

| Producer | Current spawn path (main) | Notes / target cutover |
|----------|---------------------------|------------------------|
| **Planner agent** | `internal/runtime/scheduler.go` planner adapter → `agent.Executor.Start` | **Enforced by #576** via common executor `SpawnOwner` |
| **Reviewer agent** | scheduler reviewer adapter → `agent.Executor.Start` (incl. native-resume fields) | **Enforced by #576** |
| **Fixer agent** | scheduler fixer adapter → `agent.Executor.Start` | **Enforced by #576** |
| **Worker agent** | scheduler worker adapter → `agent.Executor.Start` (incl. native-resume) | **Enforced by #576** (not post-spawn adapter Register) |
| **Coordinator agent** | `internal/coordinator/agent_llm.go` → shared `agent.Executor.Start` | **Enforced by #576**; same executor, not a separate spawn stack |
| **Native-resume fallback** | Same `agent.Executor.Start` with `NativeResumePrompt` / session; fallback to full prompt on resume failure | **Enforced by #576**; cancellation must not spawn a second process after stop |
| **Worker validation shell** | `internal/worker/runner.go` → `shell.Run` (`/bin/sh -c` validation commands) | **Enforced by #577** via `shell.Run` containment spawn boundary |
| **Fixer (and other role) shell helpers** that run daemon-owned long/blocking shell for work steps | e.g. fixer `shell.Run` helpers used during run processing | **Enforced by #577** via same `shell.Run` boundary |
| **Trusted review-submit children** | `internal/forge/trusted_review_proxy.go` spawns `looper review submit` child from daemon-bound proxy | **Enforced by #577**; handle Bind + confirmed Kill/Drain while proxy request is live |
| **Active agent stop / loop halt / daemon shutdown kill of owned agents** | Registry `Kill` via bound containment handle (confirmed drain); no live SQLite PID fallback when registry present | **Enforced by #576** after common-executor ownership; recovery still no raw PID action (#575) |

Queue **claims** themselves are not process producers, but while the daemon is
live a `queue_items.status=running` claim is an owned **operation** under #579
(**Enforced**): scheduler `AdmitOperation` → durable claim → `BindClaim` permit
→ processor → durable finalize → `Release`. Must not exist without a Supervisor lease.

### Independently lifecycle-owned (documented separate Authority)

| Producer | Path | Separate Authority |
|----------|------|--------------------|
| **Webhook forwarder (`gh webhook forward`)** | `internal/runtime/webhook.go` (`newWebhookRuntime` / `runForwarder`; `webhook_forwarder.go` manager is not production-wired) | ADR-0005: local identity gate (PID + process start + command shape). Not Supervisor domain unless this ADR is amended. |
| **Webhook tunnel `gh` subprocesses** | `internal/runtime/webhook_tunnel.go` | Local tunnel lifecycle under webhook tunnel design (ADR-0006 family); not agent work ownership. |
| **CLI-side subprocesses** (feedback agent, interactive takeover resume, daemon spawn/stop, config editor, dashboard browser open) | `cmd/looper` and the packages it calls. **Drift note (#149):** earlier revisions of this table cited `internal/cliapp/feedback.go`, `internal/cliapp/takeover_commands.go`, `internal/cliapp/daemon_runtime.go`, and `internal/cliapp/config_commands.go`. **`internal/cliapp` does not exist in the tree.** Since “no known daemon spawn path may remain unclassified” is a rule of this section, a stale path is itself a contract violation; the row is generalized here rather than left pointing at nothing. | CLI process (or the operator's terminal, or the service manager) owns these children. Not looperd Supervisor. Daemon Authority for parking/stopping a prior run remains Supervisor-owned. |
| **osascript notifications** | `internal/infra/notify/gateway.go` via `shell.Run` | Notification channel lifecycle; short-lived; not queue/agent ownership. |
| **git / gh / tea tool invocations** | `internal/infra/git`, `internal/infra/github`, `internal/forge/tea` via `shell.Run` | Provider/tool gateways; request-scoped short commands under their gateways, not Supervisor agent leases. If a future path becomes long-lived owned work, reclassify before cutover. |
| **Daemon `ps` liveness/identity probes** | `internal/runtime/runtime.go` (`defaultReadProcessCommand` for agent execution match); `internal/runtime/webhook_lifecycle.go` (`defaultProcessProbe.Argv` / `psProcessStart` non-Linux paths for forwarder identity) | Short-lived recovery/identity **evidence** only (see Authority). Not Supervisor-owned work producers and not R4 containment targets. They must never authorize live stop, terminal, requeue, or overlap while the daemon is live, and must not become confirmed-dead Authority after restart solely from PID absence. #575/#581 keep probes as evidence; do not migrate them onto Supervisor leases. |

### Out of Supervisor containment: agent-initiated forge and git mutations

**Added by #149**, because its absence is what made a generation token in the
git/gh gateways read as a complete fence. The Supervisor owns the *agent process*.
It does not own what that process does with the shell it was given.

Looper spawns agents with permissive flags (Claude skip-permissions,
Codex `workspace-write` with network, equivalent vendor flags) and an environment
inheriting `PATH`, `HOME`, `SSH_AUTH_SOCK`, `XDG_CONFIG_HOME`, `SSL_CERT_*`, plus
`cfg.Agent.Env` wholesale — which is where operators put `GH_TOKEN`. Only
`LOOPER_TRUSTED_ENV_FILE` is scrubbed. The result is an unrestricted shell in a
git worktree with `git`, `gh`, an ssh-agent socket, and `gh` hosts.yml reachable.

| Surface | Status |
|---------|--------|
| Filesystem writes into the managed worktree directory | **Contained by generation** (path divergence). Unfenceable by any token — the writer is `open(2)`. |
| `git push` from the agent shell with ambient credentials | **Open.** Forbidden by prompt text only. Leased daemon pushes make a stale agent's push detectable, not impossible. |
| Any `gh` mutation from the agent shell | **Open.** Forbidden by prompt text only. |
| `looper review submit` with the trusted socket env unset | **Open** — falls through to direct submission. |
| `looper <verb>` against the new daemon's HTTP API | **Open.** |
| Trusted review submit *while the socket env survives* | **Closed**, and closes automatically on daemon death — the listener dies with the daemon. |

Closing the four open rows means turning the trusted review proxy into a general
git/gh broker. That is tracked separately and is explicitly **not** a prerequisite
for #149: generation containment and leased pushes are complete on their own terms
for the failure #149 reports.

### Explicitly out of scope

| Producer | Reason |
|----------|--------|
| **E2E harness / test helpers** (`internal/e2e/harness`, `*_test.go` helper processes) | Test infrastructure; not production looperd ownership. |
| **External agent children of vendor CLIs** not started by Looper | Outside Looper’s spawn boundary; containment is process-group based for Looper-started leaders only. |
| **Human-edited forge state** | GitHub/Forgejo remain Authority for work eligibility; not process ownership. |

### Inventory review rules

- No known daemon spawn path may remain unclassified.
- Adding a new producer requires an inventory row in the same PR that introduces it.
- Reclassification (independent → Supervisor-owned or the reverse) requires an
  ADR matrix note and the owning implementation issue updated.
- #576 closes only when every **Supervisor-owned agent** path is covered.
- #577 closes only when every **Supervisor-owned non-agent** path is covered.

## Dangerous mid-state rules

These rules apply during multi-PR rollout and are part of this contract:

1. **Do not remove agent live in-process PID fallback** until every in-scope agent
   producer is Supervisor-owned and verified (#576). “Fallback” means reconstructing
   stop/kill from SQLite PID while the daemon is live — not reintroducing recovery
   PID action forbidden by #575.
2. **Recovery no-act must pair with quarantine**: uncertain evidence must not
   requeue, mark terminal, signal raw PID/PGID, or start overlapping work. Prefer
   existing manual-intervention (or equivalent) states unless a later slice
   justifies a new quarantine concept.
3. **No dual kill paths with partial cutover**: containment library (#574) is
   additive until producers migrate (#576/#577).
4. **Do not land operation-lease release (#579) before persistence Authority (#578)** —
   silent finalize failure plus release creates unowned durable claims.
5. **Do not mark this ADR Accepted** until full-program exit criteria hold, even
   if intermediate slices report local success.

## Full-program exit criteria (ADR Accepted only when all hold)

- [x] R1–R8 issues (#575–#581) implementation acceptance criteria met (matrix Enforced; GitHub close is process)
- [x] Enforcement matrix fully enforced (no deferred in-scope items left open)
- [x] Producer inventory reconciled: every in-scope path Supervisor-owned; independent/out-of-scope documented
- [x] No unowned in-scope agent or subprocess producer remains
- [x] No live stop/shutdown/recovery path uses raw PID/PGID as Authority
- [x] No running queue claim without an owned operation lease while daemon is live (#579)
- [x] Uncertain recovery evidence cannot signal, mark terminal, requeue, or overlap work — **replaced (not violated) by #149**: there is no uncertain class, because containment no longer depends on process knowledge. Recovery still never signals a process. This ADR is `Proposed / Partially Implemented`, so this is a legitimate revision of an unaccepted contract, not a breach of an accepted one.
- [x] Shutdown drains admission → ingress → producers → handles/finalizers before SQLite close (or retains storage / fails loud on timeout)

**Status note:** With R0–R8 enforced and the exit checklist above complete, this ADR
may move to **Accepted** when the program owner closes the contract issue (#573)
and the implementation issues. Do not flip Status in an intermediate slice PR
without that process close-out.

## Follow-on implementation issues

| Order | Issue | Title |
|------:|-------|-------|
| R1 | [#575](https://github.com/nexu-io/looper/issues/575) | Safety floor: one admission state + stop unsafe recovery PID action |
| R2 | [#574](https://github.com/nexu-io/looper/issues/574) | Process containment handle with confirmed drain |
| R3 | [#576](https://github.com/nexu-io/looper/issues/576) | Own all agent spawns at common executor boundary (stop-kill) |
| R4 | [#577](https://github.com/nexu-io/looper/issues/577) | Migrate remaining daemon subprocesses onto containment |
| R5 | [#578](https://github.com/nexu-io/looper/issues/578) | Execution persistence Authority + degrade on mid-life failure |
| R6 | [#579](https://github.com/nexu-io/looper/issues/579) | Operation lease owns queue claims until durable finalize |
| R7 | [#580](https://github.com/nexu-io/looper/issues/580) | Full non-mutating coverage when not-ready or degraded |
| R8 | [#581](https://github.com/nexu-io/looper/issues/581) | Conservative startup recovery classification without PID Authority |

Related: [#572](https://github.com/nexu-io/looper/issues/572) (retargeted by #576; keep draft).

## Non-regression for this ADR

Docs only — no runtime behavior change in the PR that lands ADR-0015.
