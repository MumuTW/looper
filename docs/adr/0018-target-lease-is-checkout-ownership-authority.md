# ADR-0018: The Target Lease is checkout ownership Authority

**Status:** Proposed

## Context

`looper takeover` protects a shared filesystem resource: the detached worktree
every Role targeting one pull request shares (`looper-fix-<project>-pr-<N>-detached`,
derived by `git.DetachedPRWorktreePath`). #273 enforces that protection through
**one loop's status**, `human_takeover`, guarded at two boundaries — the claim
predicate and a held-ness comparison inside `LoopsRepository.Upsert`.

That is correct as far as it reaches, and it does not reach far enough. Six
review rounds on #165 and #316 each surfaced another actor arriving at the same
checkout through a path that never consults a loop status, and #316 was closed
for redesign rather than merged. The findings were not a list of missed guards;
they were the same defect restated:

- **Cleanup is check-then-act.** `worktreecleanup.Plan` snapshots a decision and
  `Run` executes it later. The fixer and reviewer terminal-cleanup paths read a
  status and then call `CleanupWorktree`. A re-check narrows the window and
  cannot close it — that is what check-then-act means.
- **A granted claim cannot be revoked.** The claim predicate refuses *new*
  claims. A sibling loop already running on the same checkout keeps running.
- **`retry --discard-worktree-changes` holds a different lock.** The retry path
  takes a mutex keyed on the target it derived; takeover starts from a loop ID
  and takes no such lock, so the reset can land after the hold committed.
- **Held-ness is a boolean, so it cannot distinguish generations.** The guard
  `(loops.status = 'human_takeover') = (excluded.status = 'human_takeover')`
  admits a writer that read during hold generation 1 and writes during
  generation 2, after an intervening handback. This ABA hole is invisible to any
  design that stores *is held* without *which hold*.
- **The guard's key is not the checkout's key.** `humanHoldClaimPredicate`
  fences `pull_request` work on `project_id` **and `repo` and `pr_number`**,
  while `buildWorktreeDirectoryName` derives the directory from **project and
  PR number only**. The two disagree, and it is not a rounding error in one
  direction: see "The lease key is the shared checkout" below.

The common cause: **ownership of a shared resource is being recorded on one of
the things that shares it.** A loop status can express "this loop is held"; it
cannot express "this checkout is spoken for", which is the claim actually being
made, and it cannot be evaluated inside the statement that performs a filesystem
mutation.

## Decision

Ownership of a target's checkout becomes a first-class durable **Target Lease**,
and every actor that mutates that checkout acquires it. The `human_takeover` loop
status is retained as a **projection** for operator surfaces, not as the
authority any code branches on.

### The lease key is the shared checkout, per target type

| target type | key | rationale |
| --- | --- | --- |
| `pull_request` | project + PR number | exactly what `buildWorktreeDirectoryName` puts in `looper-fix-<project>-pr-<N>-detached`; **no repo component** |
| `issue` | project + issue id, among planner loops only | a planner branch is issue-derived; a worker's carries its loop hash and shares with nobody |
| `project` | the individual loop | `assertUniqueActiveLoopCompat` permits concurrent project workers, the lock is `worker:<loopID>`, the branch is per loop |

The key is **the directory, not the work item**. `repo` is deliberately absent
from the `pull_request` key because it is absent from the path. Including it
would mint two leases for one directory whenever a project holds targets from
more than one repository — which `validateLoopTargetProjectCompatibility` allows
outright for any project whose metadata carries no `repo` (it returns nil before
comparing anything). Two leases over one directory is not partial protection; it
is no protection, because each holder is told it owns the checkout.

**A defect this subsumes.** Today's `humanHoldClaimPredicate` on main has the
same inconsistency pointed the other way: it requires `held.repo = ...` to match
before fencing, so repo A PR 42 and repo B PR 42 under one project are treated
as distinct work — while `DetachedPRWorktreePath` maps both to one directory.
Main therefore **under-blocks**: a hold on one is not a hold on the shared
checkout. Keying the lease by path retires that predicate rather than repairing
it, so no separate fix is needed once the lease lands.

These keys are not new: they were established over six review rounds, and
getting them wrong over-blocks. Earlier iterations blocked every project worker
in a project, and blocked a worker on an issue a planner held.

### A lease carries a generation, not a flag

Every acquisition increments a monotonic generation on the lease row. A holder
carries the generation it acquired. Every guarded mutation presents its
generation and is refused when it does not match the current one.

This is the ABA fix, and it is why a boolean is insufficient: takeover →
handback → takeover produces the same *state* twice, and a stale writer from the
first hold is indistinguishable from a current one unless the two holds have
different identities.

### Acquire-then-act replaces check-then-act

For a durable mutation, the generation match is evaluated **inside the writing
statement**, as #273's guard already demonstrates.

For a filesystem mutation, the lease is **held across the operation**: acquire,
mutate, release. A concurrent actor cannot acquire, so it cannot interleave.
This is the part a status cannot do and the reason this ADR exists.

Every actor reaching a checkout acquires: role claims, run start, background
worktree cleanup (both the loop-metadata pass and the checkpoint-derived pass —
ordinary fixer and reviewer loops record the path in the run checkpoint, not
loop metadata), fixer and reviewer terminal cleanup, `retry --discard-worktree-changes`,
stale-run repair, startup recovery, and takeover itself.

### The lease, its status projection, and its queue mutation commit together

Acquisition, the `human_takeover` projection, and the queue cancellation are one
SQLite transaction. So are release, the projection clear, and the requeue.

Neither half is safe alone. A lease with no projection is permanently held and
invisible to handback, which finds no held loop to release — the operator never
got a session and cannot end one. A projection with no lease reports protection
on the dashboard, `looper status`, and the API that no code is enforcing. Release
has the symmetric hazard: freeing the lease before the requeue commits admits a
sibling into a checkout whose owning loop is not yet queued.

This costs nothing structurally, because the transaction already exists.
`loops.Service.Hold` and `loops.Service.Terminate` already run their status write
and `Queue.CancelByLoop` inside `storage.WithTransactionValue`, precisely so "no
scheduler observation can see a cancelled queue item while the loop remains
claimable". The lease write is a third statement in that transaction, under the
same rule and for the same reason.

### Releasing a lease: whose authority, per holder

A lease is released only by the authority that owns its holder. Two holders,
two authorities, and **PID absence is never either of them**.

| holder | release authority |
| --- | --- |
| human | the operator's own durable verb: handback, or explicit close/terminate |
| agent | ADR-0015 Supervisor-confirmed containment/drain |

For agent-held leases this ADR states no rule of its own; it defers to ADR-0015,
which already owns live execution ownership. A missing PID is *recovery
evidence*, classified `uncertain`, and ADR-0015 forbids it from authorizing
release, requeue, terminal marking, or overlapping work. When ownership is
uncertain the lease **stays held** and the target is quarantined. That is the
expensive-but-correct direction ADR-0015 already chose: more quarantine over
aggressive auto-clean.

**An earlier draft of this ADR got this wrong** and should be read as corrected,
not qualified. It carried over the #150 asymmetry — "absence is conclusive,
presence is not" — and made a vanished PID sufficient to release an agent lease.
That asymmetry is sound where #150 applies and wrong here: a checkout is written
by the whole process tree, so a descendant agent can outlive the recorded leader
and keep writing after the PID that named it is gone. Releasing on absence would
hand the directory to cleanup, takeover, or a sibling while that writer is live —
reintroducing, at the filesystem, exactly the failure ADR-0015 closed at the
process. Birth tokens in `internal/processidentity` sharpen *recognition* of an
observed process; they cannot prove a tree drained, and only confirmed drain can.

For human-held leases the authority is the operator's own recorded disposition,
never a probe — the same authority ADR-0015 gives `settleDisposedQuarantine`,
"the operator's existing verb — a durable loop disposition already recorded in
SQLite". A human lease never expires and is never reclaimed by liveness rules.

### Close and terminate release the lease

`human_takeover → terminated` is a legal transition today, `terminated` has no
outgoing transitions, and handback is `human_takeover → queued`. So an operator
who closes a held loop wedges its checkout permanently: the lease has no
remaining release path, and every sibling and cleanup operation on that
directory blocks forever.

