# Steps

## Step 1 (AFK): Summary Protocol Core

- Added `internal/forge/summary_protocol.go` with shared Forgejo Reviewer/Fixer Summary v1 types, constants, marker rendering/parsing, single-comment extraction, schema-version checks, enum validation, Review Item ID uniqueness/link validation, and Fixer-result validation against Reviewer Summary open items.
- Added focused tests in `internal/forge/summary_protocol_test.go` for render/parse round trips, missing/duplicate/invalid marker failures, schema/enum/round validation, duplicate Review Item IDs, superseded-link invariants, and Fixer missing/unknown/non-open result rejection.
- Verified with `go test ./internal/forge` and `go test ./...`.

## Step 2 (AFK): Reviewer Summary Publishing

<!-- Implementation and verification notes for the matching Plan step. -->

## Step 3 (AFK): Fixer Summary Consumption And Publishing

<!-- Implementation and verification notes for the matching Plan step. -->

## Step 4 (AFK): Forgejo Role Integration And Validation

<!-- Implementation and verification notes for the matching Plan step. -->

## Step 5 (AFK): EAG Validation

Record the EAG in detail here during implementation:

- Contract E2E workflows/commands run:
- Contract E2E observations:
- Sandbox repository: `https://code.powerformer.net/core/looper-sandbox`
- Sandbox operating reference: `specs/change/20260622-forgejo-provider-e2e/real-agent-e2e.md`
- Isolated runtime/config validation/daemon observation notes:
- Sandbox PR/branch/artifacts used:
- Reviewer Summary comment observed:
- Fixer Summary comment observed:
- No-resolve/native-review mutation observations:
- Issues discovered during EAG:
- Fixes made during EAG:
- Cleanup status:

## Step 6 (AFK): Documentation Sync

<!-- Implementation and verification notes for the matching Plan step. -->
