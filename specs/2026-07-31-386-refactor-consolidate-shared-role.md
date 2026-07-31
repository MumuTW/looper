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

1. **One authority for the failure-kind vocabulary.** `failureclass.Kind`
   becomes the single source for the role runners; the per-role
   `QueueFailureKind` is removed. `internal/validation`'s three `FailureKind*`
   constants are deleted, `Policy.FailureKind` is typed `failureclass.Kind`
   (not `string`), and `PolicyFor` returns the shared constants directly with no
   `string()` cast: every production importer of `internal/validation`
   (`internal/fixer`, `internal/fixer/failurepolicy`, and `internal/worker`)
   already imports `failureclass`, and `failureclass` does not import
   `validation`, so adding the import to `validation` introduces no cycle and no
   transitive dependency that `validation`'s importers do not already carry; the
   two production consumers (`worker.classifyValidationFailure` and
   `fixer/failurepolicy.ClassifyValidation`) delete their string→kind casts. The
   remaining package that spells the vocabulary as bare `string` constants —
   `internal/loops/policy` — becomes the *owner* of the kind string values
   rather than a second spelling pinned by a gate: `policy` is an intentional
   stdlib-only leaf (its package doc states this) so that `reviewer/workflow` can
   depend on it without pulling in the infra stack `failureclass` carries, and
   the dependency runs in the opposite direction — `failureclass` imports
   `policy` (no cycle: `policy` imports only the standard library) and derives
   its four `Kind` constants from `policy`'s string constants at compile time
   (Step 5). `policy` gains the two kind constants it currently lacks
   (`FailureKindRetryableTransient`, `FailureKindNonRetryable`) so it owns all
   four string values; `failureclass.Kind` stays `type Kind string` and
   `Classify`'s public API is unchanged, but each `failureclass` constant is now
   `const RetryableTransient Kind = policy.FailureKindRetryableTransient` (an
   untyped-string-to-`Kind` constant conversion), so a rename in `policy`
   propagates to `failureclass` and to every runner at compile time. This deletes
   the second spelling rather than retaining both ledgers plus a drift gate,
   per the repo's "prefer deletion over another layer" guideline. The fixer prompt's
   advertised literals (`internal/agent/prompt.go`) are no longer a separate
   spelling: the fixer (which already imports `failureclass`) passes the
   advertised kind values into `AppendFixerCompletionInstruction` as `string`
   parameters, so the `failure_kind` tokens are derived from
   `string(failureclass.*)` at the call site (Step 6) without `internal/agent`
   importing `failureclass` — `failureclass` imports `internal/infra/github`,
   which `internal/agent` does not currently reach, and coupling a generic
   prompt-construction package to the infrastructure-backed classifier would
   pull that stack into every build and test of `internal/agent`. This deletes
   the second spelling rather than pinning it by import; the call site's argument
   order is then pinned by a cross-component fixer test (Step 6) that exercises
   the production prompt builder and asserts the advertised tokens match the
   kinds `parseFixerBlockedFailureKind` honors, so swapped or unrelated `string`
   arguments at `runner.go:7307` fail the suite rather than compiling silently.
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
constants, type `Policy.FailureKind` as `failureclass.Kind` (not `string`), and
have `PolicyFor` return the shared constants directly with no `string()` cast.
Today
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
`string(failureclass.*)` in `PolicyFor` would preserve an untyped boundary where
a typo or future unsupported value still compiles and can reach retry or
persistence logic, and would force both consumers to keep their string→kind
casts — an authority converted to a string only to be converted back. Typing the
field `failureclass.Kind` deletes that boundary: the constants are deleted,
`PolicyFor` returns the shared constants directly, and both consumers drop their
casts. `Policy` is an in-memory struct consumed only by those two call sites
plus same-package tests (it is not persisted or JSON-marshaled), so widening the
field type breaks no persistence or string-context consumer.

```go
type Policy struct {
    FailureKind  failureclass.Kind
    ResumePolicy string
}

func PolicyFor(category FailureCategory) Policy {
    switch category {
    case FailureContextCanceled:
        return Policy{FailureKind: failureclass.RetryableAfterResume, ResumePolicy: loops.ResumePolicyReplayStep}
    case FailureSupervisorTimeout, FailureInfrastructure:
        return Policy{FailureKind: failureclass.RetryableTransient, ResumePolicy: loops.ResumePolicyReplayStep}
    case FailureNonZeroExit:
        return Policy{FailureKind: failureclass.ManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention}
    default:
        return Policy{FailureKind: failureclass.ManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention}
    }
}
```