**Decision: close/terminate releases the lease, in the same transaction.** The
alternative — refuse close while held, forcing handback first — was rejected.
Terminate already runs a transaction that recognises the human hold and switches
to `UpsertChangingHumanHold` for exactly this case, so release is one more
statement in a transaction that already exists and already knows. Refusing close
would instead make takeover a state with one exit, and strand the operator whose
actual intent is to abandon the work rather than return it to the queue — the
`retry`/`stop`/`close` escape hatch ADR-0015 had to retrofit onto quarantine for
the same reason.

The consequence is accepted deliberately: terminate can release a checkout while
the operator's interactive shell is still live in it. That is correct, because
the daemon has no containment handle for that shell — ADR-0015 classifies CLI
interactive takeover resume as independently lifecycle-owned, with the operator's
terminal owning the agent. The operator's verb is the only authority available,
which is precisely why it is the authority.

### Migration must backfill before the old guards come out

An existing database can already hold `human_takeover` loops. Creating an empty
lease table and removing status-based enforcement in the same upgrade leaves
those checkouts unprotected from the first tick of the new daemon: claims,
startup recovery, and the cleanup pass all consult the lease, find nothing, and
proceed.

The migration therefore synthesizes a human lease for every held target, in the
same transaction that creates the table — migrations already run one file per
`conn.BeginTx`, so this is the existing unit, not a new one. Enforcement is
removed only after that transaction commits.

Ambiguity **fails startup** rather than picking a holder. Two `human_takeover`
loops that share one lease key — the multi-repo case above makes this reachable
on real data — mean two operators believe they hold one directory, and no rule
this ADR could write would make one of them right. The unique index on the lease
key detects it: the backfill's `INSERT ... SELECT` violates the constraint, the
migration transaction rolls back, and the daemon refuses to start with both
loops still visible for an operator to resolve. The ambiguity check is the
schema, not a code path that can drift from it.

### This replaces, it does not extend

#273's per-loop held-bit guard on `Upsert`, the status join in the claim
predicate, and every per-lane status pre-check are removed as the lease takes
over. The verdict on #316 was explicit that stacking more status snapshots and
recovery adapters is the wrong direction, and an implementation that leaves both
mechanisms live has the worst property of either: two authorities that can
disagree.

## Consequences

**What this costs.** A new authority-bearing table and migration, plus a
backfill that can refuse to start the daemon. A lease acquisition on paths that
are currently free. An acquire/release discipline that a future caller can
forget, which is a real hazard: a forgotten release is a wedged target, so
release must be structural (defer, or a helper owning both halves) rather than a
convention. Uncertain agent ownership now wedges a target until an operator acts,
where today it silently proceeds.

**What it buys.** The guarantee stops being tiered. `docs/DESIGN-human-takeover.md`
currently has to split its promises into absolute, best-effort, and not-covered,
because the cleanup paths cannot be made absolute without this. With a lease
held across the mutation, the cleanup paths move into absolute and the two
not-covered cases — an already-running sibling and destructive retry — become
expressible, because both now contend for the same lease.

**What stays out of scope.** Preempting a live holder. Acquisition blocks or
refuses; it does not evict. Evicting an agent that is mid-write to a checkout is
a containment problem, and ADR-0015 already owns live execution ownership.

**Migration.** The `human_takeover` status remains readable and remains what the
dashboard, `looper status`, and the API report, so no operator-facing contract
breaks. What changes is that no code branches on it for enforcement.

### Required implementation proof

The implementation must add contract/invariant integration coverage for:

- a leader that exits while a descendant remains alive: recovery quarantines and
  retains the lease, and no competing checkout mutation starts;
- two legacy targets whose different repositories map to the same detached-path
  candidate: they contend for one lease;
- close/terminate during human takeover: the terminal transition and release
  commit together, so a later actor is not wedged;
- upgrade backfill: every unambiguous held loop receives a lease, while a
  duplicate or unkeyable holder aborts the entire migration and leaves no
  partially migrated authority; and
- injected failures in takeover and handback: neither path can expose a
  lease/status/queue split-brain state.

