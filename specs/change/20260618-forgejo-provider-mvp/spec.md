---
id: 20260618-forgejo-provider-mvp
name: Forgejo Provider Mvp
status: implemented
created: '2026-06-18'
---

## Overview

Implement the Forgejo provider MVP defined by #505.

### Problem Statement

Looper is currently GitHub/`gh` shaped for forge operations. The MVP must let Looper drive useful Forgejo workflows without requiring `gh`, while preserving existing GitHub behavior.

### Goals

- Add explicit provider support for `github` and `forgejo` projects.
- Support Forgejo planner and worker end-to-end.
- Support a reduced Forgejo reviewer that publishes comment-only reviews.
- Keep local repository/worktree operations on `git`.
- Fail fast when a Forgejo project enables unsupported GitHub-only behavior.

### Scope

- Forgejo only; Gitea remains deferred.
- Config-driven Forgejo project registration with explicit provider, base URL, token source, and repo.
- Provider capabilities, Forgejo profile defaults, per-project provider resolution, REST client, scheduler/role integration, fake-server tests, and startup validation.
- Preserve the existing GitHub `gh`-backed path.

### Non-goals

- Native Forgejo reviews, review-thread resolution, fixer, coordinator, auto-merge, branch protection, managed webhooks, routed network mode, and Gitea provider support.

### Success Criteria

- Forgejo-only installs boot and recover without `gh`.
- Existing GitHub projects continue unchanged.
- Fake-provider integration covers Forgejo planner, worker, and comment-only reviewer behavior.
- `go test ./...`, `go vet ./...`, and `go build ./...` pass.

## Research

See [design.md](./design.md).

## Design

### Design Summary

Introduce a provider boundary with a transitional GitHub wrapper and a Forgejo REST provider. Resolve provider/repo per project from config, keep existing GitHub autodetect behavior as a bounded compatibility exception, and reject duplicate bare repos instead of migrating storage in the MVP.

Forgejo uses a static capability table and a source-aware provider profile so minimal Forgejo configs validate safely while explicit opt-ins to unsupported GitHub-only behavior fail fast. Runtime, scheduler, recovery, and role adapters become provider-aware; Forgejo planner/worker/reviewer run without GitHub discovery snapshots, with reviewer reduced to comment-only publishing.

See [design.md](./design.md) for design detail.

### E2E Acceptance Gate (EAG)

Acceptance behavior: a Forgejo-only config with no `ghPath` boots through startup/recovery, validates provider/profile rules, and fake-provider integration proves planner, pre-assigned worker, and comment-only reviewer flows without GitHub discovery snapshots or `gh` prompt contracts.

Verification path: `go test ./...` including the new fake Forgejo integration/startup/prompt-contract tests, followed by `go vet ./...` and `go build ./...`.

## Plan

### Step 1: Provider Config And Contracts

Type: AFK
Goal: Establish the provider model without changing GitHub behavior.
Scope: Add provider/project config fields, normalized repository references, capability table, provider registry, source-aware Forgejo profile, duplicate bare repo validation, and a transitional GitHub wrapper over the existing gateway.
Depends on: None
Acceptance criteria: Existing GitHub configs still validate; minimal Forgejo config validates; explicit unsupported Forgejo feature opt-ins fail with clear errors.

### Step 2: Provider-Aware Runtime And Scheduler

Type: AFK
Goal: Remove hidden global GitHub assumptions from startup, recovery, and scheduling paths.
Scope: Resolve providers per project in runtime/scheduler/recovery, make `gh` bootstrap conditional on GitHub projects, gate network/webhook/coordinator paths by provider capability, and keep GitHub discovery snapshots GitHub-only.
Depends on: Step 1
Acceptance criteria: Forgejo-only startup/recovery tests pass with no `ghPath`; mixed GitHub webhook plus Forgejo polling config remains valid.

### Step 3: Forgejo REST Client MVP

Type: AFK
Goal: Provide the Forgejo API surface needed by planner, worker, and reduced reviewer.
Scope: Implement REST client auth, pagination, typed issue/PR/label/assignee/comment/identity methods, PR diff/metadata support, timeouts, and sanitized error handling with fake-server contract tests.
Depends on: Step 1

### Step 4: Forgejo Planner And Worker Enablement

Type: AFK
Goal: Deliver useful Forgejo planner and worker flows without GitHub discovery snapshots.
Scope: Wire Forgejo role adapters into scheduler discovery/processing, make planner publish spec PRs and labels, make worker process only pre-assigned current-user issues, re-check assignment before side effects, and update provider-specific prompts.
Depends on: Step 2, Step 3
Acceptance criteria: Fake-provider integration proves planner PR creation and worker PR creation; tests prove unassigned or de-assigned worker issues are skipped and no self-assignment claim is attempted.

### Step 5: Forgejo Comment-Only Reviewer

Type: AFK
Goal: Enable the reduced Forgejo reviewer while keeping disabled review features explicit.
Scope: Add label-based reviewer discovery, provider-supplied PR metadata/diff prompt contracts, comment-only publish, local-record head-SHA idempotency, label mutation, and validation/prompt tests for disabled native review/review-request/thread behavior.
Depends on: Step 4
Acceptance criteria: Reviewer publishes one comment after successful local publish record for a PR head SHA; Forgejo reviewer prompts contain no `gh pr view`, `gh pr diff`, `gh api`, review-request, or native-review instructions.

### Step 6: EAG Validation

Type: AFK
Goal: Validate the completed change against the Spec's EAG before wrap-up work.
Scope: Run `go test ./...` including fake Forgejo integration/startup/prompt-contract tests, then `go vet ./...` and `go build ./...`; confirm the EAG acceptance behavior in the Design section is covered.
Depends on: Step 5

### Step 7: Documentation Sync

Type: AFK
Goal: Keep project documentation aligned with implemented provider behavior.
Scope: If implementation changes documented config, commands, startup behavior, provider support, or role limitations, update relevant project docs and references.
Depends on: Step 6

## Progress

- [x] Step 1: Provider Config And Contracts
- [x] Step 2: Provider-Aware Runtime And Scheduler
- [x] Step 3: Forgejo REST Client MVP
- [x] Step 4: Forgejo Planner And Worker Enablement
- [x] Step 5: Forgejo Comment-Only Reviewer
- [x] Step 6: EAG Validation
- [x] Step 7: Documentation Sync

## Implementation

See [steps.md](./steps.md).

## Deferred Follow-Ups (DFU)

None.
