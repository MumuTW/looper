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
