# Steps

## Step 1: Mirror Inventory And Env Compatibility

Notes:

- Added an explicit Step 1 mirror inventory for GitHub→Forgejo contract and live sandbox cases in `internal/e2e/forgejocontract/mirror_inventory_test.go` and `internal/e2e/forgejo_mirror_inventory_test.go`, including run/skip/no-counterpart intent plus reasons.
- Added Step 1 Forgejo sandbox placeholder tests in `internal/e2e/forgejo_sandbox_test.go` so every current GitHub sandbox and dependency-gate case now has a named Forgejo counterpart, with unsupported MVP slices expressed as explicit `t.Skip` reasons.
- Added provider-specific Forgejo live env parsing in `internal/e2e/forgejo_sandbox_test.go` for `LOOPER_E2E_FORGEJO`, `LOOPER_E2E_FORGEJO_BASE_URL`, `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, and `LOOPER_E2E_FORGEJO_TOKEN`, including fail-fast validation for missing/invalid absolute base URL, invalid `owner/repo`, and missing token.
- Added GitHub sandbox repo compatibility parsing so `LOOPER_E2E_GITHUB_SANDBOX_REPO` is preferred, legacy `LOOPER_E2E_SANDBOX_REPO` still works, and conflicting values fail fast.
- Verified with `go test ./internal/e2e -run 'TestGitHubSandboxRepoEnv|TestParseForgejoSandboxConfig|TestForgejoSandbox|TestForgejoSandboxMirrorInventory' -count=1` and `go test ./internal/e2e/forgejocontract -count=1`.

## Step 2: Forgejo Contract E2E

Notes:

- Added `internal/e2e/forgejocontract/contract_test.go` to mirror the GitHub contract posture against the real Forgejo REST boundary with a strict fake HTTP server that enforces exact request order, method, escaped path, query, auth header, JSON body, pagination, and sanitized error behavior.
- Documented an authority source on every strict fake route using the Forgejo API docs, the instance OpenAPI contract, the current MVP capability surface, or recorded live observation, and added a guard test that fails if any route lacks authority metadata.
- Covered supported REST-mapped cases for current user, issue list/view, label add/remove, assignee add/remove, comment create/list/update, pull-request list/view/create/update, diff fetch, compare, and token-redacted provider errors; unsupported dependency/repo-form counterparts remain present with explicit `t.Skip` reasons in the mirror inventory.
- Verified with `go test ./internal/e2e/forgejocontract -count=1`.

## Step 3: Forgejo Live Sandbox Mirror

Notes:

- Implemented real Forgejo live sandbox coverage in `internal/e2e/forgejo_sandbox_test.go` for `TestForgejoSandboxWorkerCreatesPullRequest` and `TestForgejoSandboxNoDiffPathsDoNotOpenOrResolve/worker-no-diff-no-pr`, still guarded by `LOOPER_E2E_FORGEJO=1` and still leaving unsupported fixer-review-thread and dependency-gate slices as explicit `t.Skip` cases with current MVP reasons.
- Added fail-fast live prerequisite validation that goes beyond env-shape parsing: the Forgejo sandbox config now verifies token auth via `CurrentUser`, verifies repository accessibility via Forgejo REST `repos/{owner}/{repo}`, and verifies pull-request listing access before any live run starts.
- Switched Forgejo sandbox setup to derive the authenticated HTTPS clone/push URL strictly from `baseURL`, `repo`, and token, wire the project to a Forgejo provider config, and use Forgejo REST helpers for label creation, issue creation, repository checks, PR discovery, and cleanup instead of `gh`-specific assumptions.
- Added run-scoped titles/branch prefixes and live cleanup for created sandbox issues, pull requests, labels reuse, and pushed branches so Step 3 mirrors the GitHub sandbox posture without introducing clone URL overrides or silent fallbacks.
- Added targeted non-live prerequisite tests in `internal/e2e/forgejo_sandbox_config_test.go` to cover successful live prereq validation and fail-fast repo-inaccessible behavior while keeping live sandbox tests compilable and skipped without credentials.

## Step 4: Copy-Run-Classify Supported Cases

Case failure classifications:

- Deterministic Forgejo REST contract supported cases passed without implementation changes: current user, issue list/view, label add/remove, assignee add/remove, comment create/list/update, pull-request list/view/create/update, diff fetch, compare, pagination, request body/query/auth assertions, and sanitized provider errors remain classified as supported and enabled.
- Non-live Forgejo e2e supported cases passed without implementation changes: Forgejo sandbox config parsing, fail-fast live prerequisite checks, GitHub sandbox repo env compatibility, Forgejo live mirror inventory, and disabled-live sandbox entrypoints remain classified as supported and enabled.
- Unsupported MVP mirrors remain explicit skips rather than green-making skips for supported behavior: native review-thread resolution, Coordinator/dependency-gate behavior, and GitHub-style repo-form handling are still outside the current Forgejo capability set.
- Live Forgejo sandbox execution was not configured in this environment because the required `LOOPER_E2E_FORGEJO=1`, `LOOPER_E2E_FORGEJO_BASE_URL`, `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, and `LOOPER_E2E_FORGEJO_TOKEN` set was not fully present; no live provider defect was classified in Step 4.