The two consumers delete their casts:
`worker.classifyValidationFailure` becomes `kind: policy.FailureKind` (was
`QueueFailureKind(policy.FailureKind)`, itself becoming
`failureclass.Kind(policy.FailureKind)` after Step 1 renames the type — a
redundant cast once the field is already `failureclass.Kind`), and
`fixer/failurepolicy.ClassifyValidation` becomes `Kind: policy.FailureKind` (was
`failureclass.Kind(policy.FailureKind)`). The two same-package tests that spell
the constant names are updated to compare against the shared values instead:
`internal/validation/validation_test.go`'s
`TestRunCommandsKeepsPolicyWordsDiagnosticForNonZeroExit` compares
`policy.FailureKind` to `FailureKindManualIntervention`, and
`TestPolicyForOperationalFailureCategory` table-tests all three; both are
rewritten to expect `failureclass.ManualIntervention`,
`failureclass.RetryableAfterResume`, and `failureclass.RetryableTransient`
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
redundant casts. Because `Policy.FailureKind` is now `failureclass.Kind`, a
rename of any `failureclass.Kind` value propagates to `PolicyFor` and to both
consumers at compile time, and no validation drift test is needed. Adding the
`failureclass` import to `validation` is safe: every production importer of
`validation` (`internal/fixer`, `internal/fixer/failurepolicy`,
`internal/worker`) already imports `failureclass`, and `failureclass` does not
import `validation`, so no cycle is created and no importer gains a transitive
dependency it did not already carry.

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
prove that the seven former `xxxFailureKind` call sites actually invoke
`Normalize`. The seven sites are the only `loopError.kind` assignments that
receive a *dynamic* `failureclass.Kind` — four in the fixer
(`runner.go:3378,3403,3650,6480`, taking `d.Kind` / `failure.Kind` from a
`Decision` or validation failure) and one each in the reviewer, worker, and
planner (`runner.go:4640/3403/1936`, taking `failureclass.Classify(...)`
directly). Every other `loopError.kind` assignment spells a named
`FailureRetryable*` constant (a known kind that needs no normalization). If an
implementation replaces any of the seven dynamic-source calls with a direct
assignment (`kind: d.Kind` or `kind: failureclass.Classify(...)` without
`Normalize`), the `Normalize` unit test, the four-kind runner tests, staticcheck,
and the deleted-symbol search (Step 7) all still pass, while a future unknown
kind no longer falls back to `non_retryable`. A structural check (Validation
step 8) ties every former conversion site to `failureclass.Normalize`: it scans
the `kind:` assignments in the four runners for a dynamic source
(`failureclass.Classify` or a `.Kind` field access) and fails any line that does
not also contain `failureclass.Normalize`, so a direct-assignment bypass of the
fallback fails the suite rather than compiling silently.

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

### Step 5 — Reverse the leaf dependency: `failureclass` derives its constants from `policy`

One package outside `failureclass` still spells part of the kind vocabulary as
bare `string` constants: `internal/loops/policy` re-declares two of the four
kind strings (`FailureKindRetryableAfterResume`, `FailureKindManualIntervention`)
as plain untyped `string` constants. The earlier rationale for leaving that as a
second spelling pinned by a drift test considered only one dependency direction
— making `policy` import the heavyweight `failureclass` (which would break
`policy`'s stdlib-only leaf property). The dependency runs in the opposite
direction with no cycle: `policy` imports only the standard library, so
`failureclass` can import `policy` and derive its `Kind` constants from `policy`'s
string constants at compile time. That deletes the second spelling and the
synchronization test entirely, rather than retaining both ledgers plus a new
gate that leaves correctness dependent on the full suite running — the simpler
deletion the repo's "prefer deletion over another layer" guideline requires
before adding a gate.

`policy` becomes the single owner of the kind *string values*. It gains the two
constants it currently lacks so it owns all four:

```go
const (
	FailureKindRetryableTransient   = "retryable_transient"
	FailureKindRetryableAfterResume = "retryable_after_resume"
	FailureKindNonRetryable         = "non_retryable"
	FailureKindManualIntervention   = "manual_intervention"
	// ... existing ResumePolicy* constants unchanged ...
)
```

`failureclass` imports `policy` and re-derives its four `Kind` constants from
`policy`'s untyped string constants (an untyped-string-to-`Kind` constant
conversion, valid because `Kind` is `type Kind string`):

