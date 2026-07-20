# Implementation Steps

## Step 1 (AFK): Forgejo Review Comment API

Status: completed

Notes:

- Implement list PR review comments and resolve review comment only.
- Do not implement unresolve in this change.
- Decode `resolver` with presence tracking so absent and explicit `null` differ.
- Cover fake-server endpoint paths, pagination, unsupported endpoint errors, and sanitized errors.

## Step 2 (AFK): Default-On Compatibility Gate

Status: completed

Notes:

- Make native comment resolution the default manual Forgejo Fixer behavior.
- Keep GitHub behavior unchanged.
- If required Forgejo fields/endpoints are unsupported, fail clearly; do not silently fall back after native comments are used as repair input.
- Defer any escape hatch for old Forgejo installs until real users need it.

## Step 3 (AFK): Minimal Fixer Repair Item Extension

Status: completed

Notes:

- First try to extend/reuse existing Fixer item shape.
- Add source, provider comment ID, observed fingerprint, path, body, diff hunk, and URL as needed.
- Preserve GitHub behavior.
- If a new model is unavoidable, document the concrete diff-based justification before implementing it.

## Step 4 (AFK): Manual Forgejo Fixer Native Comment Consumption

Status: completed

Notes:

- Manual/direct Forgejo Fixer only.
- Fetch unresolved native comments by default for Forgejo projects.
- Exclude comments authored by the current Looper provider identity.
- Keep Reviewer Summary items as a separate source.
- No semantic deduplication between summary items and native comments.

## Step 5 (AFK): Structured Native Comment Results

Status: completed

Notes:

- Define the Forgejo-specific structured output contract.
- Required fields: source, providerCommentId, action, explanation, observedFingerprint.
- Allowed actions: fixed, declined, deferred.
- Missing native results count as deferred.
- Only fixed can lead to provider resolve.

## Step 6 (AFK): Safe Resolve After Successful Repair

Status: completed

Notes:

- Resolve only after agent success, validation success, and push success.
- Re-read Forgejo before resolving.
- Resolve only fixed, still-unresolved, unchanged comments.
- Treat 5xx/timeouts as retryable provider-ack failure.
- Treat 404/405 as unsupported-capability/manual-intervention failure.

## Step 7 (AFK): EAG Validation And Documentation

Status: completed

Notes:

- Run contract/integration tests.
- Run manual sandbox E2E only when a compatible Forgejo instance is available.
- Record commands, PR/comment IDs, provider resolved state, and cleanup here.
- Update docs for default native resolution, manual Fixer scope, unsupported-capability behavior, and GitHub-vs-Forgejo resolution differences.

Current validation notes:

- Completed locally during implementation:
  - `go test ./internal/forge`
  - `go test ./internal/fixer`
  - `go test ./internal/fixer ./internal/runtime`
  - `go test -count=1 ./internal/fixer ./internal/runtime`
  - `go test ./...`
  - `go vet ./...`
  - `go build ./...`
- Manual Forgejo sandbox E2E not run in this session because no compatible sandbox instance was provided.
