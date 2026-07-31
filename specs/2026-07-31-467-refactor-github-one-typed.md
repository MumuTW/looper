# Spec: One typed mergeability-state model shared by Gatekeeper and mergewatch (#467)

- **Issue:** MumuTW/looper#467 — refactor(github): one typed mergeability-state model shared by Gatekeeper and mergewatch
- **Base branch:** `main`
- **Type:** Refactor of the GitHub mergeability-state boundary; no behavior change
- **Refs:** #458, #459, #460, #464 (all add state-keyed logic — the third/fourth copy this prevents), #386 (consolidating shared role types, same spirit)

## Problem

GitHub's mergeability vocabulary reaches the daemon as a raw string and is pattern-matched independently in two packages, against two hand-rolled subsets of the same undocumented magic strings (`"clean"`, `"dirty"`, `"blocked"`, `"unstable"`, `"has_hooks"`, `"unknown"`, plus the GraphQL-only `"behind"`).

- **Gatekeeper** (`internal/gatekeeper/runner.go:336-347`): lowercases + trimming at the consumer, then `"dirty"` → `ReasonMergeConflict`; `providerPolicyStateIsAmbiguous` (`runner.go:643-650`: `"blocked"`, `"unstable"`, `"has_hooks"`) → `ReasonProviderStateAmbiguous`; anything else non-`"clean"` → `ReasonMergeabilityNotClean`. The `Mergeable == nil || state == "" || state == "unknown"` branch (`:338`) is a separate ambiguity path.
- **Coordinator mergewatch** (`internal/coordinator/mergewatch/mergewatch.go:90,102` and `internal/coordinator/mergewatch_runner.go:184-219`): lowercases + trimming at the consumer (`mergewatch_runner.go:184`), then compares against `"unknown"` (`:90`), `"dirty"` (`:102`), and `"unstable"` (`:205,216`) with its own action mapping. `Classify` (`mergewatch.go:73`) receives the already-normalized string on `PRSnapshot.MergeableState`.

Two packages, two normalizations, two subsets, no shared source of truth. Issues #458–#464 each add logic keyed on these states, which without this refactor means a third and fourth copy of the same string vocabulary. Divergence here is exactly the class of bug the compiler should catch — the invariant belongs in a type, not in each consumer's memory of GitHub's vocabulary. The raw string also leaks through two transport shapes: the REST `mergeable_state` field (lowercase) and the GraphQL `mergeStateStatus` field (uppercase: `CLEAN`, `DIRTY`, `BLOCKED`, `UNSTABLE`, `HAS_HOOKS`, `UNKNOWN`, `BEHIND`). The gateway already collapses these at `gateway.go:1731` (`firstNonEmpty(mergeable_state, mergeStateStatus)`), so a consumer can receive either casing; today each consumer re-lowercases to defend against that.

## Goals

- A typed `MergeabilityState` in the GitHub infra layer (`internal/infra/github`), parsed **once at the gateway boundary** from the raw string, with an explicit `Unknown`/unrecognized case that preserves the original string as evidence.
- Semantic predicates on the type — `IsClean()`, `HasConflict()`, `IsAmbiguousPolicyState()`, `IsUnknown()` — so consumers state intent instead of comparing strings. The predicate set is the *union* of what Gatekeeper and mergewatch currently ask, no more.
- Gatekeeper and mergewatch consume the type; the raw string survives only inside evidence/telemetry fields for auditability.
- Exhaustive switch coverage in the parser so a newly observed provider state fails loudly in one place instead of silently falling through two consumers.
- Behavior unchanged: the existing Gatekeeper reason mapping and mergewatch action mapping are preserved exactly, covered by regression tests against the shared type.

## Non-goals

- Changing *what* each consumer does with a given state. The reason/action mapping tables stay as-is; only the dispatch mechanism (string compare → typed predicate) changes.
- Touching the GraphQL `mergeStateStatus` → `HasConflicts` probe (`gateway.go:803,839,1680`). That is a separate boolean derived from `DIRTY` for discovery-list fast paths and is not part of the mergeability-state consumer surface this issue targets. It is noted only because the typed parser must accept the uppercase GraphQL vocabulary too.
- Adding new states, new predicates, or new gates. This is a consolidation, not an expansion. #458–#464 add the new state-keyed logic; this refactor only gives them one place to hang it.
- Persisting the typed enum in `Evidence`/eventlog. The durable `Evidence.MergeableState` field stays a string for backward-compatible audit reads; the type is an in-memory dispatch boundary, not a new persisted concept.

## Authority

> **What is the authority for this action, and why is it not the agent's own structured output?**

This refactor adds no gate, validation, or persisted field — it moves an existing string-to-decision mapping into a type. The authority for "what does state X mean" is GitHub's documented `mergeable_state` / `mergeStateStatus` vocabulary, parsed once at the gateway where the raw provider response enters the process. Consumers stop re-deriving that meaning locally. There is no agent output in this path, so the agent-output trust question does not arise.