```go
package failureclass

import "github.com/MumuTW/looper/internal/loops/policy"

type Kind string

const (
	RetryableTransient   Kind = policy.FailureKindRetryableTransient
	RetryableAfterResume Kind = policy.FailureKindRetryableAfterResume
	NonRetryable         Kind = policy.FailureKindNonRetryable
	ManualIntervention   Kind = policy.FailureKindManualIntervention
)
```

`Classify`'s public API is unchanged (it still returns `Kind`), and `Kind` stays
`type Kind string`, so every `failureclass.Kind` surface — the runners, the
scheduler field, `loopError.kind`, `parseFixerBlockedFailureKind`'s return type,
`Policy.FailureKind` — keeps compiling unchanged. A rename of any `policy`
string constant propagates to `failureclass` and to every consumer at compile
time, so no drift-detection test is added or needed. `policy`'s leaf property is
preserved: it still imports only the standard library, so `reviewer/workflow`
depending on `policy` still pulls in no infra. Adding `policy` to
`failureclass`'s import graph adds only a stdlib-only package to
`failureclass`'s existing `internal/infra/github` closure, and every runner
already imports `policy` transitively via `internal/loops`, so no importer of
`failureclass` gains a transitive dependency it did not already carry.

`NormalizeResumePolicy` and the other `policy` predicates keep their current
`string` signatures and branches: the `policy` constants stay untyped `string`,
so they remain assignable to the `string` parameters those functions take. No
`policy` function changes.

(`internal/validation` also re-declared the same vocabulary as bare `string`
constants, but Step 1 deletes those constants, types `Policy.FailureKind` as
`failureclass.Kind`, and has `PolicyFor` return the shared constants directly,
deleting that ledger outright; see Step 1 for the no-cycle justification. No
validation drift test is added or needed.)

### Step 6 — Derive the fixer prompt's advertised literals from `failureclass`

`internal/agent/prompt.go:55-57` (`AppendFixerCompletionInstruction`) embeds
`retryable_transient` and `manual_intervention` as string literals in the prompt
that tells the fixer agent which `failure_kind` values it may report. That prompt
is the advertising layer for `parseFixerBlockedFailureKind`: the parser bounds
what the agent returns, but the prompt tells the agent what to return. The
literals are not derived from `failureclass`, so a rename of
`failureclass.RetryableTransient` or `failureclass.ManualIntervention` would make
the parser (which reads the `FailureRetryable*` constants) follow the new value
while the prompt keeps advertising the old one — valid blocked outcomes become
contract failures. The existing `internal/agent/prompt_test.go` asserts the
literals too, not the shared constants, so it would not catch the divergence
either.

`internal/agent` must not import `failureclass` to derive these tokens.
`failureclass` imports `internal/infra/github` directly, and `internal/agent`
does not currently reach `internal/infra/github` or the rest of `failureclass`'s
transitive closure (`internal/diffanchor`, `internal/outboundguard`); the
config/storage overlap `internal/agent` already carries does not make those new
edges a no-op. Coupling a generic prompt-construction package to the
infrastructure-backed classifier would pull the GitHub infrastructure stack into
every build and test of `internal/agent`. Instead, the advertised values are
passed into the prompt builder from its caller: `internal/fixer` already imports
both `internal/agent` and `internal/loops/failureclass`, so it derives the tokens
at the call site and hands them to `AppendFixerCompletionInstruction` as `string`
parameters. The prompt is built with `strings.Join`, so the advertised tokens are
constructed from the received parameters rather than spelled as independent
literals. A rename of either shared value updates the call site at compile time,
the same way it updates the parser, so the two cannot silently diverge — that
rename-drift layer is deleted, not pinned by a synchronization test. The
compile-time link does not, however, constrain argument order: because
`AppendFixerCompletionInstruction` accepts unconstrained `string` parameters,
swapped or unrelated arguments at `runner.go:7307` would still compile and
advertise values that diverge from `parseFixerBlockedFailureKind`. That call-site
argument order is pinned by a cross-component fixer test (Step 6) that exercises
the production prompt builder and asserts the advertised tokens match the kinds
`parseFixerBlockedFailureKind` honors, rather than by a synchronization test
guarding a second spelling. This removes the second spelling rather than pinning
it, per the repo's "prefer deletion over another layer" guideline — a
synchronization test would only guard a ledger that no longer exists, and would
leave production correctness dependent on that test running. `internal/agent`
stays a lightweight package that does not import `failureclass` or the infra
stack.

