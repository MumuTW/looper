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
   becomes the single source; the per-role `QueueFailureKind` either aliases it
   or is removed.
2. **Delete the four `xxxFailureKind` conversion functions.** They exist only
   because of the type split; collapsing the type deletes them.
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

### Step 1 — Collapse `QueueFailureKind` onto `failureclass.Kind`

In each runner, replace the local declaration:

```go
type QueueFailureKind string
const (
    FailureRetryableTransient   QueueFailureKind = "retryable_transient"
    FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
    FailureNonRetryable         QueueFailureKind = "non_retryable"
    FailureManualIntervention   QueueFailureKind = "manual_intervention"
)
```

with a type alias and re-exports of the shared constants:

```go
type QueueFailureKind = failureclass.Kind

const (
    FailureRetryableTransient   = failureclass.RetryableTransient
    FailureRetryableAfterResume = failureclass.RetryableAfterResume
    FailureNonRetryable         = failureclass.NonRetryable
    FailureManualIntervention   = failureclass.ManualIntervention
)
```

The type alias keeps `QueueFailureKind` usable as an exported name in each
role's API surface (the scheduler depends on `worker.QueueFailureKind`, see
Impact) while making it the *same type* as `failureclass.Kind`. The local
constants stay as convenience aliases so the large number of call sites
(`FailureRetryableTransient` appears ~95 times in the fixer runner alone) do not
need to be rewritten in this change.

A type alias (not a distinct type) is deliberate: it is what makes the
conversion functions deletable in the next step.

### Step 2 — Delete the `xxxFailureKind` conversion functions

With `QueueFailureKind = failureclass.Kind`, `fixerFailureKind`,
`reviewerFailureKind`, `workerFailureKind`, and `plannerFailureKind` become
identity functions. Delete all four and replace their call sites with the
`failureclass.Kind` value directly (e.g. `kind: d.Kind` instead of
`kind: fixerFailureKind(d.Kind)`).

### Step 3 — Audit the cross-package typed surface

The one external consumer of a role-specific `QueueFailureKind` is
`internal/runtime/scheduler.go:241`:

```go
type workerRunCompletedNotificationInput struct {
    ...
    FailureKind       worker.QueueFailureKind
    ...
}
```

Because `worker.QueueFailureKind` is now an alias for `failureclass.Kind`, this
field's type is unchanged from the compiler's view; no edit is required. Confirm
this with `go build ./...` and `go vet ./...`. If any caller ever needed to pass
a `failureclass.Kind` literal, it now compiles without conversion; that is a
side-benefit, not a goal.

### Step 4 — Leave the step types alone; record the decision

The `workflow.Step` types and their traversal helpers are intentionally not
touched. The `Step` constants differ per role and must not be unified. The
traversal helpers (`Sequence`/`From`/`Next`/`Previous`/`Parse`) are duplicated,
but generalizing them (e.g. a generic `StepPipeline[T ~string]`) would add a
generic abstraction to save ~20 lines per package — a net negative on a
Low-severity path that has already been patched once. This is recorded here so
the decision is explicit, not silent.

## Alternatives considered

- **Create `internal/roles/` as the issue suggests.** Rejected. A new package
  would duplicate the vocabulary that `internal/loops/failureclass` already
  owns, moving the double ledger rather than deleting it. The authority already
  exists; the refactor should point at it, not stand up a parallel one. This
  aligns with the repo's "prefer deletion over another layer" guideline.
- **Remove `QueueFailureKind` entirely and use `failureclass.Kind` at every call
  site.** Considered, deferred. It would be the cleanest end state but forces a
  large mechanical rename (~150+ call sites across four runners and their
  tests) for no behavioral gain. The type alias achieves the single-authority
  goal with a small, reviewable diff; the full rename can follow later if
  desired.
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

Nothing the alias doesn't already preserve. The alias makes
`QueueFailureKind` and `failureclass.Kind` the same type; deleting the alias
later (the full-rename end state) only breaks callers that spell
`QueueFailureKind` literally, which is a mechanical rename. The conversion
functions, once deleted, cannot silently drift back — there is no second type to
reconcile.

> What does it still not catch?

`internal/loops/policy`'s two bare string constants
(`FailureKindRetryableAfterResume`, `FailureKindManualIntervention`) remain a
separate spelling of the same vocabulary. They are plain `string`, not
`failureclass.Kind`, so the policy package can still drift from `failureclass`
by string typo. This refactor does not touch policy (it would change behavior
surfaces in `NormalizeResumePolicy`); it is noted as the remaining ledger, not
fixed here.

> What is the authority for failure-kind values, and why is it not the agent's
> own structured output?

`failureclass.Classify` is the authority; the agent's structured output is an
input to it (the fixer's `parseFixerBlockedFailureKind` parses an agent-supplied
`failure_kind` string, then `fixerFailureKind` currently re-wraps it). After
this refactor the agent string is parsed straight into `failureclass.Kind`,
removing the re-wrap. Infra signals remain for drift detection, not authority.

## Impact

**Files changed (production):**
- `internal/fixer/runner.go` — alias + delete `fixerFailureKind` + call-site edits.
- `internal/reviewer/runner.go` — alias + delete `reviewerFailureKind` + call-site edits.
- `internal/worker/runner.go` — alias + delete `workerFailureKind` + call-site edits.
- `internal/planner/runner.go` — alias + delete `plannerFailureKind` + call-site edits.

**Files unchanged but verified:**
- `internal/runtime/scheduler.go` — uses `worker.QueueFailureKind`, which
  remains valid as an alias. No edit; confirm via build.
- `internal/loops/failureclass/failureclass.go` — the authority; untouched.
- `internal/loops/policy/policy.go` — untouched (see remaining ledger above).

**Test files:** Call sites in `*_test.go` that reference `FailureRetryable*`
constants continue to compile unchanged (the constants are re-exported). Tests
that referenced `fixerFailureKind`/etc. directly, if any, are updated. No new
test states are added — this is a type-alias refactor with no behavior change,
so the existing `failure_classification_test.go` files in each role are the
regression net.

**No persisted-state change:** The string values written to the queue/run
records are identical before and after. No migration, no schema touch.

## Risks

1. **Alias vs. distinct type subtlety.** A type alias makes `QueueFailureKind`
   assignable to `failureclass.Kind` and vice versa. If any code relied on the
   two being *distinct* (e.g. a type switch distinguishing them, or an interface
   implemented by only one), it would break. Audit: the only typed surface is
   the scheduler field and the `loopError.kind` field; both are internal and
   expect the same four values. Risk: negligible, caught by `go build`.
2. **Constant re-export naming.** The local `FailureRetryableTransient` becomes
   an untyped constant aliasing `failureclass.RetryableTransient`. If any code
   takes `QueueFailureKind` by reference or uses `reflect` on the constant's
   type, behavior could differ. Audit: no reflective use found. Risk: low.
3. **Scope creep into step types.** The temptation to also merge `workflow.Step`
   is real and would be a regression (loss of per-role type safety). The spec
   explicitly excludes it; a reviewer should reject any diff that touches `Step`.
4. **Second-fix signal.** This area (`failureclass`/role failure kinds) has not
   received a recent `fix:`; this is the first consolidation, not a patch on a
   patch. No revert signal.

## Validation

Per `AGENTS.md`, the root commands are the source of truth:

1. `go build ./...` — confirms the alias and the deleted conversion functions
   compile across all packages, including the scheduler's use of
   `worker.QueueFailureKind`.
2. `go vet ./...` — confirms no shadowing or unused-symbol issues from the
   deleted functions.
3. `go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 -tests=false -checks='U1000,SA1006,SA4004,SA4006' ./...` — confirms no newly-unused constants
   (the re-exported `FailureRetryable*` must remain referenced).
4. `go test ./...` — the existing `internal/{fixer,reviewer,worker,planner}/failure_classification_test.go`
   files assert the four kinds classify correctly; they are the regression net
   and must pass unchanged.
5. `gofmt -l .` — no formatting drift.

**Definition of done:** all four runners compile with `QueueFailureKind` as an
alias of `failureclass.Kind`, the four `xxxFailureKind` functions are gone, the
full `go test ./...` suite is green, and the diff contains no changes to
`workflow.Step` types, `failureclass.Classify`, or `internal/loops/policy`.
