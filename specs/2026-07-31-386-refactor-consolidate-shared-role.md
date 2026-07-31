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

A fourth copy of the same shape sits in each role as the failure carrier. Three
of the four are identical:

```go
type loopError struct {
    message string
    kind    QueueFailureKind
}
```

defined in fixer (`runner.go:916`), worker (`runner.go:714`), and planner
(`runner.go:478`). The reviewer carrier (`runner.go:606`) is *not* identical: it
carries an additional `interrupted bool` field that participates in reviewer
control flow (it records whether the run was interrupted, and is read by the
reviewer's resume/skip logic), so its shape is:

```go
type loopError struct {
    message     string
    kind        QueueFailureKind
    interrupted bool
}
```

This shape divergence is why the `loopError` extraction trade-off below is not a
straightforward identical-shape merge; see Alternatives.

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

1. **One authority for the failure-kind vocabulary.** `policy.Kind` becomes the
   single source for the kind *type and values*; the per-role `QueueFailureKind`
   is removed. `internal/validation`'s three `FailureKind*` constants are
   deleted, `Policy.FailureKind` is typed `policy.Kind` (not `string`), and
   `PolicyFor` returns the shared constants directly with no `string()` cast:
   `validation` imports the stdlib-only `internal/loops/policy` leaf for the
   `Kind` type and constants — **not** the infrastructure-backed
   `internal/loops/failureclass` — so `go test ./internal/validation` does not
   compile `failureclass` and its `internal/infra/github`, `diffanchor`, and
   `outboundguard` dependency tree. This is the same focused-package coupling
   avoidance Step 6 applies to `internal/agent`: a generic validation-policy
   package must not depend on the infrastructure-backed classifier just to name
   a kind. `failureclass` does not import `validation`, so no cycle is created;
   and because `failureclass.Kind` is a type alias for `policy.Kind` (Step 5),
   the two production consumers (`worker.classifyValidationFailure` and
   `fixer/failurepolicy.ClassifyValidation`) delete their string→kind casts —
   `policy.FailureKind` is already the same type as the `failureclass.Kind`
   fields they assign into. The remaining package that spells the vocabulary as
   bare `string` constants — `internal/loops/policy` — becomes the *owner* of
   the kind type and string values rather than a second spelling pinned by a
   gate: `policy` is an intentional stdlib-only leaf (its package doc states
   this) so that `reviewer/workflow` and `internal/validation` can depend on it
   without pulling in the infra stack `failureclass` carries, and the dependency
   runs in the opposite direction — `failureclass` imports `policy` (no cycle:
   `policy` imports only the standard library) and aliases its `Kind` type and
   re-exports its constants at compile time (Step 5). `policy` gains the two
   kind constants it currently lacks (`FailureKindRetryableTransient`,
   `FailureKindNonRetryable`) so it owns all four string values, plus the typed
   `Kind` constants the runners consume via the `failureclass` alias, and the
   umbrella `internal/loops` package re-exports those new names alongside its
   existing ones to keep its "re-exports every name here" contract (Step 5);
   `failureclass.Kind` becomes `type Kind = policy.Kind` (a type alias, so
   `failureclass.Kind` and `policy.Kind` are the identical type) and
   `Classify`'s public API is unchanged, but each `failureclass` constant is now
   `const RetryableTransient = policy.RetryableTransient` (a re-export of the
   `policy.Kind`-typed constant), so a rename in `policy` propagates to
   `failureclass` and to every runner at compile time. This deletes the second
   spelling rather than retaining both ledgers plus a drift gate, per the repo's
   "prefer deletion over another layer" guideline. The fixer prompt's
   advertised literals (`internal/agent/prompt.go`) are no longer a separate
   spelling: `internal/agent` imports the stdlib-only `internal/loops/policy`
   leaf directly and derives the `failure_kind` tokens from
   `policy.RetryableTransient` and `policy.ManualIntervention` inside the builder
   (Step 6), without importing `failureclass` — `failureclass` imports
   `internal/infra/github`, which `internal/agent` does not currently reach, and
   coupling a generic prompt-construction package to the infrastructure-backed
   classifier would pull that stack into every build and test of
   `internal/agent`. `policy` is the same authority the parser's
   `FailureRetryable*` constants trace to (via the `failureclass.*` aliases), so a
   rename updates the prompt and the parser together at compile time. This
   deletes the second spelling rather than pinning it by import or by a carrier;
   no `FixerCompletionKinds` struct, fixer call-site wiring, or cross-component
   synchronization test is added — the derivation is the simpler move the repo's
   "prefer deletion over another layer" guideline requires before introducing a
   carrier to pass the values across packages (Step 6). A unit test in
   `internal/agent` exercises the builder and asserts the advertised bullet set
   equals exactly the kinds `parseFixerBlockedFailureKind` honors, for
   advertised-subset and field-to-bullet-pairing coverage of the builder's own
   correctness.
   This is not an exhaustive audit of
   every bare-string spelling of the vocabulary: additional production consumers
   and the persisted schema also spell the kinds as bare strings and are
   explicitly out of scope for this refactor — see the Trade-off "What does it
   still not catch" for the full inventory and the rationale for deferring them.
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
local `FailureRetryable*` constants are typed re-exports: because
`failureclass.RetryableTransient` etc. are declared as `Kind` (not untyped
string constants), the local constants inherit `failureclass.Kind`, so the ~290
production call sites that spell those names keep compiling unchanged. Only the
type name itself is renamed: a repo-wide search finds 60
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

In the same step, delete `internal/validation`'s three `FailureKind*`
constants, type `Policy.FailureKind` as `policy.Kind` (not `string`), and
have `PolicyFor` return the shared constants directly with no `string()` cast.
`validation` imports the stdlib-only `internal/loops/policy` leaf for the `Kind`
type and typed constants — **not** `internal/loops/failureclass` — so the
focused `go test ./internal/validation` build does not compile `failureclass`
and its `internal/infra/github`, `diffanchor`, and `outboundguard` dependency
tree. Today
`validation.FailureKindManualIntervention = loops.FailureKindManualIntervention`
and `validation.FailureKindRetryableAfterResume = loops.FailureKindRetryableAfterResume`
(transitively `policy`'s bare strings), while
`validation.FailureKindRetryableTransient = "retryable_transient"` is a bare
string with no shared-authority link. The call-site audit establishes that these
exported constants have no production consumer outside `validation.PolicyFor`,
which assigns them to `Policy.FailureKind` (declared `string`); from there the
value flows as a `string` field into the two production consumers, both of which
cast it straight back to a typed kind — `worker.classifyValidationFailure`
applies `QueueFailureKind(policy.FailureKind)` and
`fixer/failurepolicy.ClassifyValidation` applies
`failureclass.Kind(policy.FailureKind)`. Only same-package tests additionally
spell the constant names. Keeping `Policy.FailureKind` as `string` and inlining
`string(policy.*)` in `PolicyFor` would preserve an untyped boundary where
a typo or future unsupported value still compiles and can reach retry or
persistence logic, and would force both consumers to keep their string→kind
casts — an authority converted to a string only to be converted back. Typing the
field `policy.Kind` deletes that boundary: the constants are deleted,
`PolicyFor` returns the shared constants directly, and both consumers drop their
casts (the `failureclass.Kind` fields they assign into are the same type as
`policy.Kind` via the Step 5 alias). `Policy` is an in-memory struct consumed
only by those two call sites plus same-package tests (it is not persisted or
JSON-marshaled), so widening the field type breaks no persistence or
string-context consumer.

```go
type Policy struct {
    FailureKind  policy.Kind
    ResumePolicy string
}

func PolicyFor(category FailureCategory) Policy {
    switch category {
    case FailureContextCanceled:
        return Policy{FailureKind: policy.RetryableAfterResume, ResumePolicy: loops.ResumePolicyReplayStep}
    case FailureSupervisorTimeout, FailureInfrastructure:
        return Policy{FailureKind: policy.RetryableTransient, ResumePolicy: loops.ResumePolicyReplayStep}
    case FailureNonZeroExit:
        return Policy{FailureKind: policy.ManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention}
    default:
        return Policy{FailureKind: policy.ManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention}
    }
}
```

The two consumers delete their casts:
`worker.classifyValidationFailure` becomes `kind: policy.FailureKind` (was
`QueueFailureKind(policy.FailureKind)`, itself becoming
`failureclass.Kind(policy.FailureKind)` after Step 1 renames the type — a
redundant cast once the field is already `policy.Kind`, identical to
`failureclass.Kind` via the Step 5 alias), and
`fixer/failurepolicy.ClassifyValidation` becomes `Kind: policy.FailureKind` (was
`failureclass.Kind(policy.FailureKind)`). The two same-package tests that spell
the constant names are updated to compare against the shared values instead:
`internal/validation/validation_test.go`'s
`TestRunCommandsKeepsPolicyWordsDiagnosticForNonZeroExit` compares
`policy.FailureKind` to `FailureKindManualIntervention`, and
`TestPolicyForOperationalFailureCategory` table-tests all three; both are
rewritten to expect `policy.ManualIntervention`,
`policy.RetryableAfterResume`, and `policy.RetryableTransient`
respectively (typed comparisons, no `string()` cast), so the test expectations
stay pinned to the shared authority after the constants are gone. No other
production consumer references the constant names or reads `Policy.FailureKind`
as a string, so no other call site changes.

This deletes the validation ledger outright and types the boundary with the
shared authority rather than re-exporting it as typed-string constants or
pinning it with a test-only synchronization gate, per the repo's "prefer
deletion over another layer" and "name the authority before enforcing it"
guidelines: there is no caller to preserve and no string-context consumer, so
the untyped layer adds only the documented source-level incompatibility and two
redundant casts. Because `Policy.FailureKind` is now `policy.Kind` (aliased as
`failureclass.Kind`), a rename of any kind value propagates to `PolicyFor` and
to both consumers at compile time, and no validation drift test is needed.
`validation` imports only the stdlib-only `policy` leaf for the kind type — not
`failureclass` — so `go test ./internal/validation` does not pull the
GitHub-infrastructure dependency tree into the focused test build. This is the
same coupling avoidance Step 6 applies to `internal/agent`: a generic
validation-policy package must not depend on the infrastructure-backed
classifier just to name a kind. `failureclass` does not import `validation`, so
no cycle is created, and every production importer of `validation`
(`internal/fixer`, `internal/fixer/failurepolicy`, `internal/worker`) already
imports `failureclass`, so no importer gains a transitive dependency it did not
already carry.

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

The `Normalize` unit test and the four-kind runner tests do not, by themselves,
prove that the former `xxxFailureKind` call sites actually invoke `Normalize`.
The `loopError.kind` assignments split into three groups:

1. **Dynamic sources (must be `Normalize`-wrapped).** Seven sites receive a
   `failureclass.Kind` whose value is not bounded to the four known kinds at the
   assignment: four in the fixer (`runner.go:3378,3403,3650,6480`, taking
   `d.Kind` / `failure.Kind` from a `Decision` or validation failure) and one
   each in the reviewer, worker, and planner (`runner.go:4640/3403/1936`, taking
   `failureclass.Classify(...)` directly). These are the former `xxxFailureKind`
   call sites and become `kind: failureclass.Normalize(...)`.

2. **Known-safe carrier sources (also `Normalize`-wrapped, for a uniform
   gate).** Two carrier producers feed `loopError.kind` with a value that is
   *not* a known-constant literal and *not* a `loopError.kind` copy, yet is
   provably bounded to the four known kinds by its producer:
   - `worker.validationFailure.kind` (`internal/worker/runner.go:2811`) is
     assigned only from `policy.FailureKind` (after Step 1 a known
     `policy.Kind` from `validation.PolicyFor`, which returns only the four
     known constants) or from `FailureManualIntervention` /
     `FailureRetryableAfterResume` / `FailureRetryableTransient` constants. It is
     read into `loopError.kind` at four worker sites
     (`runner.go:1943,1958,2007,2038` as `kind: failure.kind`).
   - `fixerRepairTaskOutcome` (`internal/fixer/runner.go:1367`) returns the
     `blockedKind` that flows into `loopError.kind` at
     `internal/fixer/runner.go:3327` (`kind: blockedKind`); its kind result comes
     only from `parseFixerBlockedFailureKind`, which allowlists
     `FailureManualIntervention` / `FailureRetryableAfterResume` /
     `FailureRetryableTransient` (the empty-string return is paired with
     `blocked=false`, so it never reaches this `loopError`).

   Neither carrier can hold an unknown kind, so `Normalize` is a no-op for them.
   They are nevertheless wrapped in `failureclass.Normalize` at the call site
   (`kind: failureclass.Normalize(failure.kind)` and
   `kind: failureclass.Normalize(blockedKind)`) rather than special-cased as
   "safe carriers" in the gate. This keeps the gate's rule uniform — every
   `loopError.kind` assignment whose value is not a known-kind constant and not a
   `loopError.kind` copy must be `Normalize`-wrapped — so the gate does not
   maintain an allowlist of carrier struct/function names whose "only known
   kinds" property must be re-proven by hand when a producer changes. The wrap is
   defensive too: if a future edit makes either carrier hold a dynamic kind,
   `Normalize` catches it instead of letting an unknown kind reach
   `isQueueRetryEligible`. Per the repo's "name the authority before enforcing
   it" guideline, the authority for "this is a known kind" is `Normalize` itself,
   not an inferred allowlist of producer names.

3. **Statically safe shapes (no `Normalize` needed).** Every other
   `loopError.kind` assignment is a named `FailureRetryable*` constant (a known
   kind that needs no normalization — the accepted set is derived from the
   `policy.Kind`/`failureclass.Kind` authority, not enumerated in the gate, so a
   future fifth constant is accepted automatically; see Validation step 8), or a
   copy of an already-normalized kind —
   a `loopError.kind` field read (`kind: failure.kind` where `failure` is a
   `loopError` *local constructed in this function with a safe `kind``) or a
   local that is itself only ever assigned from those two
   shapes (e.g. `kind := FailureRetryableTransient; ...; kind: kind`). A
   `loopError.kind` read off a `loopError` parameter or a `loopError` returned
   from an untracked helper is *not* in this group: the receiver's `kind` is not
   proven safe within this function, so it is a dynamic source that must be
   `Normalize`-wrapped (see the receiver-origin rule in Validation step 8).
   Taking the field's address (`&failure.kind`) is also *not* safe: the gate
   forbids it because an address-taken `kind` can be mutated through the pointer
   without an assignment the origin tracker visits (see the address-of rule in
   Validation step 8).

If an implementation replaces any dynamic-source or carrier call with a direct
assignment (`kind: d.Kind` or `kind: failureclass.Classify(...)` without
`Normalize`), the `Normalize` unit test, the four-kind runner tests, staticcheck,
and the deleted-symbol search (Step 7) all still pass, while a future unknown
kind no longer falls back to `non_retryable`.

A line-regex check is not a sound gate for this: it only sees the text of the
`kind:` line, so storing the dynamic value in a local first
(`classified := failureclass.Classify(...)` then `kind: classified`) bypasses
`Normalize` while matching no dynamic-source pattern on the `kind:` line. A
regex that claims every bypass fails is claiming an invariant it cannot enforce.
Validation step 8 therefore uses a type-aware check with intra-procedural
origin tracking instead of a regex: for every `loopError.kind` assignment in the
four runners it traces the value expression back to its origin within the same
function and fails when a dynamic source (`failureclass.Classify(...)` or a
`.Kind` field read off a non-`loopError` struct such as `Decision`) reaches
`kind` without being wrapped in `failureclass.Normalize`. Selector ownership and
call resolution are proven with `go/types`, not syntax alone: a `.kind` read is
treated as a `loopError` copy only when `go/types` resolves its receiver type to
`loopError` *and* the receiver's own origin is traced to a safe `loopError`
construction within this function (a `loopError` parameter or a `loopError`
returned from an untracked helper is not a safe copy — see the receiver-origin
rule in Validation step 8), and a `failureclass.Normalize(...)` call is recognized only when
`go/types` resolves it to the imported `failureclass` package's `Normalize`, so a
shadowed `failureclass` identifier or an unrelated struct's lowercase `kind`
field cannot pass as safe. A bare local like `kind: classified` is resolved to
its assignments, so the indirect assignment is caught — the implementer must
write `kind: failureclass.Normalize(classified)`. A bare identifier that is a
function parameter (no local definition to trace) is **unsafe**: its value
comes from an untrusted call-site argument the intra-procedural check cannot
trace, so a helper returning `&loopError{kind: kind}` from a `Kind` parameter
must wrap the parameter in `Normalize` rather than passing it through. A local
with no reaching assignments is likewise **unsafe**: its uninitialized
zero-value `Kind` is an unknown kind, and the "every reaching assignment is
safe" rule would accept the empty assignment set vacuously — the same hole the
parameter rule closes — so the implementer must wrap it in `Normalize` too.
Known-constant references, `loopError.kind` copies whose receiver origin is
safe, and locals with at least one reaching assignment whose every reaching
assignment is one of those two safe shapes pass. The
carrier reads from group 2
(`validationFailure.kind`, `blockedKind`) are `Normalize`-wrapped per Step 2, so
they pass the gate via the `Normalize`-call rule — the gate does not recognize
the carrier producers by name, which is why they must be wrapped rather than
allowlisted. This makes the gate match the invariant it claims.

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

### Step 5 — Reverse the leaf dependency: `policy` owns the `Kind` type; `failureclass` aliases it

One package outside `failureclass` still spells part of the kind vocabulary as
bare `string` constants: `internal/loops/policy` re-declares two of the four
kind strings (`FailureKindRetryableAfterResume`, `FailureKindManualIntervention`)
as plain untyped `string` constants. The earlier rationale for leaving that as a
second spelling pinned by a drift test considered only one dependency direction
— making `policy` import the heavyweight `failureclass` (which would break
`policy`'s stdlib-only leaf property). The dependency runs in the opposite
direction with no cycle: `policy` imports only the standard library, so
`failureclass` can import `policy` and alias its `Kind` type and re-export its
constants at compile time. That deletes the second spelling and the
synchronization test entirely, rather than retaining both ledgers plus a new
gate that leaves correctness dependent on the full suite running — the simpler
deletion the repo's "prefer deletion over another layer" guideline requires
before adding a gate.

`policy` becomes the single owner of the kind *type and string values*. It gains
the two untyped `string` constants it currently lacks so it owns all four string
values, and it gains the `Kind` type plus four typed `Kind` constants derived
from those untyped strings. The untyped `string` constants stay so the existing
`policy` predicates (`NormalizeResumePolicy`, `IsHardHold`, etc.) keep their
`string` signatures with no function change (see below); the typed `Kind`
constants are the authority the runners consume via the `failureclass` alias and
that `internal/validation` consumes directly:

```go
package policy

type Kind string

const (
	FailureKindRetryableTransient   = "retryable_transient"
	FailureKindRetryableAfterResume = "retryable_after_resume"
	FailureKindNonRetryable         = "non_retryable"
	FailureKindManualIntervention   = "manual_intervention"
	// ... existing ResumePolicy* constants unchanged ...
)

const (
	RetryableTransient   Kind = FailureKindRetryableTransient
	RetryableAfterResume Kind = FailureKindRetryableAfterResume
	NonRetryable         Kind = FailureKindNonRetryable
	ManualIntervention   Kind = FailureKindManualIntervention
)
```

The typed constants derive from the untyped ones in the same package, so there
is one owner and one derivation direction — not a second ledger. The untyped
constants exist only to feed the `string`-parameter predicates; the typed
constants are the single `Kind` authority.

The umbrella `internal/loops` package's documented compatibility contract
(`internal/loops/policy.go:7`: "internal/loops re-exports every name here")
covers every exported name in the leaf, so the two new untyped constants, the
`Kind` type, and the four typed constants must be re-exported there too, not
added to the leaf alone. `internal/loops/policy.go` gains the matching untyped
aliases alongside the existing two, plus a `Kind` type alias and the four typed
constant re-exports:

```go
package loops

import "github.com/MumuTW/looper/internal/loops/policy"

type Kind = policy.Kind

const (
	FailureKindRetryableTransient   = policy.FailureKindRetryableTransient
	FailureKindRetryableAfterResume = policy.FailureKindRetryableAfterResume
	FailureKindNonRetryable         = policy.FailureKindNonRetryable
	FailureKindManualIntervention   = policy.FailureKindManualIntervention
	// ... existing ResumePolicy* aliases unchanged ...
)

const (
	RetryableTransient   = policy.RetryableTransient
	RetryableAfterResume = policy.RetryableAfterResume
	NonRetryable         = policy.NonRetryable
	ManualIntervention   = policy.ManualIntervention
)
```

Without these aliases, callers following the documented `loops.*` umbrella API
cannot reach `loops.FailureKindRetryableTransient`, `loops.Kind`, or
`loops.RetryableTransient` — the leaf gains names the umbrella promises to
expose, breaking the contract the package doc asserts. Adding them is part of
this step, not a follow-up.

`failureclass` imports `policy` and aliases the `Kind` type and re-exports the
typed constants, so `failureclass.Kind` and `failureclass.RetryableTransient`
etc. remain available to the runners without the runners (or
`internal/validation`) depending on `failureclass` for the *type*:

```go
package failureclass

import "github.com/MumuTW/looper/internal/loops/policy"

type Kind = policy.Kind

const (
	RetryableTransient   = policy.RetryableTransient
	RetryableAfterResume = policy.RetryableAfterResume
	NonRetryable         = policy.NonRetryable
	ManualIntervention   = policy.ManualIntervention
)
```

`Classify`'s public API is unchanged (it still returns `Kind`, now `policy.Kind`
via the alias), and `failureclass.Kind` is `type Kind = policy.Kind` (a type
alias, so `failureclass.Kind` and `policy.Kind` are the identical type):
`failureclass.Classify` and `failureclass.Kind` themselves keep compiling
unchanged. The surfaces that adopt `failureclass.Kind` — the runners, the
scheduler field, `loopError.kind`, and `parseFixerBlockedFailureKind`'s return
type — retain their semantic values but do not keep compiling unchanged: their
declarations change to `failureclass.Kind` as the planned type edits of Steps 1
and 3, and Step 5 adds no further edit to them. `Policy.FailureKind` is typed
`policy.Kind` (Step 1), which is the same type as `failureclass.Kind` via the
alias, so the two consumers assign `policy.FailureKind` into `failureclass.Kind`
fields with no cast. A rename of any `policy` constant propagates to
`failureclass`, to the umbrella, and to every consumer at compile time, so no
drift-detection test is added or needed. `policy`'s leaf property is preserved:
it still imports only the standard library, so `reviewer/workflow` and
`internal/validation` depending on `policy` still pull in no infra. Adding
`policy` to `failureclass`'s import graph adds only a stdlib-only package to
`failureclass`'s existing `internal/infra/github` closure, and every runner
already imports `policy` transitively via `internal/loops`, so no importer of
`failureclass` gains a transitive dependency it did not already carry.

`NormalizeResumePolicy` and the other `policy` predicates keep their current
`string` signatures and branches: the untyped `FailureKind*` constants stay
untyped `string`, so they remain assignable to the `string` parameters those
functions take. The typed `Kind` constants are a separate declaration set and do
not affect the predicates. No `policy` function changes.

(`internal/validation` also re-declared the same vocabulary as bare `string`
constants, but Step 1 deletes those constants, types `Policy.FailureKind` as
`policy.Kind` (importing the stdlib-only `policy` leaf, not the infra-backed
`failureclass`), and has `PolicyFor` return the shared constants directly,
deleting that ledger outright; see Step 1 for the no-cycle and no-infra-coupling
justification. No validation drift test is added or needed.)

### Step 6 — Derive the fixer prompt's advertised literals from the `policy` leaf

`internal/agent/prompt.go:55-57` (`AppendFixerCompletionInstruction`) embeds
`retryable_transient` and `manual_intervention` as string literals in the prompt
that tells the fixer agent which `failure_kind` values it may report. That prompt
is the advertising layer for `parseFixerBlockedFailureKind`: the parser bounds
what the agent returns, but the prompt tells the agent what to return. The
literals are not derived from the shared vocabulary, so a rename of
`policy.RetryableTransient` or `policy.ManualIntervention` would make the parser
(which reads the `FailureRetryable*` constants, themselves re-exports of the
`failureclass.*` aliases of the `policy.*` constants) follow the new value while
the prompt keeps advertising the old one — valid blocked outcomes become
contract failures. The existing `internal/agent/prompt_test.go` asserts the
literals too, not the shared constants, so it would not catch the divergence
either.

`internal/agent` must not import `failureclass` to derive these tokens:
`failureclass` imports `internal/infra/github` directly, and `internal/agent`
does not currently reach `internal/infra/github` or the rest of `failureclass`'
transitive closure (`internal/diffanchor`, `internal/outboundguard`); coupling a
generic prompt-construction package to the infrastructure-backed classifier
would pull the GitHub infrastructure stack into every build and test of
`internal/agent`. Step 5's reverse-dependency derivation makes this constraint
cheap to satisfy without a carrier: `internal/loops/policy` is a stdlib-only leaf
that now owns the `Kind` type and all four typed `Kind` constants, so
`internal/agent` imports `policy` directly and derives the two advertised values
from `policy.RetryableTransient` and `policy.ManualIntervention` inside the
builder. `policy` imports only the standard library, so this adds no
infrastructure edge to `internal/agent`'s build or test closure — the same
property that lets `internal/validation` and `reviewer/workflow` depend on
`policy` without pulling in the infra stack. The builder's signature is
unchanged (`AppendFixerCompletionInstruction(prompt string) string`); the two
values are read from the imported `policy` constants at construction time rather
than received from a caller, so no carrier struct, call-site wiring, or
cross-package value-passing machinery is introduced:

```go
package agent

import (
	"strconv"
	"strings"

	"github.com/MumuTW/looper/internal/loops/policy"
)
```

The prompt is built with `strings.Join`, so the advertised tokens are
constructed from `string(policy.RetryableTransient)` and
`string(policy.ManualIntervention)` rather than spelled as independent literals.
A rename of either `policy` constant updates the prompt and the parser at
compile time — both trace to the same `policy` constants (the parser via the
`FailureRetryable*` re-exports of `failureclass.*`, which are themselves
re-exports of `policy.*`), so the two cannot silently diverge. That rename-drift
layer is deleted, not pinned by a synchronization test: there is no second
spelling to keep in sync, so no cross-component test is added to guard it. This
deletes the `FixerCompletionKinds` carrier, the fixer call-site wiring, and the
cross-component synchronization coverage an earlier draft introduced solely to
pass those two values across the `internal/agent`/`internal/fixer` boundary —
the simpler derivation the repo's "prefer deletion over another layer" guideline
requires before adding a carrier. `internal/agent` stays a lightweight package
that imports the stdlib-only `policy` leaf, not `failureclass` or the infra
stack.

`AppendFixerCompletionInstruction` advertises each `failure_kind` value in two
distinct forms; both are reconstructed from the imported `policy` constants so a
rename that updates one form but misses the other is a compile error, not a
silent drift:

1. The quoted bullet tokens `- "retryable_transient":` and
   `- "manual_intervention":` (a leading `- `, a double-quoted value, then `:`)
   are built as `"- " + strconv.Quote(string(policy.RetryableTransient)) + ":"`
   and the same for `policy.ManualIntervention`, embedded in the joined slice.
   The prompt intentionally advertises only those two (not
   `retryable_after_resume`, which the parser still accepts — see the comment at
   `parseFixerBlockedFailureKind`), so only the advertised subset is derived.
2. The blocked-completion example at `internal/agent/prompt.go:57`
   (`{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`)
   independently embeds `manual_intervention` as a `"failure_kind":"<value>"`
   occurrence. That example is a separate spelling of the same vocabulary: if
   `policy.ManualIntervention` is renamed and the bullet token is updated but the
   example is missed, the prompt keeps demonstrating a value that
   `parseFixerBlockedFailureKind` would reject after the rename. The example is
   therefore reconstructed with
   `"\"failure_kind\":" + strconv.Quote(string(policy.ManualIntervention))` so
   the same rename updates it.

The call site in `internal/fixer/runner.go:7307` (inside `buildFixerPrompt`) is
unchanged — it stays
`agent.AppendFixerCompletionInstruction(strings.Join(parts, "\n\n"))` — because
the builder now derives the advertised values itself from the `policy` import
rather than receiving them as arguments. No `FixerCompletionKinds` struct is
declared, and no `string(failureclass.*)` wiring is added at the fixer call site.

The existing `internal/agent/prompt_test.go` is rewritten to import
`internal/loops/policy` (the same stdlib-only leaf the production builder
imports, so `go test ./internal/agent` still does not compile `failureclass` or
its `internal/infra/github`/`diffanchor`/`outboundguard` dependency tree) and to
assert the builder embeds the shared constants, not independent literals. It
parses every advertised bullet token out of the produced prompt — each line
matching the `- "<value>":` form (a leading `- `, a double-quoted value, then
`:`) — collecting the quoted values into a set, and compares that parsed set for
equality with exactly
`{string(policy.RetryableTransient), string(policy.ManualIntervention)}`,
the subset `parseFixerBlockedFailureKind` honors as advertised outcomes. The
comparison is set-equality, not membership: a missing expected bullet fails it,
and so does any extra bullet — `string(policy.RetryableAfterResume)` (still
accepted on input, not advertised), `string(policy.NonRetryable)`, or any
arbitrary third token retained or later added. An agent following a stray bullet
has its blocked outcome rejected by `parseFixerBlockedFailureKind`, so the gate
must reject the prompt that advertises it, not merely confirm the two expected
bullets are present.

Set-equality alone is not sufficient: the builder reads two `policy` constants
and places each on a description bullet, and nothing in the derivation
constrains which constant fills which bullet. `AppendFixerCompletionInstruction`
could place `policy.ManualIntervention` on the retryable-description bullet and
`policy.RetryableTransient` on the manual-description bullet, and the
set-equality check would still see both expected values, while the
blocked-completion example (built from `policy.ManualIntervention` directly)
would still embed the manual value — so both assertions pass while agents
receive reversed guidance. The test therefore also asserts each advertised
bullet pairs its value with its corresponding description: the bullet whose
description text is the retryable guidance ("another attempt at the repair
could succeed...") must carry `string(policy.RetryableTransient)`, and the
bullet whose description text is the manual guidance ("no retry can succeed
without a human decision...") must carry
`string(policy.ManualIntervention)`. The pairing is checked by matching each
bullet line's quoted value against the kind expected for that bullet's
description, so a builder that swaps which constant fills which advertised slot
fails even though the parsed set is still equal. It also asserts the
blocked-completion example
`{"outcome":"blocked","failure_kind":"<value>",...}` embeds
`string(policy.ManualIntervention)` (not the transient value), so a rename that
updates the bullet but misses the example is caught. This is advertised-subset
and field-to-bullet-pairing coverage of the builder's own correctness, exercised
as a unit test in `internal/agent` against the same `policy` constants the
builder uses — not cross-package synchronization coverage. A rename of either
`policy` constant updates the builder and the test's expected tokens together at
compile time, a removal of either advertised bullet is caught because the parsed
set no longer equals the expected set, an addition of any non-advertised bullet
is caught the same way, and a builder that reverses which constant fills which
description bullet is caught by the pairing assertion. The earlier draft's
cross-component fixer test
`TestFixerPromptOffersOnlyHonoredFailureKinds`
(`internal/fixer/runner_repair_outcome_test.go`) is split, not deleted whole.
Its prompt-offers-only-honored-kinds assertions (the part that checked the
prompt advertises only `retryable_transient`/`manual_intervention`) existed
solely to synchronize the prompt's advertised values with the parser's honored
values across the `internal/agent`/`internal/fixer` boundary, and that
synchronization is now compile-time because both sides derive from the same
`policy` constants, so that half is deleted. Its other half — the assertion
that `parseFixerBlockedFailureKind("retryable_after_resume")` stays accepted
and maps to `FailureRetryableAfterResume` — is **retained**, because it is the
repository's only direct coverage of the parser's backward-compatible-acceptance
promise (the comment at `parseFixerBlockedFailureKind` in
`internal/fixer/runner.go` states `retryable_after_resume` stays accepted
rather than rejected so a reporting agent is not downgraded to a contract
failure). The `internal/agent` unit test above intentionally excludes
`retryable_after_resume` from the advertised set, so it still passes if the
parser later rejects that kind; the retained assertion is what fails in that
case. It is relocated out of the deleted cross-component test into a focused
fixer-package test of `parseFixerBlockedFailureKind` (still in
`internal/fixer/runner_repair_outcome_test.go`, asserting the accepted kinds
`manual_intervention`, `retryable_after_resume`, and `retryable_transient`
each map to their constant and an unknown kind is rejected), so the parser's
own allowlist — including the non-advertised `retryable_after_resume` — stays
directly asserted rather than only implied by the prompt test; the prompt's
advertised subset and field-to-bullet pairing are covered by the
`internal/agent` unit test above.

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
- **Share `loopError` across roles.** The four `loopError` structs are *not*
  identical in shape: the reviewer carrier has an additional `interrupted bool`
  field (see the inventory above) that the other three lack and that participates
  in reviewer control flow. Extracting a shared `roles.LoopError` is plausible,
  but the reviewer-specific field means a shared struct either carries an unused
  `interrupted` field for three roles or forces the reviewer to keep a separate
  carrier — a half-extraction either way. Each role also attaches role-specific
  skip-error siblings (`holdSkipError`, `labelMismatchSkipError`,
  `runtimeSkipKind`) and helper methods around it. Pulling the struct out without
  those neighbors would leave a half-extraction; pulling them all out expands
  scope well past the failure-kind ledger the issue names. Deferred.
- **Pass the fixer prompt kinds across packages via a carrier.** Considered,
  rejected. An earlier draft had `AppendFixerCompletionInstruction` take two
  positional `string` arguments (`retryableTransient`, `manualIntervention`)
  pinned by a cross-component test, then revised to a named struct
  (`FixerCompletionKinds{RetryableTransient, ManualIntervention}`) owned by
  `internal/agent` that the fixer call site filled from
  `string(failureclass.*)`. Both designs deliberately introduced a carrier plus
  cross-package wiring (and, for the positional form, a gate solely to detect
  *call-site* swaps) — exactly the "add a layer to pass values across packages"
  pattern the repo's "prefer deletion over another layer" guideline warns
  against. Step 5's reverse-dependency derivation makes the carrier unnecessary:
  `internal/loops/policy` is a stdlib-only leaf that owns the `Kind` type and
  typed constants, so `internal/agent` imports `policy` directly and derives the
  two advertised values inside the builder (Step 6). This deletes the carrier,
  the fixer call-site wiring, and the cross-component synchronization test
  introduced solely to keep the prompt and parser in sync across packages —
  both now trace to the same `policy` constants, so a rename updates them
  together at compile time. The per-bullet description pairing is retained as a
  unit test in `internal/agent` (not a cross-component gate), because the
  derivation does not constrain which constant the builder places on which
  description bullet.
  Recorded here per the guideline that the deletion-first attempt be recorded even
  when adopted.
- **Generalize the step traversal helpers.** See Step 4 — rejected as net
  negative.
- **Pin `policy`'s kind constants to `failureclass` by a drift-detection test.**
  Considered, rejected. The earlier rationale considered only one dependency
  direction — making `policy` import `failureclass`, which would break `policy`'s
  stdlib-only leaf property. The dependency runs the other way with no cycle
  (`policy` imports only the standard library), so `failureclass` can import
  `policy` and alias its `Kind` type and re-export its typed constants at compile
  time (Step 5). A drift gate would retain both ledgers and leave
  correctness dependent on the full suite running, against the repo's "prefer
  deletion over another layer" guideline; the compile-time alias deletes the
  second spelling entirely.

## Trade-off (per design guidelines)

> Delete this six months from now — what breaks?

`QueueFailureKind` is already gone, so there is no alias to delete later. The
local `FailureRetryable*` constants are typed re-exports: they inherit
`failureclass.Kind` from the shared constants (which are declared as `Kind`, not
as untyped string constants), so deleting them later (the further-cleaned end
state) only breaks callers that spell those names literally, which is a
mechanical rename. The conversion functions, once deleted, cannot silently drift
back — there is no second type to reconcile, and `failureclass.Normalize` carries
the unknown-kind fallback explicitly.

> What does it still not catch?

This refactor targets the per-role `QueueFailureKind` ledger and the
`internal/validation` bare-string ledger (deleted by typing
`Policy.FailureKind` as `policy.Kind` and having `PolicyFor` return the
shared constants directly in Step 1). It is deliberately not a repo-wide audit
of every bare-string spelling of the vocabulary. The remaining spellings split
into two groups: those derived from the shared constants at compile time (caught
by import, no drift test), and those explicitly out of scope (not caught at all).
No spelling is pinned by a drift-detection test after Step 5 reverses the leaf
dependency: `policy` owns the kind type and string values and `failureclass`
aliases the type and re-exports the constants by import, so the former "pinned by
test" group is deleted, not retained.

**Derived from the shared constants (caught at the call site by import, no drift
test):**

1. `internal/loops/policy`'s kind constants become the single owner of the kind
   *type and string values*. `policy` gains the two untyped `string` constants it
   currently lacks (`FailureKindRetryableTransient`, `FailureKindNonRetryable`)
   so it owns all four string values, plus the `Kind` type and four typed `Kind`
   constants derived from those untyped strings, and `failureclass` imports
   `policy` and aliases `type Kind = policy.Kind` and re-exports the four typed
   constants at compile time (Step 5). The umbrella `internal/loops` package
   re-exports the two new untyped constants, the `Kind` type alias, and the four
   typed constants alongside its existing ones, keeping its "re-exports every
   name here" contract so the documented `loops.*` API exposes all of them
   (Step 5). The dependency runs in that direction because `policy` is an
   intentional stdlib-only leaf (its package doc states this) so
   `reviewer/workflow` and `internal/validation` can depend on it without
   pulling in the `internal/infra/github` stack `failureclass` carries;
   `failureclass` importing `policy` adds no cycle (`policy` imports only the
   standard library) and no transitive dependency any `failureclass` importer
   did not already carry. A rename of any `policy` constant propagates to
   `failureclass`, to the umbrella, and to every runner at compile time, so no
   drift-detection test is added. Resume-policy behavior is unchanged: the
   untyped `FailureKind*` constants stay untyped `string`, and
   `NormalizeResumePolicy` keeps its current signature and branches.

2. `internal/agent/prompt.go:55-57` (`AppendFixerCompletionInstruction`) embeds
   `retryable_transient` and `manual_intervention` as string literals in the
   prompt that advertises the `failure_kind` values the fixer agent may report,
   in two forms: the quoted bullet tokens (`- "retryable_transient":`,
   `- "manual_intervention":`) and the blocked-completion example
   (`{"outcome":"blocked","failure_kind":"manual_intervention",...}`). That
   prompt is the advertising layer for `parseFixerBlockedFailureKind`: the
   parser bounds what the agent returns, but the prompt tells the agent what to
   return. `internal/agent` does not import `failureclass` to derive these
   tokens: `failureclass` imports `internal/infra/github`, which `internal/agent`
   does not currently reach, and coupling a generic prompt-construction package
   to the infrastructure-backed classifier would pull that stack into every
   build and test of `internal/agent`. Instead, Step 6 has `internal/agent`
   import the stdlib-only `internal/loops/policy` leaf directly and derive the
   two advertised values from `policy.RetryableTransient` and
   `policy.ManualIntervention` inside the builder, embedding
   `string(policy.*)` instead of independent literals. `policy` is the same
   authority the parser's `FailureRetryable*` constants trace to (via the
   `failureclass.*` aliases), so a rename of either shared value updates the
   prompt and the parser together at compile time and the two cannot silently
   diverge. No carrier struct or cross-package value-passing machinery is
   introduced, so no argument-order or synchronization test is added; the
   second spelling no longer exists in production, so no synchronization test
   guarding a duplicate ledger is added.

(`internal/validation`'s three `FailureKind*` constants previously belonged in a
separate-spelling group; Step 1 now deletes them, types `Policy.FailureKind` as
`policy.Kind` (importing the stdlib-only `policy` leaf, not the infra-backed
`failureclass`), and has `PolicyFor` return the shared constants directly,
so a rename propagates at compile time to `PolicyFor` and to both consumers
(`worker.classifyValidationFailure` and `fixer/failurepolicy.ClassifyValidation`,
which delete their string→kind casts, since `policy.Kind` is the same type as
`failureclass.Kind` via the Step 5 alias). No drift test is needed.)

**Explicitly out of scope (not caught — recorded here so a future rename does
not pass silently):**

3. Several production paths compare or emit the kind vocabulary as bare string
   literals, independent of `failureclass` and of the per-role
   `QueueFailureKind`. A rename of a `failureclass.Kind` value would leave these
   paths on the old spelling while the runners follow the new one, and no test
   added by this refactor catches that:
   - `internal/runtime/runtime.go:3096` writes `ErrorKind: "manual_intervention"`
     into a `storage.QueueFailInput` when the runtime fails a queue item outside
     the runner path (a write surface, not a comparison).
   - `internal/runtime/runtime.go:3671` (`isRuntimeRetryableTransientWithRemainingAttempts`)
     compares `queue.LastErrorKind` against the literal `"retryable_transient"`.
   - `internal/runtime/runtime.go:3965` (`latestQueueIsManualIntervention`)
     compares `queue.LastErrorKind` (and `queue.Status`) against the literal
     `"manual_intervention"`.
   - `internal/api/handler.go:3391` (`isManualInterventionQueue`) compares
     `item.LastErrorKind` (and `item.Status`) against the literal
     `"manual_intervention"`.
   - `internal/api/handler.go:3498` (`isBackingOffQueue`) recognizes only
     `"retryable_transient"` and `"retryable_after_resume"` for backoff display.
   - `internal/runtime/scheduler.go:3268,3561-3563,3633-3682` emits and compares
     bare kind strings (`"retryable_transient"`, `"non_retryable"`) when failing
     snapshot queue items and deciding retry eligibility.
   - `internal/storage/repositories.go:2497` (`QueueRepository.CleanupStaleQueued`)
     writes `last_error_kind = 'non_retryable'` when cancelling stale queued
     items (a write surface embedded in SQL, not a comparison).
   - `internal/storage/repositories.go`'s `longTermRetryPredicateLiteral` /
     `longTermRetryPredicateParam` embed the kind strings in a SQL predicate.
   These are read/write surfaces against the *persisted* kind string, not
   compile-time-typed `failureclass.Kind` values; widening this refactor to
   re-derive them would touch runtime recovery, the HTTP API, the scheduler, and
   the storage layer — well past the failure-kind *type* ledger the issue names.
   They are deferred to a separate, persisted-vocabulary audit that can evaluate
   a migration against the schema constraint below in the same change.
4. The persisted SQLite schema constrains `last_error_kind` to the four old
   string literals. The CHECK constraints in
   `internal/storage/migrations/0003_scheduler_queue.sql`,
   `0004_worker_project_target.sql`, and `0016_queue_infinite_retry_attempts.sql`
   (and the snapshot in `internal/storage/testdata/schema/sqlite-schema.snapshot.sql`)
   accept only `('retryable_transient', 'retryable_after_resume',
   'non_retryable', 'manual_intervention')`. A migration also *writes* the
   literal: `internal/storage/migrations/0013_active_queue_dedupe.sql` backfills
   `last_error_kind = COALESCE(last_error_kind, 'non_retryable')` when collapsing
   duplicate active queues, so a future rename of `NonRetryable` would leave that
   migration writing the old spelling even after the CHECK constraint is updated.
   A rename of a `failureclass.Kind` value that the runners begin writing would
   violate this constraint at insert time — a loud failure, not a silent drift,
   but one that requires a schema migration coordinated with the rename. This
   refactor adds no migration (Goal 3: no persisted-state change) and does not
   claim to catch it; a future rename of any `failureclass.Kind` value must carry
   a matching migration (constraint update plus any backfill migration that
   writes the literal) in the same change.

> What is the authority for failure-kind values, and why is it not the agent's
> own structured output?

There is more than one authority for failure-kind values, and none of them is
the agent's raw structured output. The authorities split by production path:

- For *classifier-mediated* errors — errors raised inside a runner that flow
  through the shared classifier — `failureclass.Classify` is the authority: it
  maps the error and its boundary to a `Kind`. This is not the authority for
  *all* runner-raised errors: several runner paths choose a `Kind` directly
  without invoking `Classify`, and those paths are themselves the authority for
  their semantics. Naming them so the consolidation does not mistake them for
  classifier output:
  - `internal/reviewer/runner.go:2417` (`refreshThreadResolutionCandidate`)
    assigns `FailureRetryableAfterResume` directly when the PR's head or state
    changed during thread reconciliation — a role-specific resume decision, not
    classifier output.
  - `internal/worker/runner.go:2815-2833` (`classifyValidationFailure`) derives
    the validation kind from `validation.PolicyFor` plus hint-based branches
    (dirty worktree → `manual_intervention`, stale checkpoint →
    `retryable_after_resume`, timeout → `retryable_transient`) — a role-owned
    validation classification, not `failureclass.Classify`.
  - `internal/fixer/failurepolicy/policy.go:20-35` (`ClassifyError`) returns the
    fixer's role-policy `Decision` (kind + resume policy) from the error and its
    boundary — a separate role-policy boundary, not the shared `Classify`.
  These role-specific policies are unchanged by this refactor (a Non-goal); the
  consolidation only changes the *type* their results are spelled in (from
  `QueueFailureKind` to `failureclass.Kind`), not the decision logic.
- For the fixer's agent-reported *blocked* outcome,
  `parseFixerBlockedFailureKind` is the authority: its allowlist
  (`manual_intervention`, `retryable_after_resume`, `retryable_transient`;
  everything else rejected as a contract failure) is what bounds the agent's
  `failure_kind` string — `failureclass.Classify` and the `xxxFailureKind`
  conversions are never called on that path. After this refactor the agent
  string is parsed straight into `failureclass.Kind` (the parser's return type
  becomes `failureclass.Kind`), removing the re-wrap while keeping the
  allowlist as the bound. The prompt that advertises those values
  (`AppendFixerCompletionInstruction` in `internal/agent/prompt.go`) is a
  separate spelling of the same vocabulary — it tells the agent what to return,
  the parser bounds what is accepted — and is derived from the `policy` leaf in
  Step 6 (`internal/agent` imports `internal/loops/policy` and embeds
  `string(policy.RetryableTransient)` / `string(policy.ManualIntervention)`
  inside the builder), so a rename updates the prompt and the parser at compile
  time — both trace to the same `policy` constants — and the two cannot
  silently diverge.

Infra signals remain for drift detection, not authority.

## Impact

**Files changed (production):**
- `internal/fixer/runner.go` — delete `QueueFailureKind` + delete `fixerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits. The `AppendFixerCompletionInstruction` call at `runner.go:7307` is unchanged (the builder now derives the advertised values itself from the `policy` import, Step 6, so no `string(failureclass.*)` wiring is added at the call site).
- `internal/reviewer/runner.go` — delete `QueueFailureKind` + delete `reviewerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/worker/runner.go` — delete `QueueFailureKind` + delete `workerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/planner/runner.go` — delete `QueueFailureKind` + delete `plannerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/runtime/scheduler.go` — `workerRunCompletedNotificationInput.FailureKind` becomes `failureclass.Kind` (the one `QueueFailureKind` reference here).
- `internal/loops/failureclass/failureclass.go` — add `Normalize(kind Kind) Kind` (the authority for the unknown-kind fallback); add the `internal/loops/policy` import and alias `type Kind = policy.Kind` and re-export the four typed constants from `policy` at compile time (Step 5), deleting the second spelling rather than pinning it by test; `Classify` logic and public API unchanged.
- `internal/loops/policy/policy.go` — add the two missing untyped `FailureKind*` string constants so the leaf owns all four string values; add `type Kind string` and the four typed `Kind` constants derived from the untyped ones (Step 5); no predicate changes (the untyped constants stay for the `string`-parameter functions).
- `internal/loops/policy.go` (umbrella) — re-export the two new untyped `FailureKind*` constants, the `Kind` type alias, and the four typed constants, keeping the "re-exports every name here" contract (Step 5).
- `internal/validation/validation.go` — delete the three `FailureKind*`
  constants, type `Policy.FailureKind` as `policy.Kind`, have `PolicyFor`
  return the shared constants directly (no `string()` cast), and add the
  `internal/loops/policy` import (not `failureclass`, so `go test
  ./internal/validation` does not pull in the infra stack); the two same-package
  tests that spell the constant names are updated to compare against the typed
  `policy.*` constants — see the call-site audit in Step 1.
- `internal/worker/runner.go` (additional edits beyond the type rename) —
  `classifyValidationFailure` drops the `QueueFailureKind(policy.FailureKind)`
  cast and assigns `kind: policy.FailureKind` directly, now that the field is
  `policy.Kind` (identical to `failureclass.Kind` via the Step 5 alias); the four
  `loopError{... kind: failure.kind}` sites that read `validationFailure.kind`
  (`runner.go:1943,1958,2007,2038`) become `kind:
  failureclass.Normalize(failure.kind)` per Step 2's carrier-source wrapping.
- `internal/fixer/runner.go` (additional edit beyond the type rename) — the
  blocked-outcome `loopError{... kind: blockedKind}` site (`runner.go:3327`)
  becomes `kind: failureclass.Normalize(blockedKind)` per Step 2's
  carrier-source wrapping (`blockedKind` originates from the allowlisting
  `fixerRepairTaskOutcome`, so `Normalize` is a no-op but keeps the gate
  uniform).
- `internal/fixer/failurepolicy/policy.go` — `ClassifyValidation` drops the
  `failureclass.Kind(policy.FailureKind)` cast and assigns
  `Kind: policy.FailureKind` directly, now that the field is `policy.Kind`
  (identical to `failureclass.Kind` via the Step 5 alias).
- `internal/agent/prompt.go` — `AppendFixerCompletionInstruction` adds the
  `internal/loops/policy` import and derives the advertised `failure_kind`
  values from `policy.RetryableTransient` and `policy.ManualIntervention`
  inside the builder, embedding `string(policy.*)` (both the quoted bullet
  tokens and the blocked-completion example) instead of independent literals;
  its signature is unchanged (`func(prompt string) string`). It does not import
  `failureclass`, keeping `internal/agent` decoupled from the
  infrastructure-backed classifier — `policy` is a stdlib-only leaf, so this
  adds no infra edge. No `FixerCompletionKinds` carrier struct is declared and
  no call-site wiring is added; a rename of either `policy` constant updates
  the prompt and the parser together at compile time, so no cross-component
  synchronization test is added (a unit test in `internal/agent`, Step 6,
  covers advertised-subset and field-to-bullet-pairing correctness).
- `internal/loops/policy/policy.go` — gains two new untyped `string` constants
  (`FailureKindRetryableTransient`, `FailureKindNonRetryable`) so it owns all
  four kind string values, plus `type Kind string` and the four typed `Kind`
  constants derived from the untyped ones (Step 5); its existing two constants,
  `NormalizeResumePolicy`, and the other predicates keep their current `string`
  signatures and branches (no behavior change — the untyped constants stay for
  the `string`-parameter predicates). `policy` stays a stdlib-only leaf (the new
  type and constants add no import), so `reviewer/workflow` and
  `internal/validation` depending on it still pull in no infra. No drift test is
  added: `failureclass` imports `policy` and aliases `type Kind = policy.Kind`
  and re-exports the typed constants at compile time (Step 5), so a rename
  propagates by import, not by a synchronization gate.
- `internal/loops/policy.go` — the umbrella package re-exports the two new leaf
  untyped constants (`FailureKindRetryableTransient`, `FailureKindNonRetryable`)
  as aliases alongside the existing two, plus a `Kind` type alias and the four
  typed constant re-exports, satisfying its own package-doc contract
  ("internal/loops re-exports every name here", `policy.go:7`). Without this
  edit the leaf gains names the documented `loops.*` API does not expose, so
  callers using the umbrella path cannot reach the new constants or the `Kind`
  type.

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
a four-kind net that does not yet exist, including a per-runner alias-identity
test in each of the four runner packages that compares all four local
`FailureRetryable*`/`FailureNonRetryable`/`FailureManualIntervention` re-exports
against their shared `failureclass.*` constants, so a mis-paired re-export is
caught in the role that contains it rather than only in aggregate.

No policy drift-detection test is added: Step 5 reverses the leaf dependency so `failureclass` imports `policy`
and aliases `type Kind = policy.Kind` and re-exports the typed constants at
compile time, and no validation drift test is added because Step 1 deletes the
validation constants, types `Policy.FailureKind` as `policy.Kind`, and has
`PolicyFor` return the shared constants directly (a rename propagates at compile
time in both cases). The fixer-prompt call path is covered by a unit test in
`internal/agent`: `internal/agent/prompt_test.go` is rewritten to import
`internal/loops/policy` (the same stdlib-only leaf the production builder
imports) and exercise `AppendFixerCompletionInstruction`, parsing every
advertised bullet token out of the produced prompt and asserting the parsed set
equals exactly `{string(policy.RetryableTransient), string(policy.ManualIntervention)}`
— the `failure_kind` values `parseFixerBlockedFailureKind` honors as advertised
outcomes — as advertised-subset coverage, and asserting each advertised bullet
pairs its value with its corresponding description so a builder-side
field-to-bullet swap fails even when the parsed set is still equal. The builder
derives the values itself from the `policy` import, so there is no call site to
swap and no carrier struct to fill; the per-bullet description pairing catches
the builder-side swap the derivation cannot prevent. The earlier draft's
cross-component test
`TestFixerPromptOffersOnlyHonoredFailureKinds`
(`internal/fixer/runner_repair_outcome_test.go`) is split, not deleted whole:
its prompt-advertised-values synchronization half is deleted (now compile-time
because both sides derive from the same `policy` constants), while its
parser-acceptance half — the assertion that
`parseFixerBlockedFailureKind("retryable_after_resume")` stays accepted — is
retained and relocated into a focused fixer-package test of
`parseFixerBlockedFailureKind`, because it is the only direct coverage of the
parser's backward-compatible-acceptance promise and is not subsumed by the
`internal/agent` test (which excludes `retryable_after_resume` from the
advertised set). The unit test does not import
`failureclass`, keeping `go test ./internal/agent` free of the
`internal/infra/github`/`diffanchor`/`outboundguard` dependency tree (Step 6).

**No persisted-state change:** The string values written to the queue/run
records are identical before and after. No migration, no schema touch.

## Risks

1. **Type-name rename.** `QueueFailureKind` is replaced by `failureclass.Kind`
   at 60 sites. If any code relied on the two being *distinct* types (e.g. a type
   switch distinguishing them, or an interface implemented by only one), it
   would break. The typed `QueueFailureKind` surfaces are not just the scheduler
   field and `loopError.kind`; the full inventory of declared `QueueFailureKind`
   surfaces that change type:

   - **Exported result structs** (one per role): `fixer.ProcessResult.FailureKind`
     (`internal/fixer/runner.go:581`), `reviewer.ProcessResult.FailureKind`
     (`internal/reviewer/runner.go:521`), `worker.ProcessResult.FailureKind`
     (`internal/worker/runner.go:598`), `planner.ProcessResult.FailureKind`
     (`internal/planner/runner.go:388`).
   - **Exported input struct:** `worker.RunCompletedInput.FailureKind`
     (`internal/worker/runner.go:452`), consumed cross-package by the runtime.
   - **Exported, JSON-persisted checkpoint field:** `fixer.FixerOutcomeFailure.Kind`
     (`internal/fixer/runner.go:700`, tagged `json:"kind,omitempty"`), serialized
     inside `fixerCheckpoint` via `encoding/json`.
   - **Cross-package scheduler field:**
     `runtime.workerRunCompletedNotificationInput.FailureKind`
     (`internal/runtime/scheduler.go:241`), declared `worker.QueueFailureKind`.
   - **Internal carrier structs:** `loopError.kind` in all four runners
     (`fixer/runner.go:918`, `reviewer/runner.go:608`, `worker/runner.go:716`,
     `planner/runner.go:480`) and `worker.validationFailure.kind`
     (`internal/worker/runner.go:2811`).
   - **Function parameters:** each role's `failQueueItem`/`isQueueRetryEligible`/
     `shouldRetryQueueFailure` (and fixer's `requeueQueueItem`/
     `requeueOrFailQueueItem`/`failQueueItemTerminal`, worker's
     `reconcileRecoveredLoop`/`buildRunCompletedInput`/`shouldNotifyCompletedRun`/
     `issueClaimStatusForFailure`, planner's `reconcileRecoveredLoop`).
   - **Function return types:** `fixerRepairTaskOutcome`
     (`internal/fixer/runner.go:1367`, returns `(bool, string, QueueFailureKind,
     *loopError)`) and `parseFixerBlockedFailureKind`
     (`internal/fixer/runner.go:1411`, returns `(QueueFailureKind, bool)`). These
     are the structured-agent outcome and allowlisting boundaries whose declared
     return type changes from `QueueFailureKind` to `failureclass.Kind`; their
     callers feed the returned kind into `loopError.kind` (the `blockedKind` flow
     Step 2 group 2 covers), so they are part of the caller audit, not just
     internal plumbing. (The four `xxxFailureKind` conversion functions also
     return `QueueFailureKind`, but they are deleted in Step 2, not retyped.)
   - **Test spelling:** `internal/fixer/runner_repair_outcome_test.go:24`
     (`wantKind QueueFailureKind`).

   Two of these surfaces cross a package or persistence boundary and need an
   explicit contract check beyond "it compiles":

   - *Cross-package (worker → runtime):* `worker.RunCompletedInput.FailureKind`
     and `runtime.workerRunCompletedNotificationInput.FailureKind` both become
     `failureclass.Kind`, so the value passes from worker to runtime without a
     conversion (previously the scheduler field was `worker.QueueFailureKind` and
     received a `worker.QueueFailureKind` value). The value set is identical, so
     runtime behavior is unchanged; `go build ./...` confirms the cross-package
     assignment compiles.
   - *JSON-persisted (fixer checkpoint):* `fixer.FixerOutcomeFailure.Kind` is
     serialized to and deserialized from the fixer checkpoint JSON. Both
     `QueueFailureKind` and `failureclass.Kind` are `type X string` with the same
     underlying string, so `encoding/json` marshals and unmarshals the field to
     the identical JSON string before and after the change — the persisted
     checkpoint shape is byte-identical, satisfying Goal 3. Because the field is
     read back into a `failureclass.Kind` (formerly `QueueFailureKind`) and
     compared only by string value against the four known kinds, no caller
     depends on the named type being distinct. Historical checkpoints written
     under the old type name deserialize unchanged because JSON decoding keys
     only on the JSON value, not the Go type name.

   All of these are `internal/` surfaces, so the API audience is in-module and
   fully covered by `go build ./...`. Audit found no type switch on
   `QueueFailureKind`, no interface implemented by only one of the two types, and
   no `reflect` use keyed on the named type. Risk: negligible, caught by
   `go build` plus the JSON-shape equivalence above.
2. **Constant re-export typing.** The local `FailureRetryableTransient` is a
   typed constant inheriting `failureclass.Kind` from
   `failureclass.RetryableTransient` (which is declared as `Kind`, not as an
   untyped string constant). It is therefore assignment-compatible with every
   `failureclass.Kind` surface and needs no conversion. If any code took the
   kind by reference or used `reflect` on the constant's type, behavior could
   differ from the old `QueueFailureKind`-typed declaration; audit found no
   reflective use and no `&FailureRetryable*` address-of use. Risk: low.
3. **Scope creep into step types.** The temptation to also merge `workflow.Step`
   is real and would be a regression (loss of per-role type safety). The spec
   explicitly excludes it; a reviewer should reject any diff that touches `Step`.
4. **Second-fix signal.** This area (`failureclass`/role failure kinds) has not
   received a recent `fix:`; this is the first consolidation, not a patch on a
   patch. No revert signal.

## Validation

Per `AGENTS.md`, the root commands are the source of truth:

1. `go build ./...` — the compilation check: confirms the deleted type and
   conversion functions compile across all packages, including the scheduler's
   `workerRunCompletedNotificationInput.FailureKind` now typed as
   `failureclass.Kind`.
2. `go vet ./...` — runs the standard vet analyzers (printf, struct tags, etc.).
   It does **not** detect general unused symbols or variable shadowing; those are
   covered by step 3.
3. `go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 -tests=false -checks='U1000,SA1006,SA4004,SA4006' ./...` — the unused-code check: confirms no
   newly-unused constants or functions remain (the re-exported
   `FailureRetryable*` must stay referenced; the deleted `xxxFailureKind`
   functions must not leave dead callers).
4. `go test ./...` — the full suite is green.
5. `test -z "$(gofmt -l .)"` — fails with a non-zero exit when any file has
   formatting drift. (`gofmt -l .` alone exits successfully even when it lists
   unformatted files, so it cannot gate CI on its own.)
6. **Four-kind regression coverage.** The existing
   `internal/{fixer,worker,planner}/failure_classification_test.go` files cover
   only `retryable_transient` and `non_retryable` via `classifyFailure`, and
   `internal/reviewer` has no such file. The implementation adds focused
   coverage so that each of the four `failureclass.Kind` values is asserted by
   at least one role's classification test (including a new
   `internal/reviewer/failure_classification_test.go`), plus a test for
   `failureclass.Normalize` covering the four known kinds and an unknown kind.
   This is the regression net the refactor relies on; it must exist before the
   conversion functions are deleted. Aggregate "each kind appears in at least
   one role" coverage is not sufficient on its own: the four per-runner
   re-export blocks (Step 1) are independent, so wiring one role's
   `FailureManualIntervention` to `failureclass.NonRetryable` still compiles,
   passes the call-site gate (it is a known-kind constant), and satisfies the
   aggregate coverage when another role tests the manual kind, while that
   role's failures silently change behavior. The implementation therefore adds
   a per-runner alias-identity test in each of the four runner packages
   (`internal/{fixer,reviewer,worker,planner}`) that compares all four local
   aliases — `FailureRetryableTransient`, `FailureRetryableAfterResume`,
   `FailureNonRetryable`, and `FailureManualIntervention` — against their
   shared `failureclass.*` constants
   (`failureclass.RetryableTransient`, `RetryableAfterResume`, `NonRetryable`,
   `ManualIntervention`) in every runner, so a swapped or mis-paired re-export
   is caught in the role that contains it rather than only in aggregate. (The
   alternative — removing the local aliases and having every call site spell
   `failureclass.*` directly — was considered and rejected: the ~290 call sites
   that spell the short names are unchanged by this refactor, and re-exporting
   the constants is a compile-time alias that adds no persisted state or
   runtime ledger, so the per-runner identity test is the smaller complete
   check. A rename of a shared value still propagates at compile time; the
   identity test guards only against a re-export that points at the wrong
   constant, which the compiler cannot detect because all four share one type.)
7. **Deleted-symbol absence check.** `go build ./...` can pass even if a stale
   alias or conversion function remains, so the definition of done is verified
   by an explicit repository search that fails when `QueueFailureKind` or any of
   `fixerFailureKind`, `reviewerFailureKind`, `workerFailureKind`, or
   `plannerFailureKind` remains in the affected internal packages, rather than
   relying only on the hard-coded reference count:

   ```shell
   #!/usr/bin/env bash
   set -euo pipefail

   if rg -n \
     '\bQueueFailureKind\b|\b(fixerFailureKind|reviewerFailureKind|workerFailureKind|plannerFailureKind)\s*\(' \
     internal/fixer internal/reviewer internal/worker internal/planner internal/runtime
   then
     echo "Deleted failure-kind symbols remain" >&2
     exit 1
   fi
   ```
8. **Normalize call-site type-aware check (Step 2).** The `Normalize` unit test
   and the four-kind runner tests do not prove the former `xxxFailureKind` call
   sites (and the known-safe carrier reads — see Step 2's group 2) actually
   invoke `failureclass.Normalize` — a direct `kind: d.Kind` or
   `kind: failureclass.Classify(...)` assignment bypasses the fallback but
   still passes those tests, staticcheck, and the deleted-symbol search (step 7).
   A line-regex gate is not sound here: it only inspects the text of the `kind:`
   line, so an indirect assignment that first stores the dynamic value in a local
   (`classified := failureclass.Classify(...)` then `kind: classified`) bypasses
   `Normalize` while matching no dynamic-source pattern on the `kind:` line. The
   gate must therefore see through locals, which a regex cannot.

   The check is a Go test, not a shell regex, using the stdlib `go/parser` +
   `go/ast` + `go/types` (the same parser-based pattern the repo already uses
   for structural contract tests such as
   `internal/runtime/runs_service_absent_test.go`, extended with `go/types`
   because this gate makes claims parser-only analysis cannot prove; no
   `golang.org/x/tools` dependency is added — `go/types` is standard library).
   It parses and type-checks each of the four runner packages
   (`internal/{fixer,reviewer,worker,planner}`), locates every `loopError`
   composite literal `kind` element (and every assignment to a `loopError`
   value's `.kind` field), and classifies the value expression by
   intra-procedural origin tracking within the enclosing function. It also
   flags any `&<receiver>.kind` unary address-of expression whose selector
   receiver `go/types` resolves to `loopError` as **unsafe** — forbidding taking
   the field's address — because an address-taken `kind` field can be mutated
   through the pointer (`dst := &failure.kind; *dst = decision.Kind`) without
   producing an assignment whose left side is `.kind` or a dynamic
   composite-literal element for the origin tracker to visit, so the pointer
   alias would otherwise bypass `Normalize` while the gate claims every
   assignment is covered. There is no legitimate production reason to take the
   address of `loopError.kind` (it is a value field read by value, never passed
   to a mutator), so the gate fails closed on the address-of expression itself
   rather than attempting to track pointer aliases and indirect stores through
   them, which is inter-procedural and out of scope for the same reason the
   parameter rule is conservative. (A `*<ptr>` store whose pointer does not
   originate from `&<loopError>.kind` within this function is not reachable
   today — the audit found no `&failure.kind` or `&loopError`-kind address-of
   use — so the address-of rule is a regression net against a future
   pointer-mutation construction, the same stance the zero-valued-construction
   rule below takes.) A `loopError` composite literal that **omits** the `kind`
   element, a `new(loopError)` call, or a `var <name> loopError` declaration produces a
   zero-valued `loopError` whose `kind` is the empty-string default — an
   unknown kind that bypasses `Normalize` exactly as the uninitialized `Kind`
   local below rejects — so the gate treats each of those constructions as
   **unsafe** as well: it flags any `loopError` composite literal whose element
   list has no `kind` key, any `new(loopError)` expression, and any
   `var <name> loopError` (or `var <name> loopError = <zero-value>`) declaration
   whose declared local is later returned, assigned to a `loopError` value's
   `.kind`, or otherwise flows to a `*loopError`/`loopError` use, because none
   of them supplies a `kind` for the origin tracker to classify. (A zero-valued
   local that is never used is not flagged — the gate enforces the invariant on
   `loopError` values that reach a return or a `.kind` copy, not on dead
   declarations.) File
   selection is build-aware before parsing: the worker package contains
   mutually-exclusive build-constrained files (`internal/worker/specfile_unix.go`
   with `//go:build darwin || linux` and `specfile_other.go` with
   `//go:build !darwin && !linux`) that both declare `openSpecFileBeneath`;
   loading the directory's full file set with `go/parser` would include both, and
   `go/types` would report a duplicate declaration before this invariant is ever
   checked. The test therefore selects files with `go/build.Context.MatchFile`
   (using `build.Default`, which honors `//go:build` and the current
   `GOOS`/`GOARCH`) so only the one file active under the build constraints is
   parsed and type-checked, matching what `go build` actually compiles.
   `MatchFile` alone is not sufficient: it applies filename and build-constraint
   matching but still returns `true` for `_test.go` files, so it does not match
   `go build`, which compiles only production sources. Type-checking the
   packages' `runner_test.go` files would enforce the production
   `loopError.kind` invariant on test-only literals — a test that deliberately
   constructs an unknown kind (e.g. a sentinel `failureclass.Kind` value) could
   then fail this structural gate even though it cannot affect production. The
   test therefore excludes `_test.go` files explicitly (skipping any
   `MatchFile`-matched file whose base name ends in `_test.go`), so the gate
   type-checks exactly the production file set `go build` compiles. Imports
   are resolved via a source importer. Selector
   ownership and call targets are resolved from `go/types` info, not from
   selector spelling: a `.kind` read is treated as a `loopError` copy only when
   `go/types` resolves its receiver type to `loopError`, and a
   `failureclass.Normalize(...)` / `failureclass.Classify(...)` call is
   recognized only when `go/types` resolves it to the imported `failureclass`
   package's function, so a shadowed `failureclass` identifier or an unrelated
   struct's lowercase `kind` field cannot pass as safe:

   - A call resolved by `go/types` to `failureclass.Normalize(...)` — **safe**
     (normalized).
   - A reference to a known-kind constant — **safe** (a known kind). The set of
     accepted constants is **derived from the authority, not enumerated in the
     gate**: the test uses `go/types` to enumerate every `const` declaration
     whose type is `policy.Kind` (the authority after Step 5) in the imported
     `internal/loops/policy` package and every `const` declaration whose type is
     `failureclass.Kind` (the `type Kind = policy.Kind` alias) in the imported
     `internal/loops/failureclass` package, plus the per-role
     `FailureRetryable*` / `FailureNonRetryable` / `FailureManualIntervention`
     re-exports by resolving each to the `failureclass.*` / `policy.*` constant
     it aliases. A reference is safe when `go/types` resolves it to one of those
     authority constants (directly or through a re-export alias). The gate never
     spells the four current names; today that set is
     `failureclass.RetryableTransient` / `RetryableAfterResume` / `NonRetryable`
     / `ManualIntervention` (and the per-role re-exports of them), but when a
     fifth `policy.Kind` constant is added and `failureclass.Normalize` is
     extended to preserve it, a safe direct assignment such as
     `kind: FailureNewKind` is accepted automatically because the new constant
     is already in the derived set — the gate does not recreate a manually
     synchronized failure-kind ledger inside itself. (A constant of type
     `policy.Kind` that `Normalize` does *not* preserve would still pass this
     branch; that gap is closed by the `Normalize` unit test in step 6, which is
     the authority for the fallback behavior, not by the call-site gate.)
   - A `.kind` selector read whose receiver `go/types` resolves to `loopError`
     (`kind: failure.kind`) — **safe only when the receiver's own origin is
     safe**. Resolving the receiver's *type* to `loopError` is necessary but not
     sufficient: a `loopError` parameter or a `loopError` returned from an
     untracked helper can carry an empty or unknown `kind`, so accepting every
     `loopError`-typed selector read as a copy of an already-normalized kind
     opens the same untracked-source hole the parameter rule below closes. The
     receiver identifier is therefore resolved to its origin within the
     enclosing function by the same rule applied to bare identifiers: if the
     receiver is a **local variable**, resolve it to every reaching assignment
     and classify each right-hand side — **safe** only if every reaching
     assignment is itself a `loopError` composite literal whose `kind` element
     is safe (recursively), so a locally-constructed `loopError` whose `kind`
     was normalized copies a safe kind; **unsafe** if any reaching assignment is
     a function parameter, a call `go/types` does not resolve to a known-safe
     producer, or any other untracked source. If the receiver is a **function
     parameter** (a `loopError` declared in the enclosing signature with no
     intra-procedural assignment) — **unsafe**: its `kind` originates from an
     untrusted call-site argument the intra-procedural check cannot trace, so a
     helper receiving a `loopError` parameter and returning
     `&loopError{kind: input.kind}` cannot pass the selector read as safe; the
     implementer must wrap the read in `failureclass.Normalize(input.kind)`.
     (Tracing every call-site argument is inter-procedural and out of scope; the
     conservative choice is unsafe-by-default, the same stance the parameter
     rule below takes for a bare `Kind` parameter.) This keeps the
     `loopError.kind` copy rule consistent with the unsafe-parameter rule rather
     than contradicting it: the copy is safe only when the copy's source is
     proven safe within this function, not merely because the source is typed
     `loopError`.
   - A bare identifier that is a **local variable** — resolve it to every
     assignment to that name within the same function and classify each
     right-hand side recursively; **safe** only if there is at least one
     reaching assignment and every reaching assignment is itself safe,
     **unsafe** if the local has no reaching assignments (an uninitialized
     zero-value `Kind` is an unknown kind — the empty-string default — so a
     helper that declares `var kind failureclass.Kind` and then returns
     `&loopError{kind: kind}` without ever writing the local passes the empty
     assignment set vacuously, the same hole the parameter rule below closes)
     or if any reaching assignment is a dynamic source not wrapped in
     `Normalize`.
   - A bare identifier that is a **function parameter** (a name declared in the
     enclosing function's signature with no intra-procedural assignment) —
     **unsafe**. A parameter has no local definition to trace, so the
     "every reaching assignment is safe" rule would accept it vacuously (an
     empty set of assignments is trivially all-safe), letting a helper such as
     one returning `&loopError{kind: kind}` from a `Kind` parameter bypass
     `Normalize` while this gate claims every indirect bypass fails. The
     parameter's value originates from an untrusted call-site argument the
     intra-procedural check cannot trace, so it is treated like any other
     untracked source: the implementer must wrap it in
     `failureclass.Normalize(kind)` at the assignment site. (Tracing and proving
     every call-site argument is inter-procedural and out of scope for this
     gate; the conservative choice is unsafe-by-default.)
   - A dynamic source — a call resolved by `go/types` to
     `failureclass.Classify(...)`, or a `.Kind` selector read whose receiver
     `go/types` resolves to a non-`loopError` struct (e.g. `Decision.Kind`,
     `failure.Kind` on a `validationFailure`) — **unsafe** unless it is the
     argument of a call resolved to `failureclass.Normalize(...)`. This is the
     rule the Step 2 carrier reads satisfy: `kind: failure.kind` where `failure`
     is a `validationFailure` is a `.kind` read on a non-`loopError` struct, so
     it is dynamic and **unsafe** unless wrapped — which is why Step 2 wraps it
     in `failureclass.Normalize(failure.kind)` rather than allowlisting
     `validationFailure` as a "safe carrier". Likewise `kind: blockedKind`
     resolves `blockedKind` to its assignment from `fixerRepairTaskOutcome(...)`,
     a call `go/types` does not resolve to `failureclass.Normalize` or
     `failureclass.Classify`, so it falls through to the next branch.
   - Any other expression — **unsafe** (conservative: an untracked source could
     carry a future unknown kind past the fallback). This is the branch
     `blockedKind` (traced to `fixerRepairTaskOutcome(...)`) reaches, which is
     why Step 2 wraps it in `failureclass.Normalize(blockedKind)` too.
   - A `loopError` construction that supplies **no `kind` at all** — a composite
     literal whose element list omits `kind` (e.g. `&loopError{message: msg}`),
     a `new(loopError)` call, or a `var <name> loopError` zero-value declaration
     whose local flows to a return, a `.kind` copy, or any other
     `*loopError`/`loopError` use — **unsafe**. These never produce a `kind`
     element or `.kind` assignment for the origin tracker to classify, so the
     rules above would not visit them at all and the construction would pass
     silently with the empty-string default. That is the same hole the
     uninitialized `Kind` local rule closes: a `var kind failureclass.Kind`
     never written is unsafe because its zero value is an unknown kind, and a
     `loopError` built with no `kind` element carries that same zero value with
     no assignment to flag. The gate therefore flags the construction itself,
     not a `kind` expression inside it, so the implementer must add an explicit
     `kind:` element (a known-kind constant or a `Normalize`-wrapped dynamic
     source) rather than rely on the zero default. A zero-valued local that is
     never used is not flagged, matching the dead-declaration carve-out above.

   The test fails on the first **unsafe** `kind` assignment (or zero-valued
   `loopError` construction, or address-of `loopError.kind` expression),
   reporting the file and position. This closes the
   indirect-assignment hole: `kind: classified`
   resolves `classified` back to `failureclass.Classify(...)` and fails, so the
   implementer must write `kind: failureclass.Normalize(classified)`. The
   existing safe shapes keep passing — `kind: FailureRetryableTransient`,
   `kind: failure.kind` (where `failure` is a `loopError` *local* constructed in
   this function with a safe `kind`), and `kind: kind`
   where `kind` is a local only ever assigned from `FailureRetryable*` constants
   all resolve to a safe origin; a `kind` that is a function parameter is
   **unsafe** unless wrapped, because its origin is an untraceable call-site
   argument, and likewise a `kind` local that is never assigned (an
   uninitialized zero-value `Kind`) is **unsafe** unless wrapped, because its
   empty-string default is an unknown kind the reaching-assignment rule would
   otherwise accept vacuously, and likewise a `.kind` read off a `loopError`
   parameter or a `loopError` returned from an untracked helper is **unsafe**
   unless wrapped, because the receiver's `kind` is not proven safe within this
   function, and likewise a `loopError` built with no `kind` element (or via
   `new(loopError)`/`var <name> loopError`) is **unsafe** because its zero-value
   `kind` is an unknown kind with no assignment for the origin tracker to visit,
   and likewise `&<loopError>.kind` is **unsafe** because the address-taken field
   can be mutated through the pointer without an assignment the origin tracker
   visits.
   The carrier reads pass because Step 2 wraps them in
   `Normalize`, not because the gate recognizes their producers. The
   dynamic-vs-copy distinction is no longer made by selector name: `go/types`
   resolves the receiver, so a `.Kind` read on `Decision`/validation structs is
   dynamic and a `.kind` read on a `loopError` is a copy, regardless of spelling
   — a struct that happened to spell its field the other way could not fool the
   gate. A `loopError`-typed copy is safe only after the receiver's origin is
   traced, so a `loopError` parameter or untracked-helper result cannot pass as
   a safe copy the way a locally-constructed `loopError` can.

   This is covering, not propping up: it is the first enforcement of the
   `Normalize` fallback invariant the refactor relies on, and it replaces a
   regex that claimed to enforce that invariant but could not. The production
   surface it guards does not grow — the seven dynamic-source call sites and the
   five known-safe carrier reads already route through `Normalize` after Step 2,
   and every existing `loopError` composite literal already supplies an explicit
   `kind:` element (there are no `new(loopError)`, `var <name> loopError`,
   `kind`-omitting literals, or `&<loopError>.kind` address-of expressions in
   production today) — so the zero-valued construction and address-of rules
   flag nothing on the current branch and are a regression
   net against a future construction that would otherwise bypass the fallback
   silently. The test is a regression net, not a new state machine.

   (Step 5's reverse-dependency derivation — `failureclass` importing `policy`
   and aliasing `type Kind = policy.Kind` and re-exporting the typed constants —
   needs no separate validation step: it is a compile-time alias and constant
   re-export, so a rename in `policy` that `failureclass` does not follow is a
   `go build` failure covered by step 1. No policy or validation drift test is
   added or needed.)
9. **Fixer-prompt builder coverage (Step 6).**
   `internal/agent/prompt_test.go` is rewritten to import
   `internal/loops/policy` (the same stdlib-only leaf the production builder
   imports, so `go test ./internal/agent` does not compile `failureclass` or its
   infra dependency tree) and to exercise `AppendFixerCompletionInstruction`,
   parsing every advertised bullet token (each `- "<value>":` line) out of the
   produced prompt and asserting the parsed set equals exactly
   `{string(policy.RetryableTransient), string(policy.ManualIntervention)}`
   — the kinds `parseFixerBlockedFailureKind` honors as advertised outcomes — so a
   missing expected bullet or any extra bullet (`retryable_after_resume`,
   `non_retryable`, or any later-added token) fails the set-equality check. It
   also asserts each advertised bullet pairs its value with its corresponding
   description: the retryable-description bullet must carry
   `string(policy.RetryableTransient)` and the manual-description bullet must
   carry `string(policy.ManualIntervention)`, so a builder that swaps which
   constant fills which advertised slot fails even when the parsed set is still
   equal, and asserts the blocked-completion example embeds
   `string(policy.ManualIntervention)` (not the transient value). This is
   advertised-subset and field-to-bullet-pairing coverage of the builder's own
   correctness, exercised as a unit test in `internal/agent` against the same
   `policy` constants the builder uses — not cross-package synchronization
   coverage. The earlier draft's cross-component test
   `TestFixerPromptOffersOnlyHonoredFailureKinds`
   (`internal/fixer/runner_repair_outcome_test.go`) is split, not deleted whole:
   its prompt-advertised-values synchronization half is deleted (now compile-time
   because both sides derive from the same `policy` constants), while its
   parser-acceptance half — the assertion that
   `parseFixerBlockedFailureKind("retryable_after_resume")` stays accepted and
   maps to `FailureRetryableAfterResume` — is retained and relocated into a
   focused fixer-package test of `parseFixerBlockedFailureKind` (asserting the
   accepted kinds `manual_intervention`, `retryable_after_resume`, and
   `retryable_transient` each map to their constant and an unknown kind is
   rejected), because it is the only direct coverage of the parser's
   backward-compatible-acceptance promise and is not subsumed by the
   `internal/agent` test (which excludes `retryable_after_resume` from the
   advertised set, so it still passes if the parser later rejects that kind).

**Definition of done:** `QueueFailureKind` is gone from all four runners (the 60
type-name references replaced by `failureclass.Kind`), the four `xxxFailureKind`
functions are gone and `failureclass.Normalize` preserves their unknown-kind →
`non_retryable` fallback, `internal/validation`'s `FailureKind*` constants are
deleted, `Policy.FailureKind` is typed `policy.Kind` (importing the stdlib-only
`policy` leaf, not the infra-backed `failureclass`), and `PolicyFor`
returns the shared constants directly (with both consumers'
string→kind casts deleted, since `policy.Kind` is the same type as
`failureclass.Kind` via the Step 5 alias), `internal/loops/policy` owns the
`Kind` type and all four kind string constants and `failureclass` imports
`policy` and aliases `type Kind = policy.Kind` and re-exports the typed
constants from it at compile time (no policy or validation drift test), the
umbrella `internal/loops` package re-exports the two new untyped policy
constants (`FailureKindRetryableTransient`, `FailureKindNonRetryable`), the
`Kind` type alias, and the four typed constants alongside the existing ones so
the documented `loops.*` API exposes all of them, the fixer
prompt's advertised `failure_kind` tokens are derived from
`policy.RetryableTransient` and `policy.ManualIntervention` inside
`AppendFixerCompletionInstruction` (which imports the stdlib-only
`internal/loops/policy` leaf, not `failureclass`, so no `FixerCompletionKinds`
carrier, fixer call-site wiring, or cross-component synchronization test is
added), the
deleted-symbol absence check (step 7) passes, the
Normalize call-site type-aware check (step 8) passes — so every former
`xxxFailureKind` call site routes its dynamic `failureclass.Kind` through
`failureclass.Normalize`, and the known-safe carrier reads
(`validationFailure.kind`, `blockedKind` from `fixerRepairTaskOutcome`) are
`Normalize`-wrapped too so the gate stays uniform (no carrier-name allowlist),
and a bypass fails the suite whether the dynamic
value is assigned inline or stored in a local first (the type-aware check
traces locals to their origin and treats a local with no reaching assignments
as unsafe so an uninitialized zero-value `Kind` cannot pass vacuously, flags a
`loopError` composite literal that omits `kind` (or a `new(loopError)`/`var
<name> loopError` zero-value construction that flows to a use) as unsafe so an
empty-string default kind cannot bypass `Normalize` the same way, traces
`loopError.kind` selector reads back to their receiver's origin so a
`loopError` parameter or untracked-helper result is not accepted as a safe
copy, and resolves selector ownership and call targets via `go/types`, and
selects files with `go/build.Context.MatchFile` (excluding `_test.go` files so
the gate type-checks only the production sources `go build` compiles) so
mutually-exclusive build-constrained files like the worker's
`specfile_unix.go`/`specfile_other.go` are not both type-checked) — the
four-kind regression coverage above exists, the fixer-prompt builder coverage
test (step 9) exists — exercising `AppendFixerCompletionInstruction` for
advertised-subset coverage of the advertised `failure_kind` tokens via set
equality plus per-bullet description pairing (the builder derives the values
itself from the `policy` import, so there is no call site to swap; the pairing
catches a builder-side field-to-bullet swap the derivation cannot prevent), with
`internal/agent/prompt_test.go` importing the stdlib-only `policy` leaf and no
`failureclass` import, and the earlier cross-component
`TestFixerPromptOffersOnlyHonoredFailureKinds` split — its prompt-sync half
deleted (now compile-time) and its `parseFixerBlockedFailureKind` acceptance
half retained as a focused fixer-package test so the parser's
`retryable_after_resume` backward-compatible acceptance stays directly
asserted — the full `go test ./...` suite is green, and the diff contains no
changes to `workflow.Step` types, `failureclass.Classify` logic, or
`NormalizeResumePolicy` behavior.
