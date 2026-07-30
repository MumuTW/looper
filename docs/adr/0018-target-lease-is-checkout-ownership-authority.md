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
| `pull_request` | project + repo + PR number | the `looper-fix-<project>-pr-<N>-detached` checkout every Role on that PR shares |
| `issue` | project + issue id, among planner loops only | a planner branch is issue-derived; a worker's carries its loop hash and shares with nobody |
| `project` | the individual loop | `assertUniqueActiveLoopCompat` permits concurrent project workers, the lock is `worker:<loopID>`, the branch is per loop |

These are not new: they were established over six review rounds, and getting
them wrong over-blocks. Earlier iterations blocked every project worker in a
project, and blocked a worker on an issue a planner held.

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

### A human lease is released only by handback

A lease held by a human never expires and is never reclaimed by liveness rules.
An agent-held lease whose owning process is verifiably gone is released, using
the asymmetry settled in #150: **absence is conclusive, presence is not.** A PID
that holds no process proves the owner is gone; a PID that is occupied proves
nothing, because PIDs are reused, and `internal/processidentity` already records
the birth token needed to tell the two apart.

### This replaces, it does not extend

#273's per-loop held-bit guard on `Upsert`, the status join in the claim
predicate, and every per-lane status pre-check are removed as the lease takes
over. The verdict on #316 was explicit that stacking more status snapshots and
recovery adapters is the wrong direction, and an implementation that leaves both
mechanisms live has the worst property of either: two authorities that can
disagree.

## Consequences

**What this costs.** A new table and its migration — the first persisted state
this program has added. A lease acquisition on paths that are currently free.
An acquire/release discipline that a future caller can forget, which is a real
hazard: a forgotten release is a wedged target, so release must be structural
(defer, or a helper owning both halves) rather than a convention.

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

**Reuse the `worktrees` row as the lease.** Tempting: it already names the
shared resource and carries a unique index on `(project_id, branch)`. Rejected
because its lifecycle is wrong — rows are created by `CreateWorktree` and
cleaned by retention, so a lease would come into existence after the work it
protects and vanish while a human still holds it. The lease must outlive the
checkout it names.

**An in-process mutex keyed on the target.** Rejected: it does not survive a
daemon restart, and startup recovery is one of the paths that needs to respect
the lease — the case that currently fails daemon boot.