## Approach

### 1. The type — `internal/infra/github/mergeability.go`

A new small file in the existing `internal/infra/github` package (no new package — the gateway already lives here and is the parse site).

```go
type MergeabilityState struct {
    kind  MergeabilityKind
    raw   string // original string, preserved for evidence/telemetry
}

type MergeabilityKind int

const (
    MergeabilityClean MergeabilityKind = iota
    MergeabilityDirty        // conflict
    MergeabilityBlocked      // policy blocker, ambiguous
    MergeabilityUnstable     // failing/pending required checks, ambiguous
    MergeabilityHasHooks     // server-side hooks pending, ambiguous
    MergeabilityBehind       // GraphQL-only; head behind base
    MergeabilityUnknown      // provider has not computed, or unrecognized string
)
```

- `ParseMergeabilityState(raw string) MergeabilityState` is the single parse site. It trims, lowercases, and switches exhaustively over the known vocabulary. The `default` arm sets `MergeabilityUnknown` and keeps the original `raw` so an unrecognized provider value is auditable, not silently re-classified.
- Predicates: `IsClean()`, `HasConflict()`, `IsAmbiguousPolicyState()` (true for `Blocked`/`Unstable`/`HasHooks` — the existing `providerPolicyStateIsAmbiguous` set), `IsUnknown()`, `Raw() string` (returns the preserved original string for evidence).
- No `Behind` predicate beyond the kind: Gatekeeper currently routes `behind` to `ReasonMergeabilityNotClean` (the "not clean, not ambiguous, not conflict" fall-through), and mergewatch does not special-case it. `Behind` gets a kind so the parser is exhaustive and the value is auditable, but no consumer predicate is added until #458–#464 need one. Adding the kind now is what makes the switch exhaustive; adding a predicate now would be speculative.

### 2. Gateway parse site — `internal/infra/github/gateway.go`

- `PullRequestDetail.MergeableState` changes type from `string` to `MergeabilityState`. The two assignment sites (`gateway.go:1686` for the GraphQL-shaped view, `:1731` for the REST `ViewPullRequestMergeWatch` used by both Gatekeeper and mergewatch) call `ParseMergeabilityState(...)` instead of storing the raw string. Case normalization moves out of the consumers and into the parser.
- `HasConflicts` (`:803,839,1680`) is left untouched — it is a separate GraphQL-only boolean, not a mergeability-state consumer.

### 3. Gatekeeper — `internal/gatekeeper/runner.go`

- `Evidence.MergeableState` stays `string` for durable audit reads; it is populated from `state.Raw()` so the persisted report is byte-identical to today.
- The `:336-347` block becomes predicate calls on the parsed state:
  - `Mergeable == nil || state.IsUnknown()` → `ReasonProviderStateAmbiguous` (today's `state == "" || state == "unknown"`; the empty-string case maps to `Unknown` in the parser).
  - `!*Mergeable || state.HasConflict()` → `ReasonMergeConflict`.
  - `state.IsAmbiguousPolicyState()` → `ReasonProviderStateAmbiguous` with `Subject: "mergeability:" + state.Raw()`.
  - `!state.IsClean()` (the remaining fall-through, covering `Behind` and any future non-clean kind) → `ReasonMergeabilityNotClean` with `Subject: state.Raw()`.
- `providerPolicyStateIsAmbiguous` (`:643-650`) is deleted — its logic moves into `MergeabilityState.IsAmbiguousPolicyState()`.

### 4. mergewatch — `internal/coordinator/mergewatch/mergewatch.go` + `mergewatch_runner.go`

- `PRSnapshot.MergeableState` changes type from `string` to `MergeabilityState`. The snapshot builders in `mergewatch_runner.go` (`:184` full snapshot, `:253` partial snapshot) call `ParseMergeabilityState(...)` once, dropping their local `strings.ToLower(strings.TrimSpace(...))`.
- `Classify` (`mergewatch.go:73`) switches to predicates:
  - `snapshot.Mergeable == nil || snapshot.MergeableState.IsUnknown()` → `ActionIndeterminate` / deadline path (`:90`).
  - `snapshot.MergeableState.HasConflict()` → `ActionConflict` (`:102`).
  - The `mergeableState == "unstable"` gating of failed-check detection (`mergewatch_runner.go:205,216`) becomes `snapshot.MergeableState.kind == MergeabilityUnstable` exposed via a `IsUnstable()` predicate (or equivalently a method on the type). This is the one place a consumer needs the specific kind rather than a coarse predicate, because `unstable` is the signal that a failing required check is a *red-CI* failure rather than a missing one. The predicate is named for the intent the consumer already has.
- The `ActionStillPending` path (`:29` test case uses `"blocked"`) is unchanged: `blocked` is not conflict, not unknown, not unstable-failed-checks, so it falls through to `StillPending` exactly as today.

### 5. Exhaustiveness

The parser's switch is exhaustive over the known vocabulary with a `default` → `Unknown`. A new GitHub state (e.g. a future `"draft"` or `"pending"`) lands in `Unknown` and is auditable via `Raw()`, rather than silently matching neither consumer's subset. This is the "fails loudly in one place" property. Consumers' predicate dispatch is over a fixed enum, so adding a `MergeabilityKind` is a compile-time signal to revisit every consumer that switches on kind (only mergewatch's `IsUnstable` check does; the rest use coarse predicates that are forward-compatible).