## Alternatives considered

**Keep hardening the status.** Rejected — this is the sixth round of that, and
each round closed a real gap while the next found another. The pattern is
diagnostic, not coincidental: the gaps are wherever a filesystem mutation is
separated from a status read.

**Version column on `loops` instead of a lease table.** This closes the ABA hole
and nothing else. The mutations that need serializing are not all loop writes —
cleanup and destructive retry touch the filesystem without necessarily writing
the loop at all — so a loop-scoped version cannot be presented at those
boundaries.

**Reuse the `worktrees` row as the lease.** This is the alternative that should
win under "prefer deletion over another layer", and it was previously rejected
on a false premise — that the row vanishes when its checkout is cleaned. It does
not. `WorktreesRepository` has no production delete; cleanup sets `status =
'cleaned'` and `cleaned_at` through `Upsert`. The row outlives its checkout.

It is still rejected, for three reasons that are properties of the schema rather
than assertions about lifecycle:

1. **One checkout is already two rows.** The row's identity is the unique index
   `(project_id, branch)` — *not* `worktree_path`; migration 0002 created
   `idx_worktrees_path` and migration 0004 rebuilt the table without it. Nothing
   forbids duplicate paths, and the Roles produce them: reviewer
   `runPrepareWorktreeStep` records the synthetic `pr-<N>-head`
   (`reviewerWorktreeBranch`), fixer records `detail.headRefName`, and both
   `CreateWorktree` calls resolve to the same `looper-fix-<project>-pr-<N>-detached`
   directory. Since `CreateWorktree` looks up its prior record by `GetByBranch`,
   the second Role misses, mints a new ID, and inserts a second row over the
   same path. A per-row lease would give the two Roles that share a checkout one
   lease each — the exact failure that keeping `repo` out of the lease key
   avoids. Restoring a unique `worktree_path` index is not the one-line fix it
   looks like: it would have to hold for every existing database, and reconciling
   a collision means changing what each Role records as its branch and reworking
   the restore/adopt paths that read rows by branch.

   Stated precisely, because the distinction matters: the collision is
   demonstrable in the code paths above, and was **not** observed in the
   development database checked while writing this — zero duplicate
   `worktree_path` values. So this is a live defect in waiting, not an outage in
   progress, and the index could be restored today on that database. That is a
   reason to fix the index (tracked separately), not a reason to build the lease
   on a row whose identity is the branch when the resource's identity is the path.
2. **The lease is needed where the row does not exist yet.** Takeover holds a
   loop by ID at any status, including before `prepare_worktree` ever ran, and
   the checkpoint-derived cleanup and marker-capture paths deliberately probe
   `DetachedPRWorktreePath` for a directory with no checkpoint worktree at all.
   A lease that can only exist after `CreateWorktree` cannot cover acquisition
   before first checkout. Creating the row early instead means writing an
   "active" worktree record for a directory that does not exist, which every
   existing reader of that table — `ListActive`, `ListCleanupCandidates`,
   `RestoreWorktree` — would take at face value.
3. **The record cannot be its own mutex.** The lease must be held *across*
   `CleanupWorktree`, and `CleanupWorktree`'s durable effect is rewriting that
   very row to `cleaned`. Release and the guarded mutation would be the same
   write, and the row's vocabulary (`active` / `cleaned`) describes whether a
   directory exists, not who may touch it — a held-but-cleaned checkout and a
   free-and-active one are both representable only by adding a second, lease-
   shaped dimension to the row, which is the new concept this alternative was
   supposed to avoid.

**Canonicalize the `worktrees` row so it can be the lease.** The variant of the
above that fixes (1): make both Roles record one branch per shared checkout, add
the unique `worktree_path` index back, and lease the row. Rejected because it
pays a migration over live colliding data and a rewrite of the Role restore/adopt
paths, and still leaves (2) and (3) untouched — the lease would remain
unavailable before first checkout and unable to guard the operation that mutates
it.

**An in-process mutex keyed on the target.** Rejected: it does not survive a
daemon restart, and startup recovery is one of the paths that needs to respect
the lease — the case that currently fails daemon boot.
