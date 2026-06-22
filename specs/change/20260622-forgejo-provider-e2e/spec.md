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

## Progress

- [x] Step 1: Mirror Inventory And Env Compatibility
- [x] Step 2: Forgejo Contract E2E
- [x] Step 3: Forgejo Live Sandbox Mirror
- [x] Step 4: Copy-Run-Classify Supported Cases
- [x] Step 5: EAG Validation
- [ ] Step 6: Documentation Sync

## Implementation

See [steps.md](./steps.md).

## Deferred Follow-Ups (DFU)

None.