## Risks

- **Predicate semantics drift from string semantics.** The risk of a typed refactor is that the predicate boundaries redraw the lines the strings drew. Mitigation: the predicate set is derived *exactly* from the current string comparisons — `IsAmbiguousPolicyState()` is precisely `{"blocked","unstable","has_hooks"}`, `HasConflict()` is precisely `{"dirty"}`, `IsUnknown()` is precisely `{"", "unknown", unrecognized}`. The regression tests (below) pin the mapping for every state both consumers currently handle, so any redraw fails the test.
- **Case/casing regression.** Today each consumer lowercases; the parser must lowercase too, or an uppercase GraphQL `mergeStateStatus` fallback (`gateway.go:1731`) reaches consumers un-normalized. Mitigation: `ParseMergeabilityState` lowercases; the parser test includes uppercase inputs.
- **`Behind` over-classification.** Giving `behind` a kind could tempt a future consumer to predicate on it where today it silently falls through to `ReasonMergeabilityNotClean`. Mitigation: no `IsBehind` predicate is added in this refactor; the kind exists only for parser exhaustiveness and audit. #458–#464 add the predicate if/when they need it, with the trade-off answer in their own PR.
- **Persisted-format compatibility.** `Evidence.MergeableState` is a durable event field. Keeping it `string` (populated from `Raw()`) means existing eventlog readers see identical bytes. The type is in-memory only.

## Validation

- `go build ./...`, `go vet ./...`, staticcheck (`U1000,SA1006,SA4004,SA4006`), `go test ./...` all pass.
- **No string literals remain.** A grep for `"dirty"`, `"clean"`, `"blocked"`, `"unstable"`, `"has_hooks"`, `"unknown"`, `"behind"` (as mergeability comparisons) in `internal/gatekeeper` and `internal/coordinator` returns only test fixtures and the parser itself in `internal/infra/github`. The acceptance criterion "no string literal comparisons against mergeability values remain in `internal/gatekeeper` or `internal/coordinator`" is checked by inspection.
- **Parser unit tests** (`internal/infra/github/mergeability_test.go`): for each known lowercase string, each uppercase GraphQL equivalent, the empty string, and an unrecognized string — assert the kind, `IsClean`/`HasConflict`/`IsAmbiguousPolicyState`/`IsUnknown`, and that `Raw()` preserves the original. This is the "covering" case from AGENTS.md: a pure function extracted out of two runners and now testable at all; production surface shrinks (two normalizations + two switch sets → one parser), so test growth here is the tell, not a smell.
- **Gatekeeper regression** (`internal/gatekeeper/runner_test.go`): the existing table cases (`dirty` → `ReasonMergeConflict`, `behind` → `ReasonMergeabilityNotClean`, `blocked` → `ReasonProviderStateAmbiguous`, `unknown`/nil → `ReasonProviderStateAmbiguous`) run unchanged against the shared type. Add one case for an unrecognized state asserting it maps to `ReasonProviderStateAmbiguous` (the `IsUnknown` path) with the original string in `Evidence.MergeableState`.
- **mergewatch regression** (`internal/coordinator/mergewatch/mergewatch_test.go`): the existing `TestClassify` table (`unknown` → `ActionIndeterminate`/deadline, `dirty` → `ActionConflict`, `unstable`+failed → `ActionRedCI`, `blocked`+pending → `ActionStillPending`) runs unchanged, with `MergeableState` now constructed via `ParseMergeabilityState(...)`. Add one case for an unrecognized state asserting `ActionIndeterminate` (the `IsUnknown` path).
- **Snapshot-builder regression** (`internal/coordinator`): the `mergeWatchSnapshot` / `mergeWatchPartialSnapshot` paths that previously lowercased locally now rely on the parser; an integration-style test that feeds a mixed-case `MergeableState` through `mergeWatchSnapshot` and asserts `Classify` sees the parsed kind (not a raw string compare) closes the case-normalization gap.

## Out of scope / future

#458–#464 add the new state-keyed logic. This refactor gives them one typed switch to extend instead of a third and fourth string-compare site. When they add a predicate, the parser's exhaustiveness and the regression tables are the surface that grows — by design, in one file.