`AppendFixerCompletionInstruction` advertises each `failure_kind` value in two
distinct forms; both are reconstructed from the received parameters so a rename
that updates one form but misses the other is a compile error at the call site,
not a silent drift:

1. The quoted bullet tokens `- "retryable_transient":` and
   `- "manual_intervention":` (a leading `- `, a double-quoted value, then `:`)
   are built as `"- " + strconv.Quote(retryableTransient) + ":"` and the same
   for `manualIntervention`, embedded in the joined slice. The prompt
   intentionally advertises only those two (not `retryable_after_resume`, which
   the parser still accepts — see the comment at
   `parseFixerBlockedFailureKind`), so only the advertised subset is passed.
2. The blocked-completion example at `internal/agent/prompt.go:57`
   (`{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`)
   independently embeds `manual_intervention` as a `"failure_kind":"<value>"`
   occurrence. That example is a separate spelling of the same vocabulary: if
   `ManualIntervention` is renamed and the bullet token is updated but the
   example is missed, the prompt keeps demonstrating a value that
   `parseFixerBlockedFailureKind` would reject after the rename. The example is
   therefore reconstructed with
   `"\"failure_kind\":" + strconv.Quote(manualIntervention)` so the same rename
   updates it.

The call site in `internal/fixer/runner.go:7307` (inside `buildFixerPrompt`)
becomes
`agent.AppendFixerCompletionInstruction(strings.Join(parts, "\n\n"), string(failureclass.RetryableTransient), string(failureclass.ManualIntervention))`;
the two `string(...)` casts are the compile-time link to the shared authority for
renames, but they do not constrain argument order.

The existing `internal/agent/prompt_test.go` is rewritten to use arbitrary
sentinel `string` parameters — e.g.
`AppendFixerCompletionInstruction("repair the pr", "sentinel-transient", "sentinel-manual")`
— and to assert the prompt embeds exactly those received values in both forms
(the quoted bullet tokens `"- " + strconv.Quote(sentinel) + ":"` for each, and
the blocked-completion example built from the manual sentinel). It does **not**
import `failureclass`: a test-only import would make `go test ./internal/agent`
compile `failureclass` and its `internal/infra/github`, `diffanchor`, and
`outboundguard` dependency tree — the exact test coupling Step 6 is avoiding. The
sentinel parameters prove the builder is parametric (it embeds what it receives,
not independent literals) without pulling the infra stack into the agent test
closure.

The real failure-kind values are derived from `failureclass` in the existing
cross-component fixer test `TestFixerPromptOffersOnlyHonoredFailureKinds`
(`internal/fixer/runner_repair_outcome_test.go`), which already imports
`failureclass` in production. That test is rewritten to exercise the production
prompt-building path rather than call `AppendFixerCompletionInstruction`
directly: it invokes `buildFixerPrompt` (the function containing the
`runner.go:7307` call site) and asserts the produced prompt offers exactly the
`failure_kind` values that `parseFixerBlockedFailureKind` honors —
`string(failureclass.RetryableTransient)` and
`string(failureclass.ManualIntervention)` as advertised bullets, and not
`retryable_after_resume` (still accepted on input, not advertised).

The argument-order coverage the test claims depends on asserting each value in
its semantic position, not just the bullet-token set: the two `string` arguments
at `runner.go:7307` are the same type, so a swap still yields a prompt whose
bullet-token set is exactly `{retryable_transient, manual_intervention}` and
still omits `retryable_after_resume` — a set-only assertion passes despite
attaching the retryable description to the manual kind and the manual
description to the transient kind. The test therefore asserts each advertised
bullet together with its corresponding description: the
`- "retryable_transient":` bullet is followed by the retryable description
("another attempt at the repair could succeed"), and the
`- "manual_intervention":` bullet is followed by the manual description ("no
retry can succeed without a human decision"). A swapped call site attaches the
wrong description to each token and fails the paired assertion. The test
additionally asserts the blocked-completion example
`{"outcome":"blocked","failure_kind":"<value>",...}` embeds
`string(failureclass.ManualIntervention)` (not the transient value), so using
the transient kind in the example is caught too. With both the per-bullet
description pairing and the example-kind assertion in place, swapped or
unrelated `string` arguments at `runner.go:7307` produce a prompt that fails the
assertion rather than compiling silently. A rename of either shared value
updates the call site and the test's expected tokens together at compile time,
and a removal of either advertised bullet is caught because the test asserts
both.

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
- **Generalize the step traversal helpers.** See Step 4 — rejected as net
  negative.
- **Pin `policy`'s kind constants to `failureclass` by a drift-detection test.**
  Considered, rejected. The earlier rationale considered only one dependency
  direction — making `policy` import `failureclass`, which would break `policy`'s
  stdlib-only leaf property. The dependency runs the other way with no cycle
  (`policy` imports only the standard library), so `failureclass` can import
  `policy` and derive its `Kind` constants from `policy`'s string constants at
  compile time (Step 5). A drift gate would retain both ledgers and leave
  correctness dependent on the full suite running, against the repo's "prefer
  deletion over another layer" guideline; the compile-time derivation deletes the
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
`Policy.FailureKind` as `failureclass.Kind` and having `PolicyFor` return the
shared constants directly in Step 1). It is deliberately not a repo-wide audit
of every bare-string spelling of the vocabulary. The remaining spellings split
into two groups: those derived from the shared constants at compile time (caught
by import, no drift test), and those explicitly out of scope (not caught at all).
No spelling is pinned by a drift-detection test after Step 5 reverses the leaf
dependency: `policy` owns the kind string values and `failureclass` derives from
it by import, so the former "pinned by test" group is deleted, not retained.

**Derived from the shared constants (caught at the call site by import, no drift
test):**

1. `internal/loops/policy`'s kind constants become the single owner of the kind
   *string values*. `policy` gains the two constants it currently lacks
   (`FailureKindRetryableTransient`, `FailureKindNonRetryable`) so it owns all
   four, and `failureclass` imports `policy` and derives its four `Kind`
   constants from `policy`'s untyped string constants at compile time (Step 5).
   The dependency runs in that direction because `policy` is an intentional
   stdlib-only leaf (its package doc states this) so `reviewer/workflow` can
   depend on it without pulling in the `internal/infra/github` stack
   `failureclass` carries; `failureclass` importing `policy` adds no cycle
   (`policy` imports only the standard library) and no transitive dependency any
   `failureclass` importer did not already carry. A rename of any `policy` string
   constant propagates to `failureclass` and to every runner at compile time, so
   no drift-detection test is added. Resume-policy behavior is unchanged: the
   `policy` constants stay untyped `string`, and `NormalizeResumePolicy` keeps
   its current signature and branches.

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
   build and test of `internal/agent`. Instead, Step 6 has the fixer (which
   already imports `failureclass`) pass `string(failureclass.RetryableTransient)`
   and `string(failureclass.ManualIntervention)` into
   `AppendFixerCompletionInstruction` as `string` parameters, and the prompt
   builder embeds the received values instead of independent literals. A rename
   of either shared value updates the call site at compile time, the same way it
   updates the parser, so the two cannot silently diverge. The call site's
   argument order is pinned by a cross-component fixer test (Step 6) that
   exercises `buildFixerPrompt` and asserts the advertised tokens match the kinds
   `parseFixerBlockedFailureKind` honors, so swapped or unrelated `string`
   arguments fail the suite; the second spelling no longer exists in production,
   so no synchronization test guarding a duplicate ledger is added.

(`internal/validation`'s three `FailureKind*` constants previously belonged in a
separate-spelling group; Step 1 now deletes them, types `Policy.FailureKind` as
`failureclass.Kind`, and has `PolicyFor` return the shared constants directly,
so a rename propagates at compile time to `PolicyFor` and to both consumers
(`worker.classifyValidationFailure` and `fixer/failurepolicy.ClassifyValidation`,
which delete their string→kind casts). No drift test is needed.)

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
  the parser bounds what is accepted — and is derived from `failureclass` in
  Step 6 (the fixer passes `string(failureclass.*)` into
  `AppendFixerCompletionInstruction`, which embeds the received values), so a
  rename updates the prompt and the parser at compile time and the two cannot
  silently diverge.

Infra signals remain for drift detection, not authority.

## Impact

