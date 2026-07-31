# Spec: One typed mergeability-state model shared by Gatekeeper and mergewatch (#467)

- **Issue:** MumuTW/looper#467 — refactor(github): one typed mergeability-state model shared by Gatekeeper and mergewatch
- **Base branch:** `main`
- **Type:** Refactor of the GitHub mergeability-state boundary; no behavior change
- **Lifecycle:** This spec PR *references* #467; it must **not** close it. The diff is planning-only — no typed boundary, consumer migration, or regression tests land here — so #467 stays open until the implementation PR lands. The PR body must use a non-closing reference (e.g. `Refs #467`), not `Closes #467`, or GitHub will auto-close the implementation issue the moment this spec reaches the default branch.
- **Refs:** #458, #459, #460, #464 (all add state-keyed logic — the third/fourth copy this prevents), #386 (consolidating shared role types, same spirit)

## Problem

GitHub's mergeability vocabulary reaches the daemon as a raw string and is pattern-matched independently in two packages, against two hand-rolled subsets of the same undocumented magic strings (`"clean"`, `"dirty"`, `"blocked"`, `"unstable"`, `"has_hooks"`, `"unknown"`, plus the GraphQL-only `"behind"`).

- **Gatekeeper** (`internal/gatekeeper/runner.go:336-347`): lowercases + trimming at the consumer, then `"dirty"` → `ReasonMergeConflict`; `providerPolicyStateIsAmbiguous` (`runner.go:643-650`: `"blocked"`, `"unstable"`, `"has_hooks"`) → `ReasonProviderStateAmbiguous`; anything else non-`"clean"` → `ReasonMergeabilityNotClean`. The `Mergeable == nil || state == "" || state == "unknown"` branch (`:338`) is a separate ambiguity path.
- **Coordinator mergewatch** (`internal/coordinator/mergewatch/mergewatch.go:90,102` and `internal/coordinator/mergewatch_runner.go:184-219`): lowercases + trimming at the consumer (`mergewatch_runner.go:184`), then compares against `"unknown"` (`:90`), `"dirty"` (`:102`), and `"unstable"` (`:205,216`) with its own action mapping. `Classify` (`mergewatch.go:73`) receives the already-normalized string on `PRSnapshot.MergeableState`.

Two packages, two normalizations, two subsets, no shared source of truth. Issues #458–#464 each add logic keyed on these states, which without this refactor means a third and fourth copy of the same string vocabulary. Divergence here is exactly the class of bug the compiler should catch — the invariant belongs in a type, not in each consumer's memory of GitHub's vocabulary. The raw string also leaks through two transport shapes: the REST `mergeable_state` field (lowercase) and the GraphQL `mergeStateStatus` field (uppercase: `CLEAN`, `DIRTY`, `BLOCKED`, `UNSTABLE`, `HAS_HOOKS`, `UNKNOWN`, `BEHIND`). The gateway already collapses these at `gateway.go:1731` (`firstNonEmpty(mergeable_state, mergeStateStatus)`), so a consumer can receive either casing; today each consumer re-lowercases to defend against that.

## Goals

- A typed `MergeabilityState` in the GitHub infra layer (`internal/infra/github`), parsed **once at the gateway boundary** from the raw string, with two distinct "we don't know" cases that preserve the original string as evidence: `Unknown` (provider-ambiguous: `""`/`"unknown"`) and `Unrecognized` (a non-empty, non-`"unknown"` value not in the known set). `Draft` is a named known kind. The zero value of the type is `Unknown` so uninitialized values fail closed.
- Semantic predicates on the type — `IsClean()`, `HasConflict()`, `IsAmbiguousPolicyState()`, `IsUnknown()`, `IsUnstable()` — so consumers state intent instead of comparing strings. The predicate set is the *union* of what Gatekeeper and mergewatch currently ask, no more. `Draft` and `Unrecognized` deliberately match no predicate besides `!IsClean()`, preserving today's fall-through.
- Gatekeeper and mergewatch consume the type; the raw string survives only inside in-memory telemetry/audit. Durable `Evidence` persists the parser's `Normalized()` form so existing eventlog readers see byte-identical values.
- Exhaustive switch coverage in the parser so a newly observed provider state is captured in one place (as `Unrecognized`, auditable via `Raw()`) instead of silently falling through two consumers; an explicit exhaustiveness test (not compiler enforcement) keeps the kind set and predicates in sync.
- Behavior unchanged: the existing Gatekeeper reason mapping and mergewatch action mapping are preserved exactly, covered by regression tests against the shared type.

