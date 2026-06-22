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

## Step 3: Forgejo Live Sandbox Mirror

Notes:

## Step 4: Copy-Run-Classify Supported Cases

Case failure classifications:

## Step 5: EAG Validation

Notes:

## Step 6: Documentation Sync

Notes:
