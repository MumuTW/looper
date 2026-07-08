---
id: 20260707-forgejo-native-review-comment-resolution
name: Forgejo Native Review Comment Resolution
status: draft
created: '2026-07-07'
---

## Overview

Teach Looper's **manual Forgejo Fixer** path to consume and resolve native Forgejo PR review comments by default for Forgejo projects.

The existing Forgejo Reviewer/Fixer loop remains summary-comment based by default. This change adds a narrow native path for Forgejo instances that expose comment-level review resolution through `POST /repos/{owner}/{repo}/pulls/{index}/comments/{id}/resolve` and a review-comment `resolver` field. The change does not attempt GitHub thread parity: Forgejo's reliable repair authority is the individual review comment, not a first-class thread resource.

Version policy for this spec: Looper assumes supported Forgejo projects run a sufficiently new Forgejo build with native review comment resolution. Native comment resolution is therefore **default on** for manual Forgejo Fixer. If a Forgejo instance lacks the required `resolver` field or returns 404/405 from the resolve endpoint, Looper reports an unsupported-capability error instead of silently falling back. There is no runtime capability probing in this first slice.

## Research

See [design.md](./design.md).

## Design

### Design Summary

For Forgejo projects, manual Fixer fetches native PR review comments by default, treats comments with a present `resolver: null` as unresolved repair items, and includes them alongside existing Reviewer Summary `open` items. The fixer agent must return structured results for native comment items using provider comment IDs and an observed fingerprint. After the existing validation and push safety flow succeeds, Looper re-reads Forgejo and resolves only native comments that are still unresolved, unchanged, and reported as `fixed` by the agent.

Reviewer Summary remains the authority for Looper-authored summary items. Forgejo native review comments are separate provider-authority items. V1 performs no semantic deduplication between summary items and native comments; each item keeps its own source ID.

This spec does not add a new persisted authority for current open/resolved state. Any stored handled IDs or fingerprints are audit/retry inputs only; every side-effecting resolve action re-reads Forgejo first.

See [design.md](./design.md) for design detail.

### E2E Acceptance Gate (EAG)

Acceptance behavior: for a Forgejo project, manual Fixer reads unresolved native review comments by default, repairs the PR, pushes the fix, resolves only fixed unchanged native comments through Forgejo's comment resolve endpoint, and leaves unrelated, declined, deferred, changed, deleted, already-resolved, or unhandled comments unresolved. Existing summary-protocol behavior remains available for Reviewer Summary items, but unsupported native capability is a clear failure rather than a silent downgrade.

Verification path:

- Contract tests with a fake Forgejo HTTP server prove review-comment listing, presence-aware `resolver` decoding, resolve endpoint calls, 404/405 unsupported-capability errors, and no resolve before validation/push success.
- Integration tests prove manual Forgejo Fixer builds repair input from unresolved native comments by default plus existing Reviewer Summary `open` items, with distinct source IDs and no semantic deduplication.
- Safety tests prove Looper resolves only `fixed` native comments whose observed fingerprint still matches after a post-push re-read.
- Sandbox E2E is manual/conditional like the existing Forgejo sandbox: run only against a Forgejo instance known to expose native comment resolution, create a PR with at least one unresolved native review comment, run manual Fixer, verify the comment is resolved, verify already-resolved comments are ignored, and clean up sandbox artifacts.
- `go test ./...`, `go vet ./...`, and `go build ./...` pass.

## Plan

### Step 1 (AFK): Forgejo Review Comment API

Goal: Add only the Forgejo REST API surface needed by native Fixer resolution.

Scope: Implement list PR review comments and resolve review comment. Decode `id`, `body`, `user`, presence-aware `resolver`, `path`, `commit_id`, `original_commit_id`, `position`, `original_position`, `diff_hunk`, `html_url`, `pull_request_review_id`, and an update timestamp if Forgejo returns one. Treat absent `resolver` as unsupported native resolution. Handle pagination, 404/405 unsupported endpoint responses, sanitized errors, and fake-server contract tests. Do not implement unresolve in this change.

Depends on: None

### Step 2 (AFK): Default-On Compatibility Gate

Goal: Make native Forgejo comment resolution the default manual Fixer behavior while failing clearly on unsupported Forgejo instances.

