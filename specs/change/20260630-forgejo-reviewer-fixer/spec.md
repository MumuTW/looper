---
id: 20260630-forgejo-reviewer-fixer
name: Forgejo Reviewer Fixer
status: implemented
created: '2026-06-30'
---

## Overview

Forgejo provider MVP intentionally stopped at comment-only Reviewer support and left Fixer out of scope. This change closes the Forgejo Reviewer/Fixer loop without relying on review-thread resolve APIs, because the target Forgejo instance does not expose a usable resolve/unresolve path.

The protocol is summary-comment based: Reviewer maintains a Reviewer Summary of the current open review items, and Fixer maintains a Fixer Summary for what it attempted against that Reviewer Summary. Reviewer, not Fixer, decides on the next round whether an item is resolved, still open, or superseded.

## Research

See [design.md](./design.md).

## Design

### Design Summary

Use two Looper-maintained top-level PR comments as the Forgejo Reviewer/Fixer protocol state. Reviewer Summary is the Fixer input Authority; Fixer Summary is the Fixer output audit record. Both summaries store versioned machine-readable JSON inside Looper HTML comments, while visible Markdown is only an auxiliary human view.

Reviewer Summary keeps the PR-local Review Item history. Reviewer owns Review Item identity and status: `open`, `resolved`, or `superseded`. Fixer consumes only current `open` items and reports one result per item: `fixed`, `declined`, or `deferred`. Fixer never closes review items; the next Reviewer round re-evaluates the branch state and updates Reviewer Summary.

Forgejo uses this summary-comment + no-resolve protocol by design. Native Forgejo reviews, inline comment state, thread replies, and resolve APIs are not part of the lifecycle protocol unless Forgejo later supports usable resolve semantics. Existing Fixer trigger/claim mechanics remain unchanged; after claim, Reviewer Summary is the only repair-work Authority.

See [design.md](./design.md) for design detail.

### E2E Acceptance Gate (EAG)

Acceptance behavior: contract E2E proves the summary-comment protocol invariants, and sandbox E2E proves the same loop against the dedicated Forgejo sandbox repository `https://code.powerformer.net/core/looper-sandbox`.

Verification path:

- Contract E2E: run the automated contract/integration tests that prove Reviewer writes one Reviewer Summary, Fixer consumes only Reviewer Summary `open` items, Fixer writes one Fixer Summary, no native review/thread resolve mutation occurs, and a later Reviewer round records `resolved` / `open` / `superseded`.
- Sandbox E2E: create a PR in `https://code.powerformer.net/core/looper-sandbox`, run Looper Reviewer and Fixer against it, verify the two fixed summary comments and no-resolve behavior on the real Forgejo instance, then clean up the sandbox artifacts. Follow the real-agent sandbox operating pattern in `specs/change/20260622-forgejo-provider-e2e/real-agent-e2e.md`.
- EAG execution log: record what was run, what was observed, and what issues were fixed while validating the gate in `steps.md`.

## Plan

### Step 1 (AFK): Summary Protocol Core

Goal: Add the shared Forgejo summary protocol model and parser/renderer used by Reviewer and Fixer.
Scope: Implement v1 Reviewer Summary and Fixer Summary structs, HTML-comment JSON extraction/rendering, schema validation, marker uniqueness checks, enum validation, Review Item ID invariants, and focused unit tests.
Depends on: None

### Step 2 (AFK): Reviewer Summary Publishing

Goal: Make Forgejo Reviewer publish and update the fixed Reviewer Summary comment instead of relying on freeform comment-only review output as the protocol state.
Scope: Update the Forgejo Reviewer prompt/output contract, map reviewer findings into Review Items, preserve historical items, reuse Review Item IDs for unchanged semantic issues, mark resolved/superseded items, upsert the single Reviewer Summary comment, and test idempotency/error cases.
Depends on: Step 1

### Step 3 (AFK): Fixer Summary Consumption And Publishing

Goal: Make Forgejo Fixer consume Reviewer Summary open items and publish the no-resolve Fixer Summary.
Scope: Allow the Forgejo Fixer path through existing trigger/claim mechanics, construct Fixer inputs only from Reviewer Summary `open` items, validate one agent result per item, publish/update the single Fixer Summary comment, no-op on zero open items, fail on protocol errors, and ensure no native review/thread resolve/reply mutation occurs.
Depends on: Step 1

### Step 4 (AFK): Forgejo Role Integration And Validation

Goal: Integrate the summary protocol into Forgejo role behavior while preserving unsupported-feature validation.
Scope: Adjust provider/profile/config validation only as needed for the no-resolve Fixer path, keep auto-discovery/native reviews/review requests/thread resolution disabled, wire provider top-level comment operations where needed, and add contract E2E coverage for Reviewer -> Fixer -> Reviewer rounds.
Depends on: Step 2, Step 3

### Step 5 (AFK): EAG Validation

Goal: Validate the completed change against the Spec's EAG.
Scope: Run the contract E2E suite and the real Forgejo sandbox E2E against `https://code.powerformer.net/core/looper-sandbox`, using `specs/change/20260622-forgejo-provider-e2e/real-agent-e2e.md` as the operational reference for isolated runtime, config validation, bounded daemon observation, artifact recording, and cleanup discipline. In `steps.md`, record the exact commands/workflows run, sandbox PR/artifacts used, observed summary comments and no-resolve behavior, cleanup status, and every issue discovered/fixed during EAG validation.
Depends on: Step 4

### Step 6 (AFK): Documentation Sync

Goal: Keep project documentation aligned with the implemented behavior.
Scope: If the implementation changes documented Forgejo provider behavior, role configuration, or reviewer/fixer workflows, update relevant project docs and keep this spec's implementation notes current.
Depends on: Step 5

## Progress

- [x] Step 1 (AFK): Summary Protocol Core
- [x] Step 2 (AFK): Reviewer Summary Publishing
- [x] Step 3 (AFK): Fixer Summary Consumption And Publishing
- [x] Step 4 (AFK): Forgejo Role Integration And Validation
- [x] Step 5 (AFK): EAG Validation
- [x] Step 6 (AFK): Documentation Sync

## Implementation

See [steps.md](./steps.md).

## Deferred Follow-Ups (DFU)

None.