Verification:

- `go test ./internal/e2e/forgejocontract -count=1`
- `go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1`

## Step 5: EAG Validation

Notes:

- Ran the deterministic Forgejo contract EAG command: `go test ./internal/e2e/forgejocontract -count=1`.
- Ran the relevant non-live Forgejo e2e package tests: `go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1`.
- Did not run the opt-in live Forgejo sandbox command because the required `LOOPER_E2E_FORGEJO=1`, `LOOPER_E2E_FORGEJO_BASE_URL`, `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, and `LOOPER_E2E_FORGEJO_TOKEN` set was not fully present in this environment.
- Ran full repository validation with `go test ./... && go vet ./... && go build ./...`.

## Step 6: Documentation Sync

Notes:

- Updated `README.md` and `CONTRIBUTING.md` with the deterministic Forgejo contract/non-live e2e commands, the local/manual Forgejo live sandbox command, and the GitHub sandbox repo env compatibility note.
- Updated `docs/configuration.md` with the Forgejo live sandbox e2e prerequisites and fail-fast behavior for enabled live runs.
- Verified the documentation now names `LOOPER_E2E_FORGEJO`, `LOOPER_E2E_FORGEJO_BASE_URL`, `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, `LOOPER_E2E_FORGEJO_TOKEN`, `LOOPER_E2E_GITHUB_SANDBOX_REPO`, and the legacy `LOOPER_E2E_SANDBOX_REPO` alias.
- Re-ran `go test ./internal/e2e/forgejocontract -count=1` and `go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1` after the docs sync.

## Step 7: Pair-Mode Forgejo Live Sandbox Run

Notes:

- Used repo-local live env layout: committed `e2e/.env.example`, ignored `e2e/.env*`, and kept the real token only in ignored `e2e/.env`.
- Ran live sandbox against `https://code.powerformer.net/core/looper-sandbox` with `source e2e/.env && go test ./internal/e2e -run '^TestForgejoSandbox' -count=1`.
- Classified and fixed a supported-case local test defect: live Forgejo issue creation expects numeric label IDs, so sandbox issue setup now resolves/creates the `looper-e2e` label and sends its ID.
- Classified and fixed a supported-case local test defect: Forgejo worker issues must be pre-assigned to the token user, so sandbox issue setup now sends the current Forgejo user in issue creation assignees.
- Classified and fixed a supported-case provider normalization defect: the live Forgejo compare API returned `total_commits` without `status/ahead_by`, so `ForgejoClient.CompareBranches` now treats `total_commits > 0` with missing ahead fields as an ahead comparison and has regression coverage for slash-containing head branches.
- Reviewer live sandbox coverage is intentionally not added in this step. The current GitHub sandbox mirror exercises reviewer-adjacent behavior through fixer/native review-thread scenarios; those are outside Forgejo MVP and remain skipped. Forgejo MVP's comment-only reviewer is supported and covered by non-live runtime/provider tests, but this spec does not add a Forgejo-specific live reviewer case. When Forgejo later implements real reviewer + fixer/native review-thread behavior, add combined reviewer/fixer live sandbox coverage then.

