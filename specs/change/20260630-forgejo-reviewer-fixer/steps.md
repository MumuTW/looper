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

- Routed the default runtime `fixerGitHubAdapter` through Forgejo provider operations so Forgejo Fixer can discover PRs, resolve current-user/author metadata, read top-level PR comments, publish/update the fixed Fixer Summary comment, compare heads for evidence reachability, and mutate PR labels without falling through to GitHub-only APIs.
- Added explicit Forgejo no-native-review guards in the fixer runtime adapter so thread listing/reply/resolve paths fail fast if they are reached on Forgejo instead of silently attempting unsupported GitHub review-thread behavior.
- Added runtime regression coverage in `internal/runtime/scheduler_forgejo_test.go` for the Forgejo Fixer summary-comment flow, including top-level comment create/update, compare, label mutation, and unsupported native review-thread rejection.
- Added config validation coverage in `internal/config/config_test.go` proving Forgejo still rejects unsupported reviewer auto-merge, reviewer thread resolution, and fixer auto-discovery opt-ins.
- Verified with `go test ./internal/runtime ./internal/config` and `go test ./...`.

## Step 5 (AFK): EAG Validation

- Contract E2E workflows/commands run:
  - `go build ./...`
  - `go vet ./...`
  - `go test ./internal/e2e/forgejocontract -count=1`
  - `go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1`
  - `go test ./...`
- Contract E2E observations:
  - Forgejo contract/integration coverage passed after normalizing module metadata with `go mod tidy`.
  - Added the missing real-sandbox summary-protocol slice and supporting fake-agent/test helpers, then reran `go test ./internal/e2e/...` successfully.
- Sandbox repository: `https://code.powerformer.net/core/looper-sandbox`
- Sandbox operating reference: `specs/change/20260622-forgejo-provider-e2e/real-agent-e2e.md`
- Isolated runtime/config validation/daemon observation notes:
  - Live run used the gated local sandbox env from `e2e/.env`, but forced `/opt/homebrew/bin/go` first on `PATH` and unset Go-specific env overrides so the repo used Go 1.26 instead of the older Go binary exposed by the sandbox env file.
  - Live validation command: `bash -lc 'set -a; . "e2e/.env"; set +a; export PATH="/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:$PATH"; unset GOFLAGS GO111MODULE GOPROXY GOSUMDB GOPRIVATE GOPATH GOMODCACHE GOWORK GOROOT GOTOOLCHAIN; go test -run "^TestForgejoSandboxFixerResolvesReviewThread$" ./internal/e2e -count=1 -v'`
  - The sandbox test used isolated temp homes/worktrees and bounded daemon runs for reviewer → fixer → reviewer, matching the real-agent e2e operating pattern.
- Sandbox PR/branch/artifacts used:
  - `TestForgejoSandboxFixerResolvesReviewThread` created an ephemeral sandbox PR with the `looper-e2e:<runID>` title prefix and `looper-e2e-<runID>-fixer-summary-protocol` branch naming, then auto-closed the PR and deleted the branch on success.
  - The run used three isolated temp homes (reviewer-open, fixer, reviewer-clean) plus fake-agent artifact dirs under the test temp root.
- Reviewer Summary comment observed:
  - Reviewer round 1 published exactly one top-level PR comment containing the visible `Looper Forgejo Reviewer Summary` markdown plus the embedded `<!-- looper:forgejo-reviewer-summary ... -->` JSON marker.
  - Reviewer round 2 updated the same summary comment in place and marked the prior `R-001` item `resolved`.
- Fixer Summary comment observed:
  - Fixer published exactly one top-level PR comment containing the visible `Looper Forgejo Fixer Summary` markdown plus the embedded `<!-- looper:forgejo-fixer-summary ... -->` JSON marker.
  - The Fixer Summary consumed the Reviewer Summary open item and recorded a single result for `R-001`.
- No-resolve/native-review mutation observations:
  - Forgejo summary-protocol validation completed without native review-thread resolve/unresolve APIs.
  - The live sandbox path stayed on top-level PR comments for Reviewer/Fixer protocol state; no native review resolution behavior was required or asserted.
- Issues discovered during EAG:
  - `go build ./...` initially failed because `go.mod`/`go.sum` were out of sync.
  - The repository had no real Forgejo sandbox coverage for the new reviewer/fixer summary protocol.
  - The initial sandbox reviewer setup used the wrong review label/path for Forgejo summary publishing.
  - The fake-agent commit mode tried to fetch observed review-thread hashes even when no explicit GitHub CLI path was configured.
  - The clean reviewer sandbox helper initially used GitHub-style APPROVE semantics instead of Forgejo comment-only review events.
- Fixes made during EAG:
  - Ran `go mod tidy` to restore buildable module metadata.
  - Added `TestForgejoSandboxFixerResolvesReviewThread` plus Forgejo sandbox helper/config coverage in `internal/e2e/forgejo_sandbox_test.go` and updated `internal/e2e/forgejo_mirror_inventory_test.go`.
  - Added bounded fake-agent modes for Forgejo reviewer summary rounds in `internal/e2e/harness/cmd/fake-agent/main.go` plus targeted fake-agent tests.
  - Fixed sandbox reviewer setup to use the Forgejo comment-only `looper:review` path, manual reviewer loops, and COMMENT/COMMENT review events.
  - Guarded fake-agent thread-hash fetching behind an explicit `LOOPER_E2E_FAKE_AGENT_GH_PATH` and added reviewer regression coverage for inferred Forgejo comment-only clean handling.
- Cleanup status:
  - Successful live sandbox runs auto-closed the ephemeral PR and deleted the ephemeral branch.
  - Follow-up read-only API inspection for recently closed matching PRs returned `403`, so post-cleanup IDs were not retained outside the test logs.

## Step 6 (AFK): Documentation Sync

- Updated user-facing Forgejo support wording in `README.md`, `docs/users-guide.md`, and `docs/configuration.md` from comment-only reviewer / unsupported fixer to the implemented summary-comment Reviewer/Fixer protocol.
- Documented that Forgejo Reviewer publishes a top-level Reviewer Summary comment, Forgejo Fixer consumes only open Reviewer Summary items, publishes a top-level Fixer Summary comment, and does not use native review-thread resolution.
- Kept the Looper skill config reference in `skills/looper/references/config.md` aligned with the same Forgejo role limits.
- Verified documentation references with targeted Forgejo markdown searches and `go test ./internal/forge ./internal/reviewer ./internal/fixer ./internal/runtime ./internal/config`.
