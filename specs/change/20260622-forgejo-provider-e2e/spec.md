---
id: 20260622-forgejo-provider-e2e
name: Forgejo Provider E2e
status: planned
created: '2026-06-22'
---

## Overview

PR 507 added the Forgejo provider MVP. Forgejo is now expected to become a first-class provider alongside GitHub, so provider parity needs contract end-to-end and live end-to-end coverage rather than only MVP fake-provider coverage.

### Goals

- Add Forgejo contract e2e coverage modeled on the existing GitHub e2e suite.
- Add Forgejo live e2e coverage that exercises a real Forgejo provider.
- First copy the GitHub e2e shape closely enough to run, then use the failures to separate incomplete MVP scope from actual defects.
- Preserve GitHub behavior while raising Forgejo to a parallel provider test posture.

### Constraints

- Follow the existing GitHub e2e patterns before introducing new Forgejo-specific abstractions.
- Keep failures observable: missing Forgejo support, config, credentials, API behavior, or test prerequisites should fail clearly rather than falling back to mock data.
- Record confirmed facts with sources during research and design.

## Research

See [design.md](./design.md).

## Design

### Design Summary

Mirror the existing GitHub e2e posture for Forgejo instead of inventing a reduced Forgejo-only suite. Forgejo gets counterparts for the GitHub contract and live sandbox cases; supported provider behavior runs, unsupported MVP behavior remains present with explicit `t.Skip` reasons, and only GitHub test tooling or CI mechanics may have no Forgejo counterpart.

Forgejo contract e2e uses the same strict-boundary idea as GitHub contract e2e, but the contract object changes from `gh` CLI behavior to Forgejo REST API behavior. Forgejo live e2e uses a real dedicated Forgejo sandbox repository and remains local/manual for this spec; no GitHub Actions workflow job is added.

See [design.md](./design.md) for design detail.

### E2E Acceptance Gate (EAG)

Acceptance behavior: Forgejo has a mirrored e2e posture beside GitHub: deterministic contract tests validate Forgejo REST API contracts with strict fake boundaries, live sandbox tests are available behind local opt-in env vars, unsupported MVP cases are present with explicit skip reasons, and GitHub sandbox repo env compatibility remains fail-fast.

Verification path: `go test ./internal/e2e/forgejocontract -count=1` plus the relevant non-live Forgejo e2e package tests. When a real Forgejo sandbox is configured, `LOOPER_E2E_FORGEJO=1 ... go test ./internal/e2e -run '^TestForgejoSandbox' -count=1` proves the opt-in live path.

## Plan

### Step 1: Mirror Inventory And Env Compatibility

Type: AFK
Goal: Establish the full GitHub-to-Forgejo e2e mirror surface before filling in behavior.
Scope: Enumerate GitHub contract/live/dependency-gate e2e cases in Forgejo counterpart files or tables, mark run/skip/no-counterpart intent, add `LOOPER_E2E_GITHUB_SANDBOX_REPO` with `LOOPER_E2E_SANDBOX_REPO` compatibility and conflict failure, and define Forgejo live env parsing.
Depends on: None
Acceptance criteria: Every existing GitHub e2e case has a Forgejo counterpart or a documented no-counterpart reason; GitHub sandbox repo env ambiguity fails fast.

### Step 2: Forgejo Contract E2E

Type: AFK
Goal: Add deterministic Forgejo contract e2e that mirrors GitHub contract e2e using the real Forgejo REST boundary.
Scope: Create `internal/e2e/forgejocontract`, add strict fake Forgejo HTTP request recording, assert method/path/query/auth/body/pagination/error behavior, run supported REST-mapped cases, and skip unsupported counterpart slices with explicit reasons.
Depends on: Step 1
Acceptance criteria: Each strict fake Forgejo route has a documented authority source from official docs, instance OpenAPI, MVP capability, or recorded live observation; `go test ./internal/e2e/forgejocontract -count=1` passes without real network.

### Step 3: Forgejo Live Sandbox Mirror

Type: AFK
Goal: Add local/manual Forgejo live sandbox e2e with the same posture as GitHub sandbox e2e.
Scope: Add Forgejo sandbox config parsing, HTTPS clone/push URL derivation from base URL/repo/token, run-specific artifact naming and cleanup, enabled worker/no-diff supported cases, and skipped fixer/dependency/coordinator counterparts.
Depends on: Step 1, Step 2
Acceptance criteria: Without `LOOPER_E2E_FORGEJO=1`, live Forgejo tests skip; with it enabled, missing or invalid prerequisites fail clearly.