Scope: Update Forgejo profile/capability docs so manual Fixer collects native review comments by default. Keep GitHub thread-resolution config unchanged. If the provider lacks required fields before repair starts, fail with a non-retryable unsupported-capability error. If the resolve endpoint returns 404/405 after a safe push, surface manual intervention/provider-ack failure rather than reporting plain success.

Depends on: Step 1

### Step 3 (AFK): Minimal Fixer Repair Item Extension

Goal: Represent Forgejo native comments in Fixer without overbuilding a new cross-provider abstraction.

Scope: First attempt to extend/reuse the existing Fixer item model with source, provider comment ID, observed fingerprint, path, body, diff hunk, and URL. Introduce a new provider-neutral model only if the existing model cannot represent these fields cleanly, and document that diff-based justification. Preserve GitHub behavior and prompts.

Depends on: Step 2

### Step 4 (AFK): Manual Forgejo Fixer Native Comment Consumption

Goal: Include unresolved native comments in manual Forgejo Fixer prompts by default.

Scope: At Fixer claim time, fetch fresh Forgejo review comments, require the `resolver` field to be present, filter to `resolver == null`, exclude comments authored by the current Looper provider identity to avoid self-repair loops, and include native comment items in the prompt with Forgejo-specific terminology. Keep Reviewer Summary items as a separate source. V1 performs no semantic deduplication except exact duplicate native provider comment IDs.

Depends on: Step 3

### Step 5 (AFK): Structured Native Comment Results

Goal: Make side-effecting resolve decisions depend on a precise agent result contract.

Scope: Define and validate Forgejo-specific structured output such as `repair_results: [{source: "forgejo_review_comment", providerCommentId, action: "fixed"|"declined"|"deferred", explanation, observedFingerprint}]`. Require one result per native candidate or define missing results as `deferred`. Resolve eligibility is limited to `action == "fixed"`.

Depends on: Step 4

### Step 6 (AFK): Safe Resolve After Successful Repair

Goal: Resolve only fixed native comments after code is safely pushed.

Scope: After agent success, validation success, and push success, re-fetch candidate comments, confirm each fixed comment is still present, still unresolved, and still matches the observed fingerprint, then call Forgejo resolve. Already-resolved comments are skipped as success. Deleted/missing comments are skipped with an audit note. 5xx/timeouts are retryable provider-ack failures for the resolve step. 404/405 unsupported responses are non-retryable unsupported-capability/manual-intervention failures. Declined, deferred, changed, unrelated, and unhandled comments remain unresolved.

Depends on: Step 5

### Step 7 (AFK): EAG Validation And Documentation

Goal: Validate the completed narrow slice and update docs for the chosen policy.

Scope: Run contract/integration tests, run manual sandbox E2E when a compatible Forgejo instance is available, record exact commands and observed provider state in `steps.md`, update `docs/configuration.md` and `docs/users-guide.md` to describe default native resolution, unsupported-capability behavior, manual Fixer scope, and the difference between Forgejo comment resolution and GitHub thread resolution.

Depends on: Step 6

## Progress

- [x] Step 1 (AFK): Forgejo Review Comment API
- [x] Step 2 (AFK): Default-On Compatibility Gate
- [x] Step 3 (AFK): Minimal Fixer Repair Item Extension
- [x] Step 4 (AFK): Manual Forgejo Fixer Native Comment Consumption
- [x] Step 5 (AFK): Structured Native Comment Results
- [x] Step 6 (AFK): Safe Resolve After Successful Repair
- [x] Step 7 (AFK): EAG Validation And Documentation

## Implementation

See [steps.md](./steps.md).

## Deferred Follow-Ups (DFU)

- Forgejo Reviewer native inline comment publishing.
- Forgejo Fixer auto-discovery/scheduler support for native comment repair.
- Unresolve API support.
- Gitea provider support.
- Optional compatibility escape hatch for old Forgejo installs, if real users need it.
- Runtime capability probing or a declared minimum supported Forgejo release once release/version data is pinned.
- Broader GitHub/Forgejo repair-item terminology cleanup if `thread` remains embedded in internal or user-facing messages for Forgejo comment repair items.
