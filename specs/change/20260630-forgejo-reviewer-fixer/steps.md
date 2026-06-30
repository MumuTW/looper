# Steps

## Step 1 (AFK): Summary Protocol Core

- Added `internal/forge/summary_protocol.go` with shared Forgejo Reviewer/Fixer Summary v1 types, constants, marker rendering/parsing, single-comment extraction, schema-version checks, enum validation, Review Item ID uniqueness/link validation, and Fixer-result validation against Reviewer Summary open items.
- Added focused tests in `internal/forge/summary_protocol_test.go` for render/parse round trips, missing/duplicate/invalid marker failures, schema/enum/round validation, duplicate Review Item IDs, superseded-link invariants, and Fixer missing/unknown/non-open result rejection.
- Verified with `go test ./internal/forge` and `go test ./...`.

## Step 2 (AFK): Reviewer Summary Publishing

- Updated Forgejo reviewer comment-only publish to upsert exactly one fixed Reviewer Summary PR comment via `internal/forge/summary_protocol.go` instead of treating freeform markdown as protocol state.
- Extended the comment-only reviewer completion contract in `internal/reviewer/runner.go` so the agent must return structured `summary` / `outcome` / `findings` data, with optional `review_item_id` reuse and `supersedes` links for materially redefined issues.
- Added deterministic Reviewer Summary synthesis that preserves historical items, reuses referenced `review_item_id` values, allocates new `R-###` IDs, marks unmatched open items `resolved`, and marks explicitly replaced items `superseded`.
- Added Forgejo reviewer adapter support for list/update top-level PR comments so summary comments can be edited in place.
- Added focused reviewer tests for the prompt contract, summary-comment creation, clean summary publication, existing-summary update/id reuse/supersede behavior, and duplicate-summary failure.
- Verified with:
  - `gofmt -w "internal/reviewer/runner.go" "internal/reviewer/runner_test.go" "internal/reviewer/runner_integration_test.go" "internal/runtime/scheduler.go"`
  - `go test ./internal/reviewer ./internal/runtime ./internal/forge`

## Step 3 (AFK): Fixer Summary Consumption And Publishing

- Updated `internal/fixer/runner.go` so Fixer consumes Forgejo Reviewer Summary comments as the repair-work authority, converts only `open` Review Items into fix items, and fails fast on invalid or duplicated Reviewer Summary protocol state during the collect/resolve path.
- Added Forgejo Fixer Summary publishing in the resolve-comments phase: validates one agent result/explanation per open Review Item, renders the `looper:forgejo-fixer-summary` v1 marker plus visible Markdown, creates or updates the single Fixer Summary top-level comment, and no-ops when the Reviewer Summary has zero open items.
- Preserved the no-resolve protocol by short-circuiting Forgejo summary-backed fixer runs before GitHub-style review-thread view/reply/resolve mutation logic.
- Added focused tests for Reviewer Summary open-item consumption, Fixer Summary validation/render inputs, and missing per-item agent-result rejection.
- Verified with:
  - `gofmt -w "internal/fixer/runner.go" "internal/fixer/runner_test.go"`
  - `go test ./internal/fixer ./internal/forge`
  - `go test ./...`

## Step 4 (AFK): Forgejo Role Integration And Validation

<!-- Implementation and verification notes for the matching Plan step. -->

## Step 5 (AFK): EAG Validation

Record the EAG in detail here during implementation:

- Contract E2E workflows/commands run:
- Contract E2E observations:
- Sandbox repository: `https://code.powerformer.net/core/looper-sandbox`
- Sandbox operating reference: `specs/change/20260622-forgejo-provider-e2e/real-agent-e2e.md`
- Isolated runtime/config validation/daemon observation notes:
- Sandbox PR/branch/artifacts used:
- Reviewer Summary comment observed:
- Fixer Summary comment observed:
- No-resolve/native-review mutation observations:
- Issues discovered during EAG:
- Fixes made during EAG:
- Cleanup status:

## Step 6 (AFK): Documentation Sync

<!-- Implementation and verification notes for the matching Plan step. -->
