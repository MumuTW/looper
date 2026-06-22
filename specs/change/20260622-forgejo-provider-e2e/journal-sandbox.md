# Journal

## 2026-06-22: Step 7 pair-mode start and AFK preflight

Status: blocked on Human operations for live sandbox inputs.

AI operations completed:

- Added Step 7 pair-mode plan to `spec.md`, splitting AI operations from Human operations.
- Ran deterministic Forgejo contract preflight: `go test ./internal/e2e/forgejocontract -count=1` — passed.
- Ran non-live Forgejo e2e preflight: `go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1` — passed.

Next Human operations required:

1. Provide `LOOPER_E2E_FORGEJO_BASE_URL` for the live Forgejo instance.
2. Provide `LOOPER_E2E_FORGEJO_SANDBOX_REPO` in `owner/repo` form for a dedicated existing sandbox repository.
3. Provide `LOOPER_E2E_FORGEJO_TOKEN` for a token authorized to read the current user/repository, manage sandbox issues/PRs/labels/comments, and push/delete test branches over HTTPS.

## 2026-06-22: Step 7 sandbox repository provided

Status: blocked on Human operation for Forgejo token creation.

Human-provided live sandbox inputs:

- `LOOPER_E2E_FORGEJO_BASE_URL=https://code.powerformer.net`
- `LOOPER_E2E_FORGEJO_SANDBOX_REPO=core/looper-sandbox`
- Sandbox repository URL: `https://code.powerformer.net/core/looper-sandbox`

Next Human operation required:

1. Create a Forgejo access token for the sandbox run.
2. Provide `LOOPER_E2E_FORGEJO_TOKEN` for this shell/session only; do not commit it to any file.

## 2026-06-22: Step 7 token storage guidance

Status: blocked on Human operation to place the token in a local-only env file and confirm the path.

AI guidance provided:

- Do not store the token in a repo-root `.env` because `.env` is not currently ignored by this repository.
- Preferred local-only path: `~/.looper/forgejo-e2e.env` with owner-only permissions.
- The env file should export `LOOPER_E2E_FORGEJO`, `LOOPER_E2E_FORGEJO_BASE_URL`, `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, and `LOOPER_E2E_FORGEJO_TOKEN`.
- No token value was recorded in the journal.

## 2026-06-22: Step 7 repo-local env layout selected

Status: blocked on Human operation to copy the template and paste the token into the ignored local env file.

Human direction:

- Store the local live e2e env file inside the repository under an `e2e/` directory.
- Add a committed `e2e/.env.example` template.

AI operations completed:

- Added `e2e/.env` to `.gitignore` so the real token file is not committed.
- Added `e2e/.env.example` with the Forgejo sandbox base URL and repo plus a token placeholder.
- No token value was recorded in the journal or committed files.

Follow-up AI operation completed:

- Strengthened `.gitignore` to ignore `e2e/.env*` while explicitly allowing `e2e/.env.example`, so local token variants remain untracked but the template stays committed.

Next Human operation required:

1. Copy `e2e/.env.example` to `e2e/.env`.
2. Replace `replace-with-forgejo-token` with the real token.
3. Tell the AI when `e2e/.env` is ready.

## 2026-06-22: Step 7 live sandbox execution

Status: completed.

Human operations completed:

- Created `e2e/.env` from the committed template with the live Forgejo token.
- Confirmed `e2e/.env` was ready.

AI operations completed:

- Verified `e2e/.env` is ignored and not tracked by git.
- Ran initial live sandbox command: `source e2e/.env && go test ./internal/e2e -run '^TestForgejoSandbox' -count=1`.
- Classified the first live failure as a supported-case local test defect: the live Forgejo issue-create API expects label IDs, not label names, for `CreateIssueOption.labels`.
- Fixed `createForgejoSandboxIssue` to ensure the sandbox label and send its numeric label ID when creating issues.
- Classified the second live failure as a supported-case local test defect: Forgejo worker issues must be pre-assigned to the current user, and the live instance accepted assignees on issue creation rather than the separate assignee route.
- Fixed `createForgejoSandboxIssue` to send `assignees` with the current Forgejo user during issue creation.
- Classified the final live failure as a supported-case Forgejo provider normalization defect: the live Forgejo compare API returned `total_commits` plus commit/file data without `status` or `ahead_by`, so Looper misclassified an ahead branch as no-diff.
- Fixed `ForgejoClient.CompareBranches` to normalize a `total_commits > 0` response with missing `status/ahead_by` to an ahead comparison, and added a unit regression test for slash-containing head branches.

Verification completed:

- `go test ./internal/forge -run 'TestCompareBranchesNormalizesForgejoTotalCommitsOnlyResponse|TestForgejoClient' -count=1` — passed.
- `go test ./internal/e2e/forgejocontract -count=1` — passed.
- `source e2e/.env && go test ./internal/e2e -run '^TestForgejoSandbox' -count=1` — passed.
