# Spec: Remove the bare `hold` label (#227)

- **Issue:** MumuTW/looper#227 — Remove the bare `hold` label, which duplicates `looper:hold` and does nothing
- **Base branch:** `main`
- **Type:** Forge label deletion (no production code change)

## Problem

The repository carries two hold labels with identical colour (`B60205`) and near-identical descriptions:

| label | colour | description |
|---|---|---|
| `hold` | `B60205` | Temporarily pause automated Looper work |
| `looper:hold` | `B60205` | Pause all automatic Looper work |

`looper:hold` is the fixed, operator-facing hold contract. Every fixed hold gate reads `labels.HoldGlobal`, which is the constant `"looper:hold"`:

- `domain.IsAutoLaneHeld` — `internal/domain/domain.go:155`
- `dispatch.autonomousDispatchHeld` — `internal/coordinator/dispatch/dispatch.go:318`
- `gatekeeper.evaluate` — `internal/gatekeeper/runner.go:273`

No fixed code path reads a bare `hold`. A grep for the literal `"hold"` in Go source turns up only a `ReasonCode` constant (`"hold"`) and an assembled alias (`labels.Prefix + "hold"`, which evaluates to `looper:hold`) — neither is a bare-`hold` read. The one exception is the compatibility setting `roles.coordinator.dispatch.autonomous.holdLabel`: it is checked in addition to `looper:hold`, and must be audited before deletion. The GitHub gateway can also recreate a non-standard label if effective configuration asks it to apply that label. Subject to those configuration checks, the bare label is decorative: it promises exactly the behaviour it does not have.

The failure mode is silent. A maintainer reaching for a hold sees two identically-coloured labels, picks the shorter one, and gets no hold — indistinguishable from no hold being wanted. This is the same trap #167 fixed for case variants (`Looper:Hold` was ignored), reached by a different route. The difference is that #227 is a label deletion, not a code change.

## Goals

- Eliminate the silent no-op hold label so a maintainer cannot accidentally pick a hold that does nothing.
- Make no production source change: the authority (`looper:hold`) already works; migrate any effective configuration that still uses `hold` before deletion.
- Keep the change costless and reversible once the pre-delete checks establish that `hold` is unused by items and active configuration.

## Non-goals

- Changing `labels.HoldGlobal` or any hold-gate logic. The authority path is correct and stays as-is.
- Adding bare `hold` to the `HoldGlobal` match set (the "make it work" alternative — see Alternatives).
- Touching the per-role holds (`looper:hold:worker`, `:fixer`, `:reviewer`).

## Approach

A single forge operation, after the pre-delete checks: delete the `hold` label from the repository.

```shell
gh api -X DELETE /repos/MumuTW/looper/labels/hold
```

No production source or documentation change is needed. The active configuration may need a migration before the forge operation:

- **Configured application can recreate it.** `labels.Standard()` (`internal/labels/labels.go:111`) does not contain bare `hold`, but `AddIssueLabels` and `AddPullRequestLabels` call `ensureLabelsExist` before applying a configured label. A running configuration that asks Looper to add `hold` would recreate it with the fallback presentation. Deletion is therefore conditional on auditing the effective configuration and migrating any such value first.
- **`labelaudit` does not protect it.** The audit (`internal/labelaudit`) only governs `looper:`-prefixed labels; bare `hold` is outside Looper's namespace and outside the audit's scope.
- **Docs already name the official family.** `docs/users-guide.md` and `docs/configuration.md` list `looper:hold` and the per-role variants as the official hold labels. The configuration guide also identifies `roles.coordinator.dispatch.autonomous.holdLabel` as a compatibility-only override, so this migration must preserve its documented `looper:hold` value.

### Pre-delete check (gate, not ceremony)

Before deleting, establish that no item or live configuration uses `hold`, and that the GitHub search result is complete:

1. **In-repo usage:** run `gh api "search/issues?q=repo:MumuTW/looper+label:hold" --jq '{total_count, incomplete_results}'`. It must report exactly `{"total_count":0,"incomplete_results":false}` (open and closed). The issue reports zero today; re-confirm at execution time. If either value differs, stop: use a complete, authoritative listing and migrate those items to `looper:hold` (or deliberately drop the label) before retrying.
2. **Effective configuration:** inspect the running `looperd` configuration after all precedence layers (file, environment, and flags), not only the checked-in defaults. `roles.coordinator.dispatch.autonomous.holdLabel` must be `looper:hold` (or empty if legacy compatibility is intentionally disabled). Also reject any effective label value normalized to `hold` that can reach Looper's automatic issue/PR label-application paths; migrate it to its intended official or project-specific label, restart/reload the daemon, and re-inspect before deleting. This prevents both removal of a configured autonomous-dispatch veto and automatic recreation by `ensureLabelsExist`.
3. **External dependency:** the issue flags that a maintainer-owned automation or saved filter outside this repository could apply `hold`. There is no way to survey that exhaustively from inside the repo. `looper:hold` is the only documented operator-facing hold contract, but a known external consumer must be migrated before deletion rather than assumed harmless.

### Execution

Run the `DELETE` only after every pre-delete check passes. The desired state is a missing label; reruns return 404 and must be handled as an already-complete result.

## Alternatives considered

### A. Make `hold` work — add it to the `HoldGlobal` match set

Instead of deleting, teach `labels.Has(..., labels.HoldGlobal)` (or a dedicated matcher) to also accept bare `hold`.

- **Rejected.** This keeps a second, un-namespaced control surface that promises the same thing as the official one. It trades a silent no-op for a silent duplicate: two labels that mean the same thing drift independently, and the `labelaudit` machinery — which exists precisely to keep label authority unambiguous — has no jurisdiction over the un-prefixed name. The issue names this as the option to avoid: "keeping a decorative label that reads like a control." Deletion removes the ambiguity rather than ratifying it.

### B. Leave `hold` in place

- **Rejected.** Zero usage today means deletion costs nothing, and leaving it preserves the exact silent-failure trap the issue is about. Doing nothing is the status quo the issue was opened against.

## Risks

- **Configured legacy veto or re-creation.** `holdLabel` can veto autonomous dispatch and configured label-application paths can recreate any non-standard label. Mitigation: inspect the effective configuration, migrate every applicable `hold` value, restart/reload it, and re-check before deletion. No new persisted state or runtime gate is added; this is a one-time operator precondition for a destructive forge operation.
- **External automation applies `hold`.** A maintainer-owned saved filter or script outside this repo could rely on bare `hold`. Mitigation: migrate known consumers before deletion. If an external consumer must retain the alias, do not delete until its product decision is made.
- **Re-creation by a human.** A maintainer could re-add `hold` later. This is not a code risk — it is the same human-discipline problem that exists for any label, and the docs pin the official family. No new guard is added because it would turn a one-time operator decision into an unauthoritative runtime gate.
- **No regression risk after migration.** The fixed code paths do not read bare `hold`; once active configuration and labeled items have been migrated, deleting it cannot change a hold gate's output. `go test ./...` is expected to be unchanged.

## Validation

- **Pre-delete:** the label search reports `total_count: 0` and `incomplete_results: false`; the effective runtime configuration contains no `hold` value in `holdLabel` or any automatic label-application path; and known external consumers are migrated.
- **Post-delete:** `gh api /repos/MumuTW/looper/labels/hold` returns 404; `gh api /repos/MumuTW/looper/labels/looper:hold` still returns 200 with colour `b60205`.
- **No code regression:** `go vet ./...` and `go test ./...` remain green and unchanged (no source files are modified).
- **Docs consistency:** `docs/users-guide.md` and `docs/configuration.md` list the official `looper:hold` family and document the compatibility-only `holdLabel` override; no documentation edit is required for a configuration migration. Spot-check that no doc presents bare `hold` as the operator-facing control.

## Out of scope / follow-ups

- If, after deletion, a maintainer wants a short alias for muscle memory, the supported path is to make `looper:hold` easier to type (e.g. a saved filter or a `gh` alias), not to reintroduce a second label.
- No changelog entry is required for a forge-only label deletion, unless repo convention asks for one; if added, it belongs under the next patch release noting the removal of the duplicate `hold` label.