## Non-goals

- Changing *what* each consumer does with a given state. The reason/action mapping tables stay as-is; only the dispatch mechanism (string compare → typed predicate) changes.
- Touching the GraphQL `mergeStateStatus` → `HasConflicts` probe (`gateway.go:803,839,1680`). That is a separate boolean derived from `DIRTY` for discovery-list fast paths and is not part of the mergeability-state consumer surface this issue targets. It is noted only because the typed parser must accept the uppercase GraphQL vocabulary too.
- Adding new states, new predicates, or new gates. This is a consolidation, not an expansion. #458–#464 add the new state-keyed logic; this refactor only gives them one place to hang it.
- Persisting the typed enum in `Evidence`/eventlog. The durable `Evidence.MergeableState` field stays a string for backward-compatible audit reads; the type is an in-memory dispatch boundary, not a new persisted concept.

## Authority

> **What is the authority for this action, and why is it not the agent's own structured output?**

This refactor adds no gate, validation, or persisted field — it moves an existing string-to-decision mapping into a type. The authority for "what does state X mean" is GitHub's documented `mergeable_state` / `mergeStateStatus` vocabulary, parsed once at the gateway where the raw provider response enters the process. Consumers stop re-deriving that meaning locally. There is no agent output in this path, so the agent-output trust question does not arise.

## Trade-off

> Delete this six months from now — what breaks?

The two consumers revert to independent `strings.ToLower(strings.TrimSpace(...))` + hand-rolled string subsets. Issues #458–#464 then land their new state-keyed logic as a third and fourth copy of the same vocabulary with no shared source of truth, and the silent divergence the type prevents returns: one consumer can recognize `behind`/`draft`/`has_hooks` while the other does not, with no compile or test signal. The regression tables pinned against the shared type (every state both consumers handle, mapped to its reason/action) lose their meaning, because there is no longer one type to pin them against.

> What does it still not catch?

It types only the values GitHub returns today. A genuinely new provider value still lands in `MergeabilityUnrecognized` and falls through to the existing `ReasonMergeabilityNotClean` / check-pending behavior — the type makes it auditable (`Raw()` preserved) and exhaustively tested, but does not by itself decide what the new value *means*. It also does not catch predicate *misuse* (a consumer calling the wrong predicate for its intent); it catches only string-vocabulary divergence and the "two normalizations" drift. Finally, it does not catch a consumer that re-introduces a local string compare alongside the predicates — the "no string literals remain" grep in Validation is the backstop for that, not the type.

> Why deletion or failing loudly is insufficient.

Deleting the type does not surface the divergence — it just hides it again in two consumers' memories. Failing loudly on every unrecognized value (mapping it to `IsUnknown()` and the provider-ambiguity path) was the first draft and is rejected here: it changes behavior for the unrecognized + non-nil `Mergeable` case, which today falls through to `ReasonMergeabilityNotClean` (Gatekeeper) and normal check/pending (mergewatch). The type's value is consolidating the *known* vocabulary in one exhaustively-tested place while preserving the existing fall-through for what it does not know — neither deletion nor fail-loud gives both properties.

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
    // MergeabilityUnknown is the zero value so an uninitialized
    // MergeabilityState{} fails closed (IsUnknown() == true) instead of
    // reporting clean. It covers the provider-ambiguous inputs: the empty
    // string and "unknown" — GitHub has not returned a usable value.
    MergeabilityUnknown MergeabilityKind = iota
    MergeabilityClean        // clean, mergeable
    MergeabilityDirty        // conflict
    MergeabilityBlocked      // policy blocker, ambiguous
    MergeabilityUnstable     // failing/pending required checks, ambiguous
    MergeabilityHasHooks     // server-side hooks pending, ambiguous
    MergeabilityBehind       // GraphQL-only; head behind base
    MergeabilityDraft        // GitHub draft state; existing fall-through semantics
    MergeabilityUnrecognized // a non-empty, non-"unknown" value not in the known set
)
```

Two distinct "we don't know" kinds, with different semantics, so the refactor preserves today's behavior exactly:

- `MergeabilityUnknown` — the *provider-ambiguous* case: the empty string and `"unknown"`. This is the only kind for which `IsUnknown()` is true, and it is the only kind that drives Gatekeeper's `ReasonProviderStateAmbiguous` early block and mergewatch's indeterminate/deadline path. (The `Mergeable == nil` check stays separate at the consumers, exactly as today.)
- `MergeabilityUnrecognized` — a non-empty, non-`"unknown"` string the parser does not recognize (e.g. a future `"pending"`). It preserves the original string via `Raw()` but **keeps the existing fall-through behavior**: not clean, not conflict, not ambiguous-policy, not unknown, so Gatekeeper routes it to `ReasonMergeabilityNotClean` and mergewatch routes it to its normal check/pending classification — the same outcome today's code produces for any unmatched string with a non-nil `Mergeable`. Mapping it to `IsUnknown()` instead would be a behavior change (Gatekeeper provider block + mergewatch indeterminate) and is deliberately not done.
- `MergeabilityDraft` — GitHub already returns `DRAFT`/`draft` as a mergeability state today. It gets its own kind so the parser is exhaustive over the documented vocabulary and the value is auditable, but it shares `MergeabilityUnrecognized`'s fall-through predicate behavior (no predicate besides `!IsClean()` is true for it): today `draft` is not `dirty`, not `unknown`, not in `{"blocked","unstable","has_hooks"}`, and not `clean`, so Gatekeeper routes it to `ReasonMergeabilityNotClean` and mergewatch to check/pending. The kind preserves that exactly; no `IsDraft()` predicate is added in this refactor.

- `ParseMergeabilityState(raw string) MergeabilityState` is the single parse site. It trims, lowercases, and switches exhaustively over the known vocabulary (`""` and `"unknown"` → `Unknown`; `"clean"` → `Clean`; `"dirty"` → `Dirty`; `"blocked"` → `Blocked`; `"unstable"` → `Unstable`; `"has_hooks"` → `HasHooks`; `"behind"` → `Behind`; `"draft"` → `Draft`). The `default` arm sets `MergeabilityUnrecognized` and keeps the original `raw` so an unrecognized provider value is auditable and falls through with the old behavior, not silently re-classified as provider-ambiguous.
- Predicates: `IsClean()`, `HasConflict()`, `IsAmbiguousPolicyState()` (true for `Blocked`/`Unstable`/`HasHooks` — the existing `providerPolicyStateIsAmbiguous` set), `IsUnknown()` (true *only* for `Unknown`), `IsUnstable()` (true only for `Unstable`; the one consumer-specific kind predicate mergewatch needs), `Raw() string` (returns the preserved original string for telemetry/audit), `Normalized() string` (returns `strings.ToLower(strings.TrimSpace(raw))` — the byte-identical representation today's consumers persist, used for durable evidence and reason subjects).
- No `Behind` or `Draft` predicate beyond the kind: Gatekeeper currently routes `behind` and `draft` to `ReasonMergeabilityNotClean` (the "not clean, not ambiguous, not conflict" fall-through), and mergewatch does not special-case either. Both get a kind so the parser is exhaustive and the values are auditable, but no consumer predicate is added until #458–#464 need one. Adding the kinds now is what makes the switch exhaustive; adding predicates now would be speculative.

### 2. Gateway parse site — `internal/infra/github/gateway.go`

- `PullRequestDetail.MergeableState` changes type from `string` to `MergeabilityState`. The two assignment sites call `ParseMergeabilityState(...)` instead of storing the raw string, and case normalization moves out of the consumers and into the parser:
  - `gateway.go:1686` (`pullRequestDetailFromViewRow`, the GraphQL-shaped `gh pr view --json` view). **This site must also change which field it reads**: today it reads `row["mergeable_state"]`, but the GraphQL-shaped field lists (`prViewMetadataJSONFields`/`prViewFixerJSONFields`/`prViewReviewerJSONFields`/`prViewGatekeeperJSONFields` at `gateway.go:36-41`) request `mergeStateStatus`, not `mergeable_state`, so `row["mergeable_state"]` is always empty here and `ParseMergeabilityState` would parse an empty string regardless of input. The site reads `row["mergeStateStatus"]` (uppercase GraphQL vocabulary) and passes that to `ParseMergeabilityState`; the parser's lowercase step is what makes the uppercase GraphQL boundary reachable and tested.
  - `gateway.go:1731` (`firstNonEmpty(row["mergeable_state"], row["mergeStateStatus"])` for the REST `ViewPullRequestMergeWatch` used by both Gatekeeper and mergewatch) wraps the existing `firstNonEmpty(...)` expression in `ParseMergeabilityState(...)`. This site already collapses both casings, so only the parse wrapping changes.
- `HasConflicts` (`:803,839,1680`) is left untouched — it is a separate GraphQL-only boolean, not a mergeability-state consumer.

### 3. Gatekeeper — `internal/gatekeeper/runner.go`

- `Evidence.MergeableState` stays `string` for durable audit reads; it is populated from `state.Normalized()` (i.e. `strings.ToLower(strings.TrimSpace(raw))`), **not** `state.Raw()`, so the persisted report is byte-identical to today's `report.Evidence.MergeableState = strings.ToLower(strings.TrimSpace(mergeability.MergeableState))` (`runner.go:337`). `Raw()` preserves the original provider string (e.g. an uppercase GraphQL `DIRTY` or whitespace-bearing fixture value) for in-memory telemetry/audit only; persisting `Raw()` into `Evidence` would change both the durable event value and the reason subjects for uppercase/whitespace inputs, which is not a byte-identical migration.
- The `:336-347` block becomes predicate calls on the parsed state:
  - `Mergeable == nil || state.IsUnknown()` → `ReasonProviderStateAmbiguous` (today's `state == "" || state == "unknown"`; the empty-string case maps to `Unknown` in the parser). `Mergeable == nil` stays a separate check at the consumer, exactly as today.
  - `!*Mergeable || state.HasConflict()` → `ReasonMergeConflict`.
  - `state.IsAmbiguousPolicyState()` → `ReasonProviderStateAmbiguous` with `Subject: "mergeability:" + state.Normalized()` (today's subject uses the normalized `Evidence.MergeableState`, so `Normalized()` keeps it byte-identical).
  - `!state.IsClean()` (the remaining fall-through, covering `Behind`, `Draft`, `Unrecognized`, and any future non-clean kind that has no predicate) → `ReasonMergeabilityNotClean` with `Subject: state.Normalized()` (today's `:346` subject is the normalized `Evidence.MergeableState`).
- `providerPolicyStateIsAmbiguous` (`:643-650`) is deleted — its logic moves into `MergeabilityState.IsAmbiguousPolicyState()`.

### 4. mergewatch — `internal/coordinator/mergewatch/mergewatch.go` + `mergewatch_runner.go`

- `PRSnapshot.MergeableState` changes type from `string` to `MergeabilityState`. The snapshot builders in `mergewatch_runner.go` (`:184` full snapshot, `:253` partial snapshot) **copy `detail.MergeableState` directly into the snapshot** — they do **not** call `ParseMergeabilityState(...)` again. `PullRequestDetail.MergeableState` is already a parsed `MergeabilityState` after the gateway step (section 2), so re-parsing is unimplementable (`ParseMergeabilityState` takes a string) and would violate the parse-once invariant. The local `strings.ToLower(strings.TrimSpace(detail.MergeableState))` at `:184` and `:253` is deleted; the `mergeableState` local at `:184` becomes the typed `detail.MergeableState` and the `:205,216` comparisons switch to `snapshot.MergeableState.IsUnstable()`.
- `Classify` (`mergewatch.go:73`) switches to predicates:
  - `snapshot.Mergeable == nil || snapshot.MergeableState.IsUnknown()` → `ActionIndeterminate` / deadline path (`:90`). `IsUnknown()` covers `""` and `"unknown"`; today mergewatch checked only `"unknown"`, but an empty `mergeStateStatus` co-occurs with `Mergeable == nil` in practice (GitHub has not computed), so routing `""` through `IsUnknown()` aligns mergewatch with Gatekeeper's existing `state == ""` ambiguity handling with no observable behavior change. `Mergeable == nil` stays a separate check.
  - `snapshot.MergeableState.HasConflict()` → `ActionConflict` (`:102`).
  - The `mergeableState == "unstable"` gating of failed-check detection (`mergewatch_runner.go:205,216`) becomes `snapshot.MergeableState.IsUnstable()`. This is the one place a consumer needs the specific kind rather than a coarse predicate, because `unstable` is the signal that a failing required check is a *red-CI* failure rather than a missing one. The predicate is named for the intent the consumer already has.
- The `ActionStillPending` path (`:29` test case uses `"blocked"`) is unchanged: `blocked` is not conflict, not unknown, not unstable-failed-checks, so it falls through to `StillPending` exactly as today. `draft` and any `Unrecognized` value with a non-nil `Mergeable` fall through the same way — preserving today's behavior for unmatched strings.

### 5. Exhaustiveness

The parser's switch is exhaustive over the known vocabulary with a `default` → `MergeabilityUnrecognized`. A new GitHub state (e.g. a future `"pending"`) lands in `Unrecognized`, preserves its original string via `Raw()`, and falls through with the existing behavior, rather than silently matching neither consumer's subset. This is the "fails loudly in one place" property: the parser is the single site that decides kind, and its regression table asserts every known string maps to the right kind.

Go does **not** require enum-like switches to cover every constant, so adding a `MergeabilityKind` does **not** by itself produce a compile-time signal in the parser or consumers — and the consumers here call coarse predicates (`IsClean`/`HasConflict`/`IsAmbiguousPolicyState`/`IsUnknown`/`IsUnstable`), not a `switch` over the kind, so the compiler cannot flag a new kind that no predicate covers. The "compile-time enforcement" claim is therefore **not** made for this design. Exhaustiveness is enforced by an explicit test mechanism instead:

- `internal/infra/github/mergeability_test.go` includes an **exhaustiveness table** that iterates over every defined `MergeabilityKind` constant (via a `var allKinds = []MergeabilityKind{...}` slice kept next to the constants) and asserts, for each kind: (a) the predicate behavior is defined — `IsClean`/`HasConflict`/`IsAmbiguousPolicyState`/`IsUnknown`/`IsUnstable` each return the intended bool — and (b) at least one parser input maps to it. Adding a new kind without extending this table fails the test (the kind is unreachable from any input and unasserted), which is the real signal. A `go vet`/staticcheck run does not replace this; it is a deliberate, code-owned check.
- The "no string literals remain" grep in Validation is the backstop that prevents a consumer from re-introducing a local string compare that bypasses the predicates entirely.

This is an explicit, test-owned exhaustiveness mechanism, not compiler-enforced. The trade-off is that a contributor who adds a kind and updates the table but forgets a consumer predicate will not get a compile error — the regression tables for Gatekeeper and mergewatch (below) are what catch a missed consumer mapping.

## Risks

- **Predicate semantics drift from string semantics.** The risk of a typed refactor is that the predicate boundaries redraw the lines the strings drew. Mitigation: the predicate set is derived *exactly* from the current string comparisons — `IsAmbiguousPolicyState()` is precisely `{"blocked","unstable","has_hooks"}`, `HasConflict()` is precisely `{"dirty"}`, `IsUnknown()` is precisely `{"", "unknown"}` (provider-ambiguous only), and `Draft`/`Unrecognized` deliberately match no predicate besides `!IsClean()` so they keep the old fall-through. The regression tests (below) pin the mapping for every state both consumers currently handle, so any redraw fails the test.
- **Case/casing regression.** Today each consumer lowercases; the parser must lowercase too, or an uppercase GraphQL `mergeStateStatus` (now actually read at `gateway.go:1686` per section 2) reaches consumers un-normalized. Mitigation: `ParseMergeabilityState` lowercases; the parser test includes uppercase inputs, including the uppercase GraphQL vocabulary read at the `:1686` site.
- **`Behind`/`Draft` over-classification.** Giving `behind` and `draft` their own kinds could tempt a future consumer to predicate on them where today both silently fall through to `ReasonMergeabilityNotClean` / check-pending. Mitigation: no `IsBehind`/`IsDraft` predicate is added in this refactor; the kinds exist only for parser exhaustiveness and audit. #458–#464 add a predicate if/when they need it, with the trade-off answer in their own PR.
- **Persisted-format compatibility.** `Evidence.MergeableState` is a durable event field. Keeping it `string` and populating it from `state.Normalized()` (not `Raw()`) means existing eventlog readers see byte-identical values — `Normalized()` is exactly today's `strings.ToLower(strings.TrimSpace(...))`. `Raw()` is in-memory telemetry/audit only. The type itself is in-memory only.

## Validation

- `go build ./...`, `go vet ./...`, staticcheck (`U1000,SA1006,SA4004,SA4006`), `go test ./...` all pass.
- **No string literals remain.** A grep for `"dirty"`, `"clean"`, `"blocked"`, `"unstable"`, `"has_hooks"`, `"unknown"`, `"behind"`, `"draft"` (as mergeability comparisons) in `internal/gatekeeper` and `internal/coordinator` returns only test fixtures and the parser itself in `internal/infra/github`. The acceptance criterion "no string literal comparisons against mergeability values remain in `internal/gatekeeper` or `internal/coordinator`" is checked by inspection.
- **Parser unit tests** (`internal/infra/github/mergeability_test.go`): for each known lowercase string, each uppercase GraphQL equivalent, the empty string, `"draft"`/`"DRAFT"`, and an unrecognized string — assert the kind, `IsClean`/`HasConflict`/`IsAmbiguousPolicyState`/`IsUnknown`/`IsUnstable`, that `Raw()` preserves the original, and that `Normalized()` equals `strings.ToLower(strings.TrimSpace(input))`. Assert the zero-value `MergeabilityState{}` reports `IsUnknown() == true` and `IsClean() == false` (fails closed). This is the "covering" case from AGENTS.md: a pure function extracted out of two runners and now testable at all; production surface shrinks (two normalizations + two switch sets → one parser), so test growth here is the tell, not a smell.
- **Exhaustiveness table** (same file): iterate `allKinds` and assert each kind has defined predicate behavior and at least one parser input reaching it, per section 5 — the real enforcement that a new kind is not silently dropped.
- **Gatekeeper regression** (`internal/gatekeeper/runner_test.go`): the existing table cases (`dirty` → `ReasonMergeConflict`, `behind` → `ReasonMergeabilityNotClean`, `blocked` → `ReasonProviderStateAmbiguous`, `unknown`/nil → `ReasonProviderStateAmbiguous`) run unchanged against the shared type. Add cases for `draft` and an unrecognized state (non-nil `Mergeable`) asserting they map to `ReasonMergeabilityNotClean` (the fall-through, preserving today's behavior), with the normalized string in `Evidence.MergeableState`. Add a case for an uppercase GraphQL `DIRTY` (read at `gateway.go:1686`) asserting `ReasonMergeConflict` and that `Evidence.MergeableState` is the lowercased `dirty`.
- **mergewatch regression** (`internal/coordinator/mergewatch/mergewatch_test.go`): the existing `TestClassify` table (`unknown` → `ActionIndeterminate`/deadline, `dirty` → `ActionConflict`, `unstable`+failed → `ActionRedCI`, `blocked`+pending → `ActionStillPending`) runs unchanged, with `MergeableState` now a `MergeabilityState` constructed via `ParseMergeabilityState(...)`. Add cases for `draft` and an unrecognized state (non-nil `Mergeable`) asserting `ActionStillPending` (the fall-through, preserving today's behavior for unmatched strings), not `ActionIndeterminate`.
- **Snapshot-builder regression** (`internal/coordinator`): the `mergeWatchSnapshot` / `mergeWatchPartialSnapshot` paths that previously lowercased locally now copy `detail.MergeableState` (already parsed) directly; an integration-style test that feeds a mixed-case `mergeStateStatus` through the gateway parse and `mergeWatchSnapshot` and asserts `Classify` sees the parsed kind (not a raw string compare) closes the case-normalization gap and verifies parse-once.

## Out of scope / future

#458–#464 add the new state-keyed logic. This refactor gives them one typed switch to extend instead of a third and fourth string-compare site. When they add a predicate, the parser's exhaustiveness and the regression tables are the surface that grows — by design, in one file.
