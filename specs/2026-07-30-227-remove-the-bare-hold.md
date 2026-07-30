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

Only `looper:hold` is wired to anything. Every hold gate reads `labels.HoldGlobal`, which is the constant `"looper:hold"`:

- `domain.IsAutoLaneHeld` — `internal/domain/domain.go:155`
- `dispatch.autonomousDispatchHeld` — `internal/coordinator/dispatch/dispatch.go:318`
- `gatekeeper.evaluate` — `internal/gatekeeper/runner.go:273`

No code reads a bare `hold`. A grep for the literal `"hold"` in Go source turns up only a `ReasonCode` constant (`"hold"`) and an assembled alias (`labels.Prefix + "hold"`, which evaluates to `looper:hold`) — neither is a bare-`hold` read. The bare label is decorative: it promises exactly the behaviour it does not have.

The failure mode is silent. A maintainer reaching for a hold sees two identically-coloured labels, picks the shorter one, and gets no hold — indistinguishable from no hold being wanted. This is the same trap #167 fixed for case variants (`Looper:Hold` was ignored), reached by a different route. The difference is that #227 is a label deletion, not a code change.

## Goals

- Eliminate the silent no-op hold label so a maintainer cannot accidentally pick a hold that does nothing.
- Make no production code change: the authority (`looper:hold`) already works; the duplicate is the only thing wrong.
- Keep the change costless and reversible: `hold` is applied to 0 issues/PRs today.

## Non-goals

- Changing `labels.HoldGlobal` or any hold-gate logic. The authority path is correct and stays as-is.
- Adding bare `hold` to the `HoldGlobal` match set (the "make it work" alternative — see Alternatives).
- Touching the per-role holds (`looper:hold:worker`, `:fixer`, `:reviewer`).

## Approach

A single forge operation: delete the `hold` label from the repository.

```shell
gh api -X DELETE /repos/MumuTW/looper/labels/hold
```

No production code, config, or docs need to change:

- **Provisioning will not recreate it.** `labels.Standard()` (`internal/labels/labels.go:111`) lists exactly the official labels Looper provisions; bare `hold` is not among them. Triage/provisioning only creates what `Standard()` declares.
- **`labelaudit` does not protect it.** The audit (`internal/labelaudit`) only governs `looper:`-prefixed labels; bare `hold` is outside Looper's namespace and outside the audit's scope.
- **Docs already name only the official family.** `docs/users-guide.md` and `docs/configuration.md` list `looper:hold` and the per-role variants as the official hold labels and state "Looper never adds or removes hold labels." No doc references bare `hold` as a control.

### Pre-delete check (gate, not ceremony)

Before deleting, confirm nothing in the repository currently uses `hold` and nothing outside the repository depends on it:

1. **In-repo usage:** `gh api "search/issues?q=repo:MumuTW/looper+label:hold"` must return 0 total results (open and closed). The issue reports 0 today; re-confirm at execution time. If non-zero, stop: those items must first be migrated to `looper:hold` (or have the label deliberately dropped) so no item silently loses its intended hold.
2. **External dependency:** the issue flags that a maintainer-owned automation or saved filter outside this repository could apply `hold`. There is no way to survey that exhaustively from inside the repo. The mitigation is that `looper:hold` is the only hold label Looper documents, so any external automation applying bare `hold` was already operating on a false assumption. If a known external consumer exists, prefer the "make it work" alternative (below) over deletion.

### Execution

Run the `DELETE` once the pre-delete check passes. The desired state is a missing label; reruns return 404 and must be handled as an already-complete result.

## Alternatives considered

### A. Make `hold` work — add it to the `HoldGlobal` match set

Instead of deleting, teach `labels.Has(..., labels.HoldGlobal)` (or a dedicated matcher) to also accept bare `hold`.

- **Rejected.** This keeps a second, un-namespaced control surface that promises the same thing as the official one. It trades a silent no-op for a silent duplicate: two labels that mean the same thing drift independently, and the `labelaudit` machinery — which exists precisely to keep label authority unambiguous — has no jurisdiction over the un-prefixed name. The issue names this as the option to avoid: "keeping a decorative label that reads like a control." Deletion removes the ambiguity rather than ratifying it.

### B. Leave `hold` in place

- **Rejected.** Zero usage today means deletion costs nothing, and leaving it preserves the exact silent-failure trap the issue is about. Doing nothing is the status quo the issue was opened against.

## Risks

- **External automation applies `hold`.** A maintainer-owned saved filter or script outside this repo could rely on bare `hold`. Mitigation: the pre-delete check (step 2) surfaces known consumers; `looper:hold` is the only documented contract, so any such consumer was already broken in intent. If discovered, switch to alternative A for that consumer rather than deleting blindly.
- **Re-creation by a human.** A maintainer could re-add `hold` later. This is not a code risk — it is the same human-discipline problem that exists for any label, and the docs already pin the official set. No guard is added; adding one would be a new gate on a human action with no authority story beyond "we don't trust the maintainer," which the design guidelines reject.
- **No regression risk to hold behaviour.** No code path reads bare `hold`, so deleting it cannot change any gate's output. `go test ./...` is expected to be unchanged.

## Validation

- **Pre-delete:** `gh api "search/issues?q=repo:MumuTW/looper+label:hold"` returns `total_count: 0`.
- **Post-delete:** `gh api /repos/MumuTW/looper/labels/hold` returns 404; `gh api /repos/MumuTW/looper/labels/looper:hold` still returns 200 with colour `b60205`.
- **No code regression:** `go vet ./...` and `go test ./...` remain green and unchanged (no source files are modified).
- **Docs consistency:** `docs/users-guide.md` and `docs/configuration.md` already list only the `looper:hold` family; no edit required. Spot-check that no doc or spec references bare `hold` as a control (none found during planning).

## Out of scope / follow-ups

- If, after deletion, a maintainer wants a short alias for muscle memory, the supported path is to make `looper:hold` easier to type (e.g. a saved filter or a `gh` alias), not to reintroduce a second label.
- No changelog entry is required for a forge-only label deletion, unless repo convention asks for one; if added, it belongs under the next patch release noting the removal of the duplicate `hold` label.