### Step 4: Copy-Run-Classify Supported Cases

Type: AFK
Goal: Use the mirrored suite to distinguish unsupported MVP scope from actual Forgejo defects.
Scope: Run deterministic tests and any configured live sandbox tests, fix supported-case failures in this spec, convert true unsupported MVP cases to explicit skips, and record case failures/classifications/fixes in `steps.md`.
Depends on: Step 2, Step 3
Acceptance criteria: No supported Forgejo case is skipped merely to get green; `steps.md` records the observed failure classifications and outcomes.

### Step 5: EAG Validation

Type: AFK
Goal: Validate the completed change against the Spec's EAG before wrap-up work.
Scope: Run `go test ./internal/e2e/forgejocontract -count=1`, the relevant non-live Forgejo e2e package tests, and the opt-in live Forgejo command when credentials are available; then run `go test ./...`, `go vet ./...`, and `go build ./...`.
Depends on: Step 4

### Step 6: Documentation Sync

Type: AFK
Goal: Keep project documentation aligned with the implemented e2e behavior.
Scope: If the implementation changes documented test commands, environment variables, live sandbox setup, or provider support, update relevant project docs.
Depends on: Step 5

### Step 7: Pair-Mode Forgejo Live Sandbox Run

Type: Pair
Goal: Run the opt-in Forgejo live sandbox e2e against a real dedicated Forgejo repository, with AI operations done autonomously and human operations isolated to required manual or risky actions.
Scope: Prepare and validate the local test command, ask the human to provide only the required live Forgejo sandbox inputs, run deterministic preflight checks, run `go test ./internal/e2e -run '^TestForgejoSandbox' -count=1` with the provided live env, classify any failure as missing prerequisite, unsupported MVP behavior, live provider defect, or local test defect, and record the result in `steps.md`.
Depends on: Step 6
Acceptance criteria: The live sandbox command either passes, or fails fast with a clear recorded cause and no hidden fallback/mock behavior; all human-required actions are documented as Human operations before they are needed.

#### Step 7 Pair Operations Plan

AI operations:

1. Confirm the deterministic contract gate is still green with `go test ./internal/e2e/forgejocontract -count=1` before touching the live sandbox path.
2. Confirm the non-live Forgejo e2e entrypoints still compile and skip/validate as expected with `go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1`.
3. Prepare the exact live command using only the human-provided `LOOPER_E2E_FORGEJO_BASE_URL`, `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, and `LOOPER_E2E_FORGEJO_TOKEN` values; do not invent placeholder credentials or repos.
4. Run the live command once prerequisites are provided: `LOOPER_E2E_FORGEJO=1 LOOPER_E2E_FORGEJO_BASE_URL=... LOOPER_E2E_FORGEJO_SANDBOX_REPO=... LOOPER_E2E_FORGEJO_TOKEN=... go test ./internal/e2e -run '^TestForgejoSandbox' -count=1`.
5. If the live command fails, inspect only local test output and repository code needed to classify the failure; do not mutate the Forgejo instance outside the test's own run-scoped behavior.
6. Append the observed live run command shape, pass/fail status, and failure classification to `steps.md`.
7. Report the final result and any next required human decision.

Human operations:

1. Choose or create a dedicated existing Forgejo sandbox repository in `owner/repo` form. It must be safe for tests to create and clean run-scoped issues, branches, pull requests, labels, and comments.
2. Create or choose a Forgejo token for that sandbox. The token must be allowed to read the current user, read the repository, list/create/update issues, list/create/update pull requests, create labels/comments, and push/delete test branches over HTTPS.
3. Provide these values to the AI when asked:
   - `LOOPER_E2E_FORGEJO_BASE_URL`, for example `https://code.example.com`
   - `LOOPER_E2E_FORGEJO_SANDBOX_REPO`, for example `owner/repo`
   - `LOOPER_E2E_FORGEJO_TOKEN`, preferably pasted only for the current shell/session and not committed to any file
4. If Forgejo prompts, token scopes, branch protection, repository permissions, or network access block the test, perform the required Forgejo-side permission/configuration change manually and tell the AI what changed.
5. Review any leftover sandbox artifacts if the live test aborts before cleanup; deleting remote branches, issues, PRs, labels, or comments manually is a Human operation unless you explicitly authorize the AI to perform that cleanup.

