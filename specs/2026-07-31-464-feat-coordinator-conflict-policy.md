# Issue #464 — feat(coordinator): conflict policy — after N failed conflict repairs, close and regenerate instead of repairing forever

## Problem

When `main` moves and a watched PR goes `dirty`, mergewatch classifies it as
`ActionConflict` (`internal/coordinator/mergewatch/mergewatch.go:102-103`) and
the coordinator routes the PR back into the Fixer repair pipeline by applying
the Fixer discovery labels and updating the merge-watch comment
(`internal/coordinator/mergewatch_runner.go:99-111`). The Fixer then rediscovers
the PR through `DiscoverPullRequestsForBaseBranchUpdate`
(`internal/fixer/runner.go:1756`) and enqueues a fresh repair.

There is no bound on this loop. On a fast-moving `main` the same PR can be
repaired, go stale as `main` advances again, conflict once more, and repeat —
each cycle consuming an agent run while the branch drifts further from `main`.
In one day of operation the operator hand-rebased 25 PRs, several of them
twice. A branch that has lost the race against `main` twice is stale by
construction: the code it was written against no longer exists. The economical
move on the third conflict is not another rebase; it is closing the branch and
regenerating the PR from the issue against current `main`.

## Goals

- Bound the number of consecutive conflict-repair attempts mergewatch will
  dispatch for a single PR before giving up on the branch.
- When the bound is reached, route the PR into the shared close-and-regenerate
  flow from #462 (comment → close → issue back to Planner with context
  "superseded by base-branch movement") instead of enqueueing another repair.
- Preserve today's behavior exactly while the count is below the bound.
- Apply the same human-commit guard as #462: PRs carrying human commits
  escalate with `needs-human-review` instead of being closed.
- Make the counter durable across daemon restarts and accurate across queue
  items (a single PR produces many queue items over its lifetime).

## Non-goals

- Building the close-and-regenerate mechanism itself — that is #462, which
  blocks this issue. This spec consumes the shared flow as a dependency.
- Bounding non-conflict repair classes (`ActionRedCI`, transient errors,
  indeterminate states) — those already have their own budget/retry handling.
- Changing the Fixer's own failure-streak circuit breaker, which operates on
  consecutive *run* failures at a fixed step and is orthogonal to the
  *conflict-recurrence* counter introduced here.

## Dependency

> Numbering note: `#464` (this issue) and `#462` (the dependency below) are
> issues in **this repository** (`MumuTW/looper`). The commit history also
> contains squash-merge subjects like `fix: align fixer commit subject guidance
> (#462)` and `feat: add runner retry recovery (#464)`, but those `(#NNN)`
> references identify pull requests in the upstream source repository
> (`nexu-io/looper`, of which this repo is a fork) that were synced into this
> history — they are **not** the issues cited here. GitHub issues and pull
> requests share one number sequence *within a single repository*, so the
> fork's issues #462/#464 and the upstream's PRs #462/#464 do not collide.
> Implementers should follow the issue links at
> `https://github.com/MumuTW/looper/issues/462` and `.../issues/464`.

**Blocked by #462** — the shared close-and-regenerate mechanism (comment →
close PR → re-dispatch the originating issue to Planner with a "superseded by
base-branch movement" context). This spec assumes that flow exists and is
invokable from the coordinator. If #462 lands a function such as
`closeAndRegenerate(ctx, repo, pr, issue, reason)`, this work calls it; if it
exposes a different shape, the call site in the `ActionConflict` branch
adapts. The human-commit detection (`needs-human-review` escalation) is also
owned by #462 and reused here unchanged.

## Approach

### Where the counter lives

The conflict-repair counter must (a) survive daemon restart, (b) count across
queue items — a single PR spawns a new `QueueItemRecord` per repair, so
`QueueItemRecord.Attempts` (`internal/storage/repositories.go:282`) cannot be
the home — and (c) persist across head-SHA changes, since each repair pushes a
new head.

The existing coordinator state home is the merge-watch HTML-comment marker on
the linked Issue (`<!-- looper:coordinator:merge-watch ... -->`), which is
public, durable, idempotent, and preserves ADR-0001's stateless property
(`internal/coordinator/mergewatch/mergewatch.go:18-30`). The marker already
carries `pr`, `head_sha`, `retries`, `first_unknown_at`, `next_retry_at` and is
parsed/serialized in `mergewatch_runner.go:357-427`.

**Decision: extend the existing merge-watch marker with two fields —
`conflict_repairs=N` (the tally) and `conflict_head_sha=<sha>` (the head whose
conflict episode that tally already charged) — rather than introducing
loop-metadata persistence. `conflict_head_sha` deduplicates repeated ticks
against the same dirty head (see Counter semantics); both fields are carried
across head changes (see Carrying the counter across head changes).

Rationale against the loop-metadata alternative (which the issue floated):
the fixer loop's `MetadataJSON` is owned by the Fixer and already carries the
`fixerFailureStreak` state (`internal/fixer/runner.go:633-640, 6399-6438`).
Writing the coordinator's conflict counter into the Fixer's loop metadata
would be a cross-component write into state the Fixer mutates and resets on
head/state changes — exactly the resets that would corrupt a cross-head
counter. The marker is the coordinator's own state, lives on the Issue (not the
PR), and is the established home for cross-tick coordinator state. Extending
it adds a field to an existing layer rather than a new persistence mechanism.

> Delete this six months from now — what breaks? The unbounded conflict loop
> returns: on a fast-moving `main` the same PR is repaired indefinitely, each
> cycle burning an agent run against a branch that is progressively more stale.
> A simpler move — delete the counter and just trust the Fixer's run-failure
> streak — does not catch this, because each conflict repair can *succeed* as a
> run (the rebase applies cleanly) yet still go `dirty` again when `main` moves
> once more. The failure is recurrence against a moving base, not run failure.
>
> What does it still not catch? A PR that conflicts, gets repaired, and then
> sits idle without `main` moving again — that is a healthy repair, not a
> recurrence, and is correctly not counted. It also does not catch a PR whose
> conflict is genuinely hard but on a quiet `main` (no recurrence, so no
> close); that is desirable — close-and-regenerate only helps when `main` is
> the moving target.

### Counter semantics

The counter `conflict_repairs` counts **distinct conflict-repair cycles
dispatched** for the current PR — one per dirty head, not one per poll tick.
The coordinator's `ActionConflict` branch only applies discovery labels and
upserts the comment; it neither enqueues nor observes completion of a distinct
Fixer run. A Fixer that stays queued or running longer than the coordinator's
poll interval leaves the *same* dirty head classified as `ActionConflict` on
every tick, so a naive "increment on every `ActionConflict`" would charge
multiple repairs for a single uncompleted run and close the PR on the third
poll even though no repair has finished.

To count only acknowledged repair cycles, deduplicate by head using
`conflict_head_sha` (the head whose conflict episode the tally already
charged). On each `ActionConflict` classification with observed dirty head
`H`, read `conflict_head_sha` from the prior marker:

- If `H == conflict_head_sha` → this is a repeat tick of an already-counted
  conflict episode (the Fixer has not yet pushed a new head). Do **not**
  increment; re-apply discovery labels idempotently and upsert the comment
  unchanged (today's behavior, no counter mutation).
- If `H != conflict_head_sha` → a new conflict episode for a distinct head:
  - If `conflict_repairs < maxRepairs` → dispatch a repair (today's behavior:
    apply Fixer discovery labels, upsert the merge-watch comment), then
    increment `conflict_repairs` and set `conflict_head_sha = H`.
  - If `conflict_repairs >= maxRepairs` → do **not** enqueue another repair;
    route into the #462 close-and-regenerate flow with reason
    `superseded_by_base_branch_movement`.

A head goes from clean to dirty only after `main` moves; the only way a dirty
head goes clean is a completed repair pushing a *new* head. So a repeat tick
always carries the same SHA as `conflict_head_sha`, and each new episode
carries a different SHA — the equality test charges exactly one repair per
dispatched cycle and never charges a still-running one twice.

With the default `maxRepairs = 2`:
1. First conflict (head A, `conflict_head_sha=""`) → `A != ""` → repair #1,
   counter becomes 1, `conflict_head_sha=A`.
2. Repeat tick (head A still dirty, Fixer running) → `A == A` → no increment.
3. Fixer pushes head B; B clean (`StillPending`); counter carried (see below).
4. `main` moves; B dirty → `B != A` → repair #2, counter becomes 2,
   `conflict_head_sha=B`.
5. Repeat tick (head B still dirty) → `B == B` → no increment.
6. Fixer pushes head C; C dirty → `C != B` → `conflict_repairs=2 >= 2` →
   close-and-regenerate.

This matches "after 2 failed conflict repairs, close and regenerate," counting
repairs that actually dispatched, not polls that observed an unfinished one.

### Carrying the counter across head changes

The marker's existing fields (`retries`, `first_unknown_at`) are reset by
`normalizePrior` / `mergeWatchBaseMarker` when `head_sha` changes
(`mergewatch.go:114-118`, `mergewatch_runner.go:257-262`), because those fields
describe *transient-error* state tied to a specific head. The conflict counter
must **not** reset on head change — each repair produces a new head, and the
recurrence is precisely what we are counting.

Carrying `conflict_repairs` only inside the `ActionConflict` branch is
insufficient: the normal successful-repair lifecycle goes through a non-conflict
tick. After the Fixer pushes head B, the next coordinator tick observes B as
`ActionStillPending`; `mergeWatchBaseMarker` then builds a fresh marker because
the stored head is A, and the `StillPending` branch persists that marker. If
that fresh marker zeroes `conflict_repairs`, the count is lost — and when
`main` later advances and B goes dirty, the next conflict starts again at zero,
so the bound may never be reached whenever a clean tick occurs between repairs.

Implementation requirement: carry `conflict_repairs` and `conflict_head_sha`
across head changes in **every** non-reset action, not only `ActionConflict`.
The single chokepoint is `mergeWatchBaseMarker`
(`mergewatch_runner.go:257-262`): when the prior marker exists for the same PR
but `head_sha` differs, the fresh marker it returns must copy `conflict_repairs`
and `conflict_head_sha` from the prior marker (these are cross-head coordinator
state), while `retries` / `first_unknown_at` / `next_retry_at` keep their
existing head-scoped reset. Because `StillPending`, `Indeterminate`,
`ActionConflict`, `ActionRedCI`, and non-exhausted `ActionTransientError` all
build their persisted marker through `mergeWatchBaseMarker`, centralizing the
carry there preserves the counter through the intermediate clean/pending
observation that a successful repair always produces. The `head_sha` field
still updates to the current head; only `conflict_repairs` and
`conflict_head_sha` are carried across the head boundary.

### Reset conditions

The counter (both `conflict_repairs` and `conflict_head_sha`) resets to its
zero/empty default implicitly whenever the marker is deleted, which already
happens on terminal/transition classifications
(`mergewatch_runner.go:85-130`):
- `ActionMerged` / `ActionHumanDisabledAutoMerge` — marker deleted.
- `ActionBranchProtectionChanged` — marker deleted, issue re-triaged.
- `ActionTransientError` exhausted — marker deleted.
- close-and-regenerate (this issue) — marker deleted as part of closing the PR.

A PR that is closed-and-regenerated starts a fresh PR (new number) with no
prior marker, so its counter begins at 0 — which is correct, since the
regenerated attempt starts from current `main` and the conflict class that
killed the original cannot recur immediately.

### Config

Add to `CoordinatorMergeWatchConfig` (`internal/config/types.go:615-618`):

```go
type CoordinatorMergeWatchConfig struct {
    TransientRetries         int    `json:"transientRetries"`
    MaxIndeterminateDuration string `json:"maxIndeterminateDuration"`
    MaxRepairs               int    `json:"maxRepairs"` // new; default 2
}
```

- Canonical path `roles.coordinator.mergeWatch.maxRepairs` (the persisted
  field on `CoordinatorMergeWatchConfig`), keeping all merge-watch policy in
  one place.
- Compatibility alias `roles.coordinator.conflictPolicy.maxRepairs` — the path
  the issue's acceptance criteria name. The strict config decoder rejects
  unknown nested fields (`DisallowUnknownFields`, `load.go:562`), so simply
  documenting the rename is not enough: an operator using the acceptance
  criteria path would fail configuration loading. Model `conflictPolicy` as an
  explicit input-only alias rather than a second persisted field:
  - Add `ConflictPolicy *PartialCoordinatorConflictPolicyConfig` (with
    `MaxRepairs *int`) to `PartialCoordinatorRoleConfig` (`types.go:1272-1279`).
  - In `mergeCoordinatorRoleConfig` (`normalize.go:1237-1256`), map
    `conflictPolicy.maxRepairs` onto `mergeWatch.maxRepairs` *before* the
    `mergeWatch` overlay so that `mergeWatch.maxRepairs` (canonical) wins when
    both are set. The canonical `CoordinatorMergeWatchConfig.MaxRepairs` remains
    the single source of truth; `conflictPolicy` is a decode-time alias only.
  - Both paths are accepted by the strict decoder; if both are set and differ,
    `mergeWatch.maxRepairs` takes precedence and `conflictPolicy` is treated as
    the deprecated form.
- Default `2` in `internal/config/defaults.go:220-223`.
- Validation in `internal/config/validate.go` (alongside the existing
  `mergeWatch.transientRetries` / `maxIndeterminateDuration` checks around
  line 1548): `maxRepairs` must be a positive integer.
- Partial overlay `PartialCoordinatorMergeWatchConfig` (`types.go:1267-1269`)
  gains `MaxRepairs *int`, merged in `mergeCoordinatorMergeWatchConfig`
  (`normalize.go:1330`). Because `ProjectRefConfig.Roles` is
  `*PartialRoleConfigs` (`types.go:721`) and `ProjectRoleConfigs`
  (`project_roles.go:10-26`) merges project partials via `mergeRoleConfigs`
  (`normalize.go:1205-1235`), `projects[].roles.coordinator.mergeWatch.maxRepairs`
  is an effective per-project override — and the `conflictPolicy` alias, living
  on the same `PartialCoordinatorRoleConfig`, is project-overridable too. This
  is consistent with the existing `transientRetries` /
  `maxIndeterminateDuration` partial fields, which are already
  project-overridable through the identical path; see Out of scope.

### Routing change

The change is localized to the `ActionConflict` case in
`applyMergeWatchLocked` (`mergewatch_runner.go:99-111`). Today it unconditionally
applies Fixer labels and upserts the comment. The new logic:

1. Read `conflict_repairs` and `conflict_head_sha` from the prior marker (both
   carried across head changes by `mergeWatchBaseMarker`).
2. If the observed dirty head `H == conflict_head_sha`, this is a repeat tick of
   an already-counted episode: apply today's behavior (labels + comment upsert)
   with **no** counter mutation and stop. (Labels and comment upsert are
   idempotent, so re-applying them on a repeat tick is a no-op write.)
3. If `H != conflict_head_sha` (a new episode) and `conflict_repairs >= maxRepairs`:
   - Check the human-commit guard (reused from #462). If the PR has human
     commits, escalate with `needs-human-review` and stop (do not close).
   - Otherwise invoke the #462 close-and-regenerate flow with reason
     `superseded_by_base_branch_movement`, then delete the merge-watch marker.
4. If `H != conflict_head_sha` and `conflict_repairs < maxRepairs`: today's
   behavior (apply labels, upsert comment), with `conflict_repairs` incremented
   and `conflict_head_sha = H` written into the new marker.

`ActionRedCI` is unchanged — it is not a base-movement recurrence and keeps its
existing routing.

### Authority

> What is the authority for this action, and why is it not the agent's own
> structured output?

The authority is the coordinator's own durable marker state — the count of
conflict-repair cycles *the coordinator itself dispatched*, recorded on the
Issue. It is not inferred from agent output or infra state; it is a tally of
coordinator actions. The close-and-regenerate decision is therefore grounded
in the coordinator's own action history, not in a guess about what the agent
did. Infra signals (`MergeableState == "dirty"`) are the *trigger* for
incrementing, not the *authority* for closing — the authority is the persisted
count of prior dispatches.

## Risks

- **Counter drift if marker is edited externally.** The marker is a public
  HTML comment; a human could delete or alter it. This is the same trust model
  the existing `retries` field already has, and the consequence of tampering is
  bounded: at worst the count restarts and one extra repair is attempted
  before close-and-regenerate. No data loss; no escalation beyond the existing
  human-commit guard.
- **Close-and-regenerate availability.** This issue is blocked by #462; if
  #462's flow is delayed, the `conflict_repairs >= maxRepairs` branch must
  fail closed (leave the PR open, log, do not loop) rather than enqueue
  forever. The implementation should guard on the close-and-regenerate entry
  point existing and degrade to a logged no-op + marker retention if it is
  unavailable.
- **maxRepairs too low on a slow `main`.** With `maxRepairs = 2`, a PR on a
  `main` that moves twice in quick succession is closed after two repairs even
  if a third would have held. The default is a trade-off favoring economy; it
  is configurable, and the regenerated attempt starts from current `main`, so
  the work is not lost — it is restarted against the real target.
- **Shared marker field parsing.** Adding `conflict_repairs` and
  `conflict_head_sha` to the marker touches `parseMergeWatchComment` and
  `upsertMergeWatchComment` (`mergewatch_runner.go:357-427`). Older daemons
  that do not know the fields will drop them on their next upsert, resetting
  the count (and the dedup head). This is acceptable during rollout (degrades
  to "one extra repair") and resolves once all daemons are upgraded.

## Validation

### Unit

- `parseMergeWatchComment` round-trips a marker carrying `conflict_repairs=N`
  and `conflict_head_sha=<sha>` and preserves both values when head changes
  (carry-across-head test).
- `mergeWatchBaseMarker` copies `conflict_repairs` and `conflict_head_sha`
  from the prior marker when `head_sha` differs (the cross-head carry), while
  still resetting head-scoped `retries` / `first_unknown_at`.
- `applyMergeWatchLocked` with a **new** dirty head (`H != conflict_head_sha`)
  and `conflict_repairs < maxRepairs` applies Fixer labels, upserts comment,
  increments `conflict_repairs`, and sets `conflict_head_sha = H`.
- `applyMergeWatchLocked` with a **repeat** dirty head (`H == conflict_head_sha`)
  re-applies labels/upserts the comment but does **not** increment
  `conflict_repairs` (same-head tick dedup).
- `applyMergeWatchLocked` with a new dirty head and
  `conflict_repairs == maxRepairs` does **not** apply Fixer labels and instead
  invokes close-and-regenerate (mocked #462 entry point), then deletes the
  marker.
- Human-commit guard path: `conflict_repairs >= maxRepairs` on a PR with human
  commits escalates with `needs-human-review` and does not close.
- Config validation rejects `maxRepairs <= 0` and accepts the default `2`.
- Config load accepts both `roles.coordinator.mergeWatch.maxRepairs` and the
  `roles.coordinator.conflictPolicy.maxRepairs` alias; when both are set and
  differ, `mergeWatch.maxRepairs` wins. A project-scoped
  `projects[].roles.coordinator.mergeWatch.maxRepairs` overrides the global
  default via `ProjectRoleConfigs`.

### Contract / invariant (integration)

Per the review guidelines, cross-component lifecycle regressions prefer
contract/invariant integration coverage over unit-only tests. Add a
coordinator integration test that drives the full conflict loop:

- Persist a PR that goes `dirty` repeatedly across ticks with `main` advancing
  between each. Assert the counter increments and survives a simulated
  daemon restart (marker re-read from the issue comments), the boundary at
  `maxRepairs` triggers close-and-regenerate (not a third repair enqueue), and
  the regenerated issue carries the "superseded by base-branch movement"
  context. This is the regression that would catch a counter that resets on
  head change or a close path that skips the human-commit guard.
- The same loop must include **repeated same-head ticks** (the Fixer queued or
  running longer than the poll interval, so the unchanged dirty head is
  classified `ActionConflict` on consecutive ticks): assert the counter
  increments at most once per distinct head and does not close the PR on a
  third poll of the same uncompleted repair.
- The same loop must include the **intermediate clean/pending observation**
  between repairs: after the Fixer pushes head B, drive a tick that observes
  B as `ActionStillPending` before `main` advances and B goes dirty again.
  Assert `conflict_repairs` is preserved through that clean tick (not reset to
  zero), so the bound is still reachable when a clean observation occurs
  between repairs.

### Build / CI

- `go vet ./...`, `go build ./...`, `gofmt -l .` clean.
- `staticcheck` production-only checks pass.
- `go test ./...` green, including the new unit and integration coverage.

## Out of scope

- Per-project `maxRepairs` tuning is **not** blocked: because the field rides
  the existing `PartialCoordinatorMergeWatchConfig` overlay that
  `transientRetries` / `maxIndeterminateDuration` already use, and
  `ProjectRoleConfigs` merges project partials onto global roles,
  `projects[].roles.coordinator.mergeWatch.maxRepairs` (and the
  `conflictPolicy` alias) become effective per-project overrides for free.
  Rejecting that path would mean special-casing one merge-watch field when its
  siblings are already project-overridable, which is the less consistent move.
  This issue therefore *supports* per-project override as a consequence of the
  existing mechanism (covered by a config test asserting a project value
  overrides the global default) and does not add a separate rejection gate.
- Surfacing the counter in the dashboard (separate follow-up).
- Applying the bound to `ActionRedCI` (different failure class).