Verification:

- `go test ./internal/forge -run 'TestCompareBranchesNormalizesForgejoTotalCommitsOnlyResponse|TestForgejoClient' -count=1`
- `go test ./internal/e2e/forgejocontract -count=1`
- `source e2e/.env && go test ./internal/e2e -run '^TestForgejoSandbox' -count=1`

## Step 8: Pair-Mode Forgejo Real-Agent Local Run

Notes:

- Built local real-run binaries with `go build -o dist/looperd ./cmd/looperd` and `go build -o dist/looper ./cmd/looper`.
- Used the ignored repo-local `e2e/.env` for Forgejo sandbox env values and token; no token value was recorded in tracked files.
- Used `codex` as the real local agent command.
- Created an isolated runtime outside the repository at `/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent` with a sandbox clone, separate SQLite DBs, logs, backups, and worktree roots.
- Prepared a worker-only config with Forgejo MVP-supported behavior only: polling, worker label discovery by `looper:worker-ready`, current-user assignment requirement, reviewer discovery disabled, fixer disabled, coordinator disabled, osascript disabled, and auto-merge disabled.
- Validated the worker config with `source e2e/.env && dist/looper --config <config-worker.toml> config validate`.
- Human-created worker issue #10 was open, assigned to `nettee`, and labeled `looper:worker-ready`.
- Started local `looperd` with the worker-only config and real `codex` execution. The real-agent remote path passed: Looper created PR #11, the PR changed `looper-real-agent-worker-smoke.txt`, and the PR head contained `worker real-agent smoke test 2026-06-22`.
- The worker run also exposed a local recovery/idempotency defect after the monitor script stopped `looperd` immediately after PR creation but before local bookkeeping completed. Recovery requeued the interrupted run and then moved worker queue items to `manual_intervention` with `upsert run: UNIQUE constraint failed: runs.loop_id`. This is classified as a local Looper recovery defect induced by shutdown timing, not a Forgejo provider API failure and not a real agent failure.
- Prepared a separate reviewer-only runtime and config so the reviewer run did not reuse the worker DB that preserved recovery-failure evidence.
- Configured Forgejo MVP comment-only reviewer behavior only: polling, label-based discovery by `looper:review`, `requireReviewRequest = false`, self-review allowed for the sandbox PR, clean/blocking events set to `COMMENT`, thread resolution disabled, fixer disabled, worker disabled, coordinator disabled, osascript disabled, and auto-merge disabled.
- Human-prepared PR #11 by adding `looper:review` while leaving the PR open.
- Started local `looperd` with the reviewer-only config and real `codex` execution. The reviewer comment-only path passed: queue item `queue_d1e9b66f56b1394f18cbda973e0d45b9` completed, run `run_7b9e6da08e0dee442d28c9254dbf7b31` succeeded, and Forgejo PR comment ID `50` was created with outcome `clean`.

Conclusion:

- Forgejo MVP is sufficient for a real-agent local worker live run through issue discovery, real agent execution, branch push, and PR creation.
- Forgejo MVP is sufficient for a real-agent local comment-only reviewer live run through label discovery, real agent review, and normal Forgejo PR comment publication.
- Forgejo MVP is still not sufficient for fixer, coordinator/dependency gates, native review requests, inline review threads, thread resolution, auto-merge, or webhook/routed mode.
- Real-agent e2e should wait for queue/run terminal state before stopping `looperd`; stopping immediately after observing a remote side effect can expose or create ambiguous local recovery state.

Artifacts:

- Worker issue: `https://code.powerformer.net/core/looper-sandbox/issues/10`
- Worker PR: `https://code.powerformer.net/core/looper-sandbox/pulls/11`
- Reviewer comment: Forgejo PR comment ID `50` on PR #11.
- Detailed execution record: `journal.md`.
- Operational write-up: `real-agent-e2e.md`.