### Step 8: Pair-Mode Forgejo Real-Agent Local Run

Type: Pair
Goal: Run Looper from a local build against the real Forgejo sandbox with real agent execution, first for the Forgejo MVP worker path and then for the Forgejo MVP comment-only reviewer path.
Scope: Build local `looperd`/`looper` binaries, prepare an isolated runtime/config that points at the existing Forgejo sandbox and a real local agent command, run only Forgejo-supported MVP features, trigger one assigned worker issue and one comment-only reviewer case, record exact commands/results in `steps.md`, and stop on missing credentials, invalid config, unsupported Forgejo feature use, or agent execution failure.
Depends on: Step 7
Acceptance criteria: Local `looperd` starts from the isolated config, uses a real agent command instead of the e2e fake agent, successfully completes the worker live run or fails with a classified cause, and separately completes the reviewer comment-only live run or fails with a classified cause. No token values are committed or written to tracked files.

#### Step 8 Pair Operations Plan

AI operations:

1. Build local binaries with `go build -o dist/looperd ./cmd/looperd` and `go build -o dist/looper ./cmd/looper`.
2. Prepare an ignored/local runtime config for the real-agent Forgejo run using the existing sandbox base URL/repo and token source from `e2e/.env`, without copying token values into tracked files or logs.
3. Configure only Forgejo MVP-supported behavior: polling mode, worker enabled, comment-only reviewer enabled for the reviewer case, and fixer/coordinator/dependency gates/webhook/routed/native review-thread/auto-merge disabled.
4. Configure the real agent command from the local environment (`opencode`, `codex`, `claude`, or an explicit `agent.params.command`) and fail fast if the command is missing or not executable.
5. Run deterministic preflight checks: validate config, confirm `git` resolves, confirm Forgejo token can identify the current user/repo through the supported Looper path, and confirm runtime directories are writable.
6. For the worker case, provide the human with exact Forgejo-side issue creation/assignment instructions, then start local `looperd --config <local-config>` and observe whether Looper invokes the real agent, commits, pushes a branch, opens/updates a PR, and comments back to Forgejo.
7. For the reviewer case, provide the human with exact Forgejo-side PR preparation/trigger instructions, then run local `looperd --config <local-config>` with only comment-only reviewer behavior enabled and observe whether Looper invokes the real reviewer agent and writes a normal Forgejo comment.
8. Classify any failure as missing prerequisite, invalid local config, unsupported Forgejo MVP behavior, real agent execution failure, live Forgejo provider defect, or local Looper defect.
9. Append command shapes, pass/fail status, artifact links/IDs, and failure classifications to `steps.md` without recording secrets.
10. Commit the plan/progress with a `journal:` commit after each completed step or significant progress, and push only when explicitly requested.

Human operations:

1. Confirm which real agent command should be used locally (`opencode`, `codex`, `claude`, or another command) and ensure it is authenticated and safe to run against the sandbox worktree.
2. Confirm the existing ignored `e2e/.env` still contains the live Forgejo sandbox values and token, or update it locally. Do not paste the token into tracked files.
3. For the worker case, create a small Forgejo sandbox issue and assign it to the token user. The issue should request a tiny, low-risk repository change, such as adding one clearly named line to a sandbox test file or README section.
4. Review and approve the worker issue prompt before `looperd` is started, because the real agent will act on that prompt and may push commits to the sandbox repository.
5. For the reviewer case, prepare or approve a small Forgejo sandbox PR with a harmless diff that the reviewer can inspect. The expected reviewer output is a normal PR comment only, not inline review threads or approvals.
6. If Forgejo permissions, branch protection, token scopes, agent login, local network access, or repository state block the run, make the required manual change and tell the AI what changed.
7. Decide whether any leftover remote sandbox branches, PRs, labels, issues, or comments should be manually cleaned up. Remote destructive cleanup is a Human operation unless explicitly authorized.

## Progress

- [x] Step 1: Mirror Inventory And Env Compatibility
- [x] Step 2: Forgejo Contract E2E
- [x] Step 3: Forgejo Live Sandbox Mirror
- [x] Step 4: Copy-Run-Classify Supported Cases
- [x] Step 5: EAG Validation
- [x] Step 6: Documentation Sync
- [x] Step 7: Pair-Mode Forgejo Live Sandbox Run
- [x] Step 8: Pair-Mode Forgejo Real-Agent Local Run

## Implementation

See [steps.md](./steps.md).

## Deferred Follow-Ups (DFU)

None.
