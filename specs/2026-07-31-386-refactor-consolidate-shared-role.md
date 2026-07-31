# Refactor: consolidate shared role types across packages

Issue: [MumuTW/looper#386](https://github.com/MumuTW/looper/issues/386)
Base branch: `main`
Source: Code Tomography Scan RC-003 / MN-001
Severity: Low — maintenance friction, no correctness risk

## Problem

Each of the four loop runners (fixer, reviewer, worker, planner) independently
redefines the same failure-kind vocabulary, and three of them duplicate the same
step-traversal helpers. This is a double (arguably triple) ledger: a constant
added or renamed in one role must be mirrored in the other three, with no
compiler-enforced link between them.

### The failure-kind ledger

Every role defines an identical `type QueueFailureKind string` with identical
constants:

| Role | Location |
|------|----------|
| fixer | `internal/fixer/runner.go:62-68` |
| reviewer | `internal/reviewer/runner.go:91-98` |
| worker | `internal/worker/runner.go:49-52, 80` |
| planner | `internal/planner/runner.go:52-59` |

All four encode the same four strings: `retryable_transient`,
`retryable_after_resume`, `non_retryable`, `manual_intervention`.

These strings **already exist as the authority** in two shared packages:

- `internal/loops/failureclass` defines `type Kind string` with the exact same
  four values (`failureclass.RetryableTransient`, `RetryableAfterResume`,
  `NonRetryable`, `ManualIntervention`) plus the `Classify` function that
  produces them.
- `internal/loops/policy` defines two of the four as plain string constants
  (`FailureKindRetryableAfterResume`, `FailureKindManualIntervention`).

So the per-role `QueueFailureKind` is a 1:1 mirror of `failureclass.Kind`. Each
role even carries an identical conversion function that proves the two are the
same vocabulary:

- `fixerFailureKind` (`internal/fixer/runner.go:6483`)
- `reviewerFailureKind` (`internal/reviewer/runner.go:4656`)
- `workerFailureKind` (`internal/worker/runner.go:3421`)
- `plannerFailureKind` (`internal/planner/runner.go:1952`)

All four are byte-for-byte equivalent except for the prefix. The conversion
exists only because the roles declared a distinct type instead of reusing the
shared one.

A fourth copy of the same shape sits in each role as the failure carrier:

```go
type loopError struct {
    message string
    kind    QueueFailureKind
}
```

defined identically in fixer (`runner.go:916`), reviewer (`runner.go:606`),
worker (`runner.go:714`), and planner (`runner.go:478`).

### The step-traversal ledger (secondary)

`fixer/workflow`, `reviewer/workflow`, and `worker/workflow` each define a
`type Step string`, a `sequence` slice, and near-identical `Sequence` / `From` /
`Next` / `Previous` / `Parse` helpers. The planner carries the same helpers
inline (`stepsFrom`, `nextPlannerStep`, `asPlannerStep` at
`internal/planner/runner.go:1967-1989`) without a workflow package.

The `Step` **constants** are genuinely role-specific (each pipeline has
different stage names), but the **traversal helpers** over them are duplicated.

### Why this matters

A new failure kind (or a rename) today requires coordinated edits across four
runner files plus the two shared packages, with nothing but reviewer attention
keeping the strings in sync. The conversion functions exist solely to bridge two
types that encode the same vocabulary — they are a symptom of the duplication,
not a solution to it.

## Goals

1. **One authority for the failure-kind vocabulary.** `failureclass.Kind`
   becomes the single source for the role runners; the per-role
   `QueueFailureKind` is removed. `internal/loops/policy`'s two string constants
   remain a separate string-typed spelling — `policy` is an intentional
   stdlib-only leaf so `reviewer/workflow` can depend on it without pulling in
   the infra stack `failureclass` carries — but are pinned to the shared values
   by a drift-detection test (Step 5) rather than by import.
2. **Delete the four `xxxFailureKind` conversion functions.** They exist only
   because of the type split; deleting the type deletes them. Their unknown-kind
   → `non_retryable` fallback is preserved by `failureclass.Normalize` (Step 2).
3. **No behavior change.** This is a refactor: the persisted string values, the
   `Classify` outcomes, the retry/hold decisions, and the resume-policy
   derivation must all remain byte-identical.
4. **Keep role-specific step types role-specific.** The `Step` types stay in
   their `workflow` packages; only the duplicated traversal helpers are
   considered for sharing, and only if it can be done without a generic
   abstraction that costs more than it saves.

## Non-goals

- Merging the four `workflow.Step` types into a union type. The pipelines have
  different stages; a shared `Step` would erase compile-time safety and create a
  fake common concept. The issue's "common step types" is satisfied by leaving
  them as the thin wrappers they already are.
- Touching `failureclass.Classify` logic, boundary mapping, or resume-policy
  semantics. Those are behavior; this refactor is about types and constants.
- Extracting `loopError` into a shared package. It is a candidate (see
  Alternatives) but is out of scope for this change to keep the diff focused on
  the ledger the issue names.

## Approach

### Step 1 — Delete `QueueFailureKind`; use `failureclass.Kind` directly

In each runner, delete the local type declaration and re-export the shared
constants:

```go
// deleted: type QueueFailureKind string and its four typed constants

const (
    FailureRetryableTransient   = failureclass.RetryableTransient
    FailureRetryableAfterResume = failureclass.RetryableAfterResume
    FailureNonRetryable         = failureclass.NonRetryable
    FailureManualIntervention   = failureclass.ManualIntervention
)
```

Then replace every `QueueFailureKind` reference with `failureclass.Kind`. The
local `FailureRetryable*` constants stay as untyped re-exports of the shared
values, so the ~290 production call sites that spell those names keep compiling
unchanged. Only the type name itself is renamed: a repo-wide search finds 60
`QueueFailureKind` references (58 across the four runners, 1 in
`internal/runtime/scheduler.go`, 1 in `internal/fixer/runner_repair_outcome_test.go`),
each a mechanical `s/QueueFailureKind/failureclass.Kind/`. The scheduler field
`workerRunCompletedNotificationInput.FailureKind` becomes `failureclass.Kind`;
`loopError.kind` becomes `failureclass.Kind`; the `fixerRepairTaskOutcome` and
`parseFixerBlockedFailureKind` return types become `failureclass.Kind`.

Deleting the type name outright (not aliasing it) is deliberate. The original
~150+ estimate conflated the type-name references with the constant-name
references; the actual type-name edit is the 60 sites above, and the constant
re-exports are independent of the type name and do not need to change. This
removes a compatibility layer rather than retaining one, per the repo's "prefer
deletion over another layer" guideline, and is what makes the conversion
functions deletable in the next step.

### Step 2 — Delete the `xxxFailureKind` conversion functions; preserve the unknown-kind fallback

With `QueueFailureKind` gone, `fixerFailureKind`, `reviewerFailureKind`,
`workerFailureKind`, and `plannerFailureKind` would be identity functions for
the four known kinds — but each currently maps every *unrecognized*
`failureclass.Kind` to `FailureNonRetryable` via its `default` branch. Deleting
the switch and assigning `kind: d.Kind` directly would drop that fallback: a
future new `failureclass.Kind` value would reach `isQueueRetryEligible` as an
unknown value instead of being normalized to `non_retryable`, changing retry
behavior before all retry predicates are updated. That violates Goal 3 (no
behavior change).

Preserve the fallback explicitly. Add
`failureclass.Normalize(kind Kind) Kind` to `internal/loops/failureclass`,
returning each known kind unchanged and any unrecognized kind as `NonRetryable`,
and replace the four conversion functions with calls to it (e.g.
`kind: failureclass.Normalize(d.Kind)` instead of `kind: fixerFailureKind(d.Kind)`).
This keeps the unknown-kind → `non_retryable` normalization as a named,
testable invariant rather than an implicit side effect of the type split. Add a
focused test in `internal/loops/failureclass` asserting that an unknown `Kind`
normalizes to `NonRetryable` and that the four known kinds pass through.

### Step 3 — Audit the cross-package typed surface

The one external consumer of a role-specific `QueueFailureKind` is
`internal/runtime/scheduler.go`:

```go
type workerRunCompletedNotificationInput struct {
    ...
    FailureKind       worker.QueueFailureKind
    ...
}
```

With `worker.QueueFailureKind` deleted, this field becomes `failureclass.Kind`
(one of the 60 type-name references). The value set is identical, so the
scheduler's behavior is unchanged; confirm with `go build ./...` and
`go vet ./...`. Callers that pass a `failureclass.Kind` literal now compile
without conversion; that is a side-benefit, not a goal.

### Step 4 — Leave the step types alone; record the decision

The `workflow.Step` types and their traversal helpers are intentionally not
touched. The `Step` constants differ per role and must not be unified. The
traversal helpers (`Sequence`/`From`/`Next`/`Previous`/`Parse`) are duplicated,
but generalizing them (e.g. a generic `StepPipeline[T ~string]`) would add a
generic abstraction to save ~20 lines per package — a net negative on a
Low-severity path that has already been patched once. This is recorded here so
the decision is explicit, not silent.

### Step 5 — Pin `policy`'s string constants to `failureclass` by test

`internal/loops/policy` re-declares two of the four kind strings as plain
`string` constants (`FailureKindRetryableAfterResume`, `FailureKindManualIntervention`).
They cannot be defined from `failureclass`'s constants by import: `policy` is an
intentional stdlib-only leaf (its package doc states this) so that
`internal/reviewer/workflow` can depend on it without pulling in the
`internal/infra/github` stack that `failureclass` transitively imports. Instead,
add a drift-detection test in `internal/loops` — the umbrella package already
imports `policy` in production, and the test adds `failureclass` as a test-only
import (so no production dependents of `policy` pull in the infra stack) —
asserting:

```go
if policy.FailureKindRetryableAfterResume != string(failureclass.RetryableAfterResume) {
    t.Fatal("policy.FailureKindRetryableAfterResume drifted from failureclass.RetryableAfterResume")
}
if policy.FailureKindManualIntervention != string(failureclass.ManualIntervention) {
    t.Fatal("policy.FailureKindManualIntervention drifted from failureclass.ManualIntervention")
}
```

This catches the silent drift the single-authority goal is about (a rename of
either shared value leaving `NormalizeResumePolicy` matching the old spelling)
without breaking `policy`'s leaf property or changing resume behavior. The
constants stay `string`; `NormalizeResumePolicy` keeps its current signatures and
branches.

## Alternatives considered

- **Create `internal/roles/` as the issue suggests.** Rejected. A new package
  would duplicate the vocabulary that `internal/loops/failureclass` already
  owns, moving the double ledger rather than deleting it. The authority already
  exists; the refactor should point at it, not stand up a parallel one. This
  aligns with the repo's "prefer deletion over another layer" guideline.
- **Alias `QueueFailureKind = failureclass.Kind` instead of deleting it.**
  Considered, rejected. The alias would keep the type name alive as a
  compatibility layer, but the actual cost of deleting it is the 60 type-name
  references (not the ~150+ originally estimated, which conflated the type name
  with the ~290 `FailureRetryable*` constant call sites that stay unchanged
  either way). Since the deletion is a small mechanical edit and is the cleanest
  end state, retaining the alias would preserve a layer for no benefit, against
  the repo's "prefer deletion over another layer" guideline.
- **Share `loopError` across roles.** The four `loopError` structs are
  identical in shape. Extracting a shared `roles.LoopError` is plausible, but
  each role attaches role-specific skip-error siblings (`holdSkipError`,
  `labelMismatchSkipError`, `runtimeSkipKind`) and helper methods around it.
  Pulling the struct out without those neighbors would leave a half-extraction;
  pulling them all out expands scope well past the failure-kind ledger the issue
  names. Deferred.
- **Generalize the step traversal helpers.** See Step 4 — rejected as net
  negative.

## Trade-off (per design guidelines)

> Delete this six months from now — what breaks?

`QueueFailureKind` is already gone, so there is no alias to delete later. The
local `FailureRetryable*` constants are untyped re-exports of the shared values;
deleting them later (the further-cleaned end state) only breaks callers that
spell those names literally, which is a mechanical rename. The conversion
functions, once deleted, cannot silently drift back — there is no second type to
reconcile, and `failureclass.Normalize` carries the unknown-kind fallback
explicitly.

> What does it still not catch?

`internal/loops/policy`'s two bare string constants
(`FailureKindRetryableAfterResume`, `FailureKindManualIntervention`) remain a
separate spelling of the same vocabulary. They are plain `string`, not
`failureclass.Kind`, and `policy` cannot import `failureclass` to share the
constants directly: `policy` is an intentional stdlib-only leaf (its package
doc states this) so that `internal/reviewer/workflow` can depend on it without
pulling in the `internal/infra/github` stack that `failureclass` transitively
imports. Importing `failureclass` would break that leaf property for every
workflow package. Instead, Step 5 adds a drift-detection test in `internal/loops`
(which already imports `policy` in production and adds `failureclass` as a
test-only import) asserting
`policy.FailureKindRetryableAfterResume == string(failureclass.RetryableAfterResume)`
and the same for `ManualIntervention`. A rename of either shared value that
leaves `NormalizeResumePolicy` matching the old spelling now fails the test
rather than drifting silently. Resume-policy behavior is unchanged: the
constants stay `string`, and `NormalizeResumePolicy` keeps its current
signatures and branches.

> What is the authority for failure-kind values, and why is it not the agent's
> own structured output?

There are two authorities, one per production path, and neither is the agent's
raw structured output. For errors raised inside a runner, `failureclass.Classify`
is the authority: it maps the error and its boundary to a `Kind`. For the fixer's
agent-reported *blocked* outcome, `parseFixerBlockedFailureKind` is the
authority: its allowlist (`manual_intervention`, `retryable_after_resume`,
`retryable_transient`; everything else rejected as a contract failure) is what
bounds the agent's `failure_kind` string — `failureclass.Classify` and the
`xxxFailureKind` conversions are never called on that path. After this refactor
the agent string is parsed straight into `failureclass.Kind` (the parser's
return type becomes `failureclass.Kind`), removing the re-wrap while keeping the
allowlist as the bound. Infra signals remain for drift detection, not authority.

## Impact

**Files changed (production):**
- `internal/fixer/runner.go` — delete `QueueFailureKind` + delete `fixerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/reviewer/runner.go` — delete `QueueFailureKind` + delete `reviewerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/worker/runner.go` — delete `QueueFailureKind` + delete `workerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/planner/runner.go` — delete `QueueFailureKind` + delete `plannerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/runtime/scheduler.go` — `workerRunCompletedNotificationInput.FailureKind` becomes `failureclass.Kind` (the one `QueueFailureKind` reference here).
- `internal/loops/failureclass/failureclass.go` — add `Normalize(kind Kind) Kind` (the authority for the unknown-kind fallback); `Classify` logic unchanged.

**Files unchanged but verified:**
- `internal/loops/policy/policy.go` — untouched. Its two string constants stay
  `string` and `NormalizeResumePolicy` keeps its current branches; drift is
  caught by the new test in `internal/loops` (Step 5), not by an import that
  would break `policy`'s stdlib-only leaf property.

**Test files:** Call sites in `*_test.go` that reference `FailureRetryable*`
constants continue to compile unchanged (the constants are re-exported). The one
test that spells `QueueFailureKind` literally
(`internal/fixer/runner_repair_outcome_test.go`) is updated to
`failureclass.Kind`. The existing `failure_classification_test.go` files are the
partial regression net: only three roles have one (`internal/fixer`,
`internal/worker`, `internal/planner` — `internal/reviewer` has none), and none
of the three asserts all four kinds; they cover `retryable_transient` and
`non_retryable` via `classifyFailure` and do not exercise
`retryable_after_resume` or `manual_intervention`. The implementation therefore
adds the missing focused coverage (see Validation step 6) rather than relying on
a four-kind net that does not yet exist.

**No persisted-state change:** The string values written to the queue/run
records are identical before and after. No migration, no schema touch.

## Risks

1. **Type-name rename.** `QueueFailureKind` is replaced by `failureclass.Kind`
   at 60 sites. If any code relied on the two being *distinct* types (e.g. a type
   switch distinguishing them, or an interface implemented by only one), it
   would break. Audit: the only typed surfaces are the scheduler field and the
   `loopError.kind` field; both are internal and expect the same four values.
   Risk: negligible, caught by `go build`.
2. **Constant re-export typing.** The local `FailureRetryableTransient` becomes
   an untyped constant aliasing `failureclass.RetryableTransient`. If any code
   takes the kind by reference or uses `reflect` on the constant's type,
   behavior could differ. Audit: no reflective use found. Risk: low.
3. **Scope creep into step types.** The temptation to also merge `workflow.Step`
   is real and would be a regression (loss of per-role type safety). The spec
   explicitly excludes it; a reviewer should reject any diff that touches `Step`.
4. **Second-fix signal.** This area (`failureclass`/role failure kinds) has not
   received a recent `fix:`; this is the first consolidation, not a patch on a
   patch. No revert signal.

## Validation

Per `AGENTS.md`, the root commands are the source of truth:

1. `go build ./...` — confirms the deleted type and conversion functions compile
   across all packages, including the scheduler's
   `workerRunCompletedNotificationInput.FailureKind` now typed as
   `failureclass.Kind`.
2. `go vet ./...` — confirms no shadowing or unused-symbol issues from the
   deleted functions.
3. `go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 -tests=false -checks='U1000,SA1006,SA4004,SA4006' ./...` — confirms no newly-unused constants
   (the re-exported `FailureRetryable*` must remain referenced).
4. `go test ./...` — the full suite is green.
5. `gofmt -l .` — no formatting drift.
6. **Four-kind regression coverage.** The existing
   `internal/{fixer,worker,planner}/failure_classification_test.go` files cover
   only `retryable_transient` and `non_retryable` via `classifyFailure`, and
   `internal/reviewer` has no such file. The implementation adds focused
   coverage so that each of the four `failureclass.Kind` values is asserted by
   at least one role's classification test (including a new
   `internal/reviewer/failure_classification_test.go`), plus a test for
   `failureclass.Normalize` covering the four known kinds and an unknown kind.
   This is the regression net the refactor relies on; it must exist before the
   conversion functions are deleted.
7. **Policy drift detection (Step 5).** A test in `internal/loops` asserts
   `policy.FailureKindRetryableAfterResume == string(failureclass.RetryableAfterResume)`
   and
   `policy.FailureKindManualIntervention == string(failureclass.ManualIntervention)`,
   so a rename of either shared value that leaves `NormalizeResumePolicy`
   matching the old spelling fails the suite.

**Definition of done:** `QueueFailureKind` is gone from all four runners (the 60
type-name references replaced by `failureclass.Kind`), the four `xxxFailureKind`
functions are gone and `failureclass.Normalize` preserves their unknown-kind →
`non_retryable` fallback, the four-kind regression coverage above exists, the
policy drift-detection test exists, the full `go test ./...` suite is green, and
the diff contains no changes to `workflow.Step` types, `failureclass.Classify`
logic, or `NormalizeResumePolicy` behavior.