**Files changed (production):**
- `internal/fixer/runner.go` — delete `QueueFailureKind` + delete `fixerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits, including the `AppendFixerCompletionInstruction` call at `runner.go:7307` now passing `string(failureclass.RetryableTransient)` and `string(failureclass.ManualIntervention)` (Step 6).
- `internal/reviewer/runner.go` — delete `QueueFailureKind` + delete `reviewerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/worker/runner.go` — delete `QueueFailureKind` + delete `workerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/planner/runner.go` — delete `QueueFailureKind` + delete `plannerFailureKind` + `s/QueueFailureKind/failureclass.Kind/` + call-site edits.
- `internal/runtime/scheduler.go` — `workerRunCompletedNotificationInput.FailureKind` becomes `failureclass.Kind` (the one `QueueFailureKind` reference here).
- `internal/loops/failureclass/failureclass.go` — add `Normalize(kind Kind) Kind` (the authority for the unknown-kind fallback); add the `internal/loops/policy` import and re-derive the four `Kind` constants from `policy`'s string constants at compile time (Step 5), deleting the second spelling rather than pinning it by test; `Classify` logic and public API unchanged.
- `internal/validation/validation.go` — delete the three `FailureKind*`
  constants, type `Policy.FailureKind` as `failureclass.Kind`, have `PolicyFor`
  return the shared constants directly (no `string()` cast), and add the
  `failureclass` import; the two same-package tests that spell the constant
  names are updated to compare against the typed `failureclass.*` constants —
  see the call-site audit in Step 1.
- `internal/worker/runner.go` (additional edit beyond the type rename) —
  `classifyValidationFailure` drops the `QueueFailureKind(policy.FailureKind)`
  cast and assigns `kind: policy.FailureKind` directly, now that the field is
  `failureclass.Kind`.
- `internal/fixer/failurepolicy/policy.go` — `ClassifyValidation` drops the
  `failureclass.Kind(policy.FailureKind)` cast and assigns
  `Kind: policy.FailureKind` directly, now that the field is `failureclass.Kind`.
- `internal/agent/prompt.go` — `AppendFixerCompletionInstruction` takes the
  advertised `failure_kind` values as `string` parameters and embeds them (both
  the quoted bullet tokens and the blocked-completion example) instead of
  independent literals; it does not import `failureclass`, keeping
  `internal/agent` decoupled from the infrastructure-backed classifier. The
  call site in `internal/fixer/runner.go` passes
  `string(failureclass.RetryableTransient)` and
  `string(failureclass.ManualIntervention)`, so a rename of either shared value
  updates the prompt at compile time, and the call site's argument order is
  pinned by a cross-component fixer test (Step 6).
- `internal/loops/policy/policy.go` — gains two new untyped `string` constants
  (`FailureKindRetryableTransient`, `FailureKindNonRetryable`) so it owns all
  four kind string values; its existing two constants, `NormalizeResumePolicy`,
  and the other predicates keep their current `string` signatures and branches
  (no behavior change). `policy` stays a stdlib-only leaf (the new constants add
  no import), so `reviewer/workflow` depending on it still pulls in no infra. No
  drift test is added: `failureclass` imports `policy` and derives its `Kind`
  constants from these strings at compile time (Step 5), so a rename propagates
  by import, not by a synchronization gate.

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
a four-kind net that does not yet exist. No policy drift-detection test is
added: Step 5 reverses the leaf dependency so `failureclass` imports `policy`
and derives its `Kind` constants from `policy`'s strings at compile time, and no
validation drift test is added because Step 1 deletes the validation constants,
types `Policy.FailureKind` as `failureclass.Kind`, and has `PolicyFor` return
the shared constants directly (a rename propagates at compile time in both
cases). The fixer-prompt call path is covered by a
cross-component test: `TestFixerPromptOffersOnlyHonoredFailureKinds`
(`internal/fixer/runner_repair_outcome_test.go`) is rewritten to exercise
`buildFixerPrompt` and assert the produced prompt offers exactly the
`failure_kind` values `parseFixerBlockedFailureKind` honors (derived from
`failureclass`), so swapped or unrelated `string` arguments at the
`runner.go:7307` call site fail the suite. The existing
`internal/agent/prompt_test.go` is rewritten to use arbitrary sentinel `string`
parameters and assert the builder embeds the received values in both forms; it
does not import `failureclass`, keeping `go test ./internal/agent` free of the
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
   conversion functions are deleted.
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
8. **Normalize call-site structural check (Step 2).** The `Normalize` unit test
   and the four-kind runner tests do not prove the seven former `xxxFailureKind`
   call sites actually invoke `failureclass.Normalize` — a direct `kind: d.Kind`
   or `kind: failureclass.Classify(...)` assignment bypasses the fallback but
   still passes those tests, staticcheck, and the deleted-symbol search (step 7).
   The seven sites are the only `loopError.kind` assignments that receive a
   *dynamic* `failureclass.Kind` (a `.Kind` field access or a `Classify` call);
   every other `loopError.kind` assignment spells a named `FailureRetryable*`
   constant and needs no normalization. A structural search fails when any
   `kind:` assignment in the four runners carries a dynamic source but does not
   also contain `failureclass.Normalize`, so a direct-assignment bypass of the
   unknown-kind → `non_retryable` fallback fails the suite rather than compiling
   silently:

   ```shell
   #!/usr/bin/env bash
   set -euo pipefail

   # kind: lines that receive a dynamic failureclass.Kind (a .Kind field access
   # or a failureclass.Classify call) but do not route it through Normalize.
   if rg -n 'kind:.*(failureclass\.Classify|\w+\.Kind)\b' \
        internal/fixer/runner.go internal/reviewer/runner.go \
        internal/worker/runner.go internal/planner/runner.go \
        | rg -v 'failureclass\.Normalize'
   then
     echo "loopError.kind receives a dynamic failureclass.Kind without failureclass.Normalize" >&2
     exit 1
   fi
   ```

   (Step 5's reverse-dependency derivation — `failureclass` importing `policy`
   and re-deriving its `Kind` constants from `policy`'s strings — needs no
   separate validation step: it is a compile-time constant derivation, so a
   rename in `policy` that `failureclass` does not follow is a `go build` failure
   covered by step 1. No policy or validation drift test is added or needed.)
9. **Fixer-prompt call-site coverage (Step 6).**
   `TestFixerPromptOffersOnlyHonoredFailureKinds`
   (`internal/fixer/runner_repair_outcome_test.go`) is rewritten to exercise
   `buildFixerPrompt` and assert the produced prompt offers exactly
   `string(failureclass.RetryableTransient)` and
   `string(failureclass.ManualIntervention)` as advertised bullets (and not
   `retryable_after_resume`), matching the kinds `parseFixerBlockedFailureKind`
   honors. The assertion pairs each advertised bullet with its corresponding
   description (the `retryable_transient` bullet with the retryable description,
   the `manual_intervention` bullet with the manual description) and asserts the
   blocked-completion example embeds `string(failureclass.ManualIntervention)`,
   not the transient value — a set-only check would pass under swapped same-typed
   arguments, so the per-bullet pairing and example-kind assertion are what make
   swapped or unrelated `string` arguments at `runner.go:7307` fail the suite.
   `internal/agent/prompt_test.go` is rewritten to use arbitrary
   sentinel `string` parameters (no `failureclass` import) and assert the
   builder embeds the received values in both forms, keeping
   `go test ./internal/agent` free of the infra stack.

**Definition of done:** `QueueFailureKind` is gone from all four runners (the 60
type-name references replaced by `failureclass.Kind`), the four `xxxFailureKind`
functions are gone and `failureclass.Normalize` preserves their unknown-kind →
`non_retryable` fallback, `internal/validation`'s `FailureKind*` constants are
deleted, `Policy.FailureKind` is typed `failureclass.Kind`, and `PolicyFor`
returns the shared constants directly (with both consumers'
string→kind casts deleted), `internal/loops/policy` owns all four kind string
constants and `failureclass` imports `policy` and derives its `Kind` constants
from them at compile time (no policy or validation drift test), the fixer
prompt's advertised `failure_kind` tokens are derived from
`string(failureclass.*)` passed into `AppendFixerCompletionInstruction` from the
fixer call site, the deleted-symbol absence check (step 7) passes, the
Normalize call-site structural check (step 8) passes — so every former
`xxxFailureKind` call site routes its dynamic `failureclass.Kind` through
`failureclass.Normalize` and a direct-assignment bypass fails the suite — the
four-kind regression coverage above exists, the fixer-prompt call-site coverage
test (step 9) exists — exercising `buildFixerPrompt` to catch swapped or
unrelated `string` arguments at the `runner.go:7307` call site, with
`internal/agent/prompt_test.go` using sentinel parameters and no `failureclass`
import — the full `go test ./...` suite is green, and the diff contains no
changes to `workflow.Step` types, `failureclass.Classify` logic, or
`NormalizeResumePolicy` behavior.
