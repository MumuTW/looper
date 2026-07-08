# Design

## Research

### Current Looper Forgejo Support

- Forgejo provider MVP is implemented and supports config-driven Forgejo projects, REST provider access, provider-aware runtime/scheduler, planner, worker, and comment-only reviewer. Source: `specs/change/20260618-forgejo-provider-mvp/spec.md`.
- Forgejo Reviewer/Fixer summary protocol is implemented. Reviewer writes a top-level Reviewer Summary; Fixer consumes `open` items from that summary and writes a Fixer Summary. Source: `specs/change/20260630-forgejo-reviewer-fixer/spec.md`.
- Current docs describe Forgejo reviewer/fixer as summary-comment based and explicitly say Forgejo fixer does not resolve native review threads. Source: `docs/configuration.md:227-230`, `docs/users-guide.md:269`.
- Current sandbox coverage includes Forgejo worker PR creation and Forgejo reviewer/fixer summary protocol. Source: `internal/e2e/forgejo_sandbox_test.go`.

### Forgejo Native Review Comment Resolution

- Current Forgejo/Gitea code paths expose comment-level resolve/unresolve endpoints for PR review comments:
  - `POST /repos/{owner}/{repo}/pulls/{index}/comments/{id}/resolve`
  - `POST /repos/{owner}/{repo}/pulls/{index}/comments/{id}/unresolve`
- Review comment objects include fields such as `id`, `body`, `user`, `resolver`, `pull_request_review_id`, `path`, `commit_id`, `original_commit_id`, `position`, `original_position`, `diff_hunk`, `html_url`, and `pull_request_url`.
- The resolved state is comment-centric: `resolver` is set when a review comment is resolved and explicit JSON `null` means unresolved.
- This differs from GitHub's first-class review-thread/conversation model. Forgejo does not provide a GitHub-equivalent thread resource ID and thread-level state; the safe native authority is the individual review comment.
- Version caveat: older Gitea releases and older Forgejo builds may not expose these endpoints or the `resolver` field. This spec assumes supported Forgejo projects run a sufficiently new Forgejo build and enables native resolution by default for manual Forgejo Fixer. Older unsupported installs fail clearly rather than silently downgrading.

### Source Links

- Gitea API docs: https://docs.gitea.com/api/
- Gitea PR adding resolve/unresolve review comment endpoints: https://github.com/go-gitea/gitea/pull/36441
- Gitea pull review structs: https://github.com/go-gitea/gitea/blob/main/modules/structs/pull_review.go
- Gitea model resolve support: https://github.com/go-gitea/gitea/blob/v1.26.2/models/issue_comment.go
- Forgejo related PR/issues:
  - https://codeberg.org/forgejo/forgejo/pulls/11529
  - https://codeberg.org/forgejo/forgejo/issues/12237
  - https://codeberg.org/forgejo/forgejo/pulls/12238

## Design Detail

### Design Decisions

- Model Forgejo native review repair as **comment-centric resolution**, not GitHub thread parity. The provider authority is Forgejo review comment state: `id` identifies the unit, and a present `resolver` field determines resolved state.
- Ship the first slice as **manual/direct Fixer only**. Forgejo Fixer auto-discovery remains out of scope because current Forgejo validation disables fixer auto-discovery.
- Enable native Forgejo comment resolution by default for manual Forgejo Fixer. There is no startup or first-use probing in v1. If required fields/endpoints are absent, the run fails clearly before repair work or before provider acknowledgment, depending on where the unsupported signal appears.
- Preserve GitHub behavior. Existing GitHub review-thread logic should keep its prompt/output/resolve semantics unchanged.
- Preserve Forgejo summary protocol. Reviewer Summary remains the authority for Looper-authored summary items; Forgejo native review comments are separate provider-authority items. V1 performs no semantic deduplication between those sources.
- Avoid a broad provider-neutral repair model unless needed. First try to extend/reuse existing Fixer item structs with Forgejo-specific source and provider comment fields; introduce a new model only with a concrete diff-based justification.
- Do not add persisted authority for native comment open/resolved state. Stored IDs/fingerprints are audit and retry inputs only; every resolve side effect re-reads Forgejo.
- Resolve only `fixed` native comments. `declined`, `deferred`, missing-result, unrelated, changed, deleted, or unhandled comments remain unresolved.
- Keep native Forgejo Reviewer inline publishing out of this spec. It requires reliable diff-position mapping and idempotency, which are separate design problems.

### Authority Statement

The authority for Forgejo native repair state is the Forgejo PR review comment object returned by the provider API. A present `resolver: null` means unresolved; a present non-null `resolver` means resolved. If the `resolver` field is absent, Looper must treat native resolution as unsupported for that response.

Looper agent output is the authority only for whether the agent claims it fixed a specific provider comment ID from the prompt. Before resolving, Looper verifies current provider state and drift using the comment ID plus observed fingerprint.

### New Concept Trade-Off

This change may introduce a small extension to Fixer repair items for Forgejo native comments.

- Failure it prevents: forcing Forgejo comments into GitHub thread IDs would make resolve targets ambiguous and could resolve the wrong provider object.
- Cost: additional source labels, provider comment IDs, observed fingerprints, prompt/output validation, and tests proving GitHub behavior remains unchanged.
- Simpler alternative rejected: keep summary protocol only. That avoids item-shape changes but misses native Forgejo UI resolution even though Forgejo now exposes a usable comment-level API.

This spec does **not** authorize a large cross-provider capability redesign or a new persisted review-state authority. If implementation needs either, stop and update this design first.

### Compatibility Policy

V1 policy is fixed:

1. Native Forgejo comment resolution is default on for manual Forgejo Fixer.
2. Fixer collects native review comments for Forgejo projects before loading summary items.
3. Review comments must include a present `resolver` field. Missing `resolver` is an unsupported-capability error before native repair starts.
4. A 404/405 from the resolve endpoint is an unsupported-capability/manual-intervention failure. Looper does not silently fall back after using native comments as repair input.
5. There is no runtime endpoint probing in v1. The first side-effecting resolve call is not a capability probe; it is part of the run after repair safety gates.
6. An optional escape hatch for old Forgejo installs is deferred until a real need appears.

### Native Repair Item Contract

Each native Forgejo comment candidate gets a source-scoped ID and fingerprint:

```json
{
  "source": "forgejo_review_comment",
  "providerCommentId": 12345,
  "path": "internal/example.go",
  "body": "Handle nil config before dereference.",
  "diffHunk": "@@ ...",
  "url": "https://code.example.com/org/repo/pulls/7#...",
  "observedFingerprint": "sha256:<hash>"
}
```

The fingerprint should prefer provider timestamp/version data if available; otherwise use a stable hash of fields Looper showed the agent, such as provider comment ID, body, path, diff hunk, commit ID, and original position. Resolve requires the post-push comment to match the observed fingerprint.

Agent output for native items must include structured repair results:

```json
{
  "repair_results": [
    {
      "source": "forgejo_review_comment",
      "providerCommentId": 12345,
      "action": "fixed",
      "explanation": "Added the nil config guard.",
      "observedFingerprint": "sha256:<hash>"
    }
  ]
}
```

Allowed actions are `fixed`, `declined`, and `deferred`. Missing native results are treated as `deferred`. Only `fixed` results can lead to provider resolve.

### System Procedure

1. Manual Fixer starts against a Forgejo PR.
2. Fixer fetches PR metadata and fresh Forgejo review comments.
3. Fixer fails unsupported before repair if the review comment response does not include a presence-aware `resolver` field.
4. Fixer filters native review comments to unresolved comments (`resolver` field present and value `null`) and excludes comments authored by the current Looper provider identity.
5. Fixer loads existing Reviewer Summary `open` items separately.
6. Fixer prompt includes source-specific repair items. Forgejo native items use comment terminology, not thread terminology.
7. Agent returns structured results. Missing native results are treated as deferred.
8. Looper validates, commits, and pushes using the existing Fixer safety flow.
9. Before resolving, Looper re-fetches native comments.
10. Looper resolves only comments whose result is `fixed`, whose current provider state is still unresolved, and whose fingerprint still matches.
11. Looper records resolve outcomes in existing run/checkpoint output if possible. These records are audit only, not current-state authority.

### Failure Behavior

- Missing `resolver` field before repair: non-retryable unsupported capability; no repair work starts from native comments.
- Resolve 404/405: non-retryable unsupported-capability/manual-intervention failure.
- Resolve 5xx/timeouts: retryable provider-ack failure for the resolve step after push.
- Already resolved at post-push re-read: success/skipped for that comment.
- Deleted/not found at post-push re-read: skipped with audit note; do not fail the whole repair solely because the comment disappeared.
- Fingerprint changed at post-push re-read: leave unresolved and report drift/manual intervention.
- Partial resolve failure after push: run must not report plain success; surface retryable provider-ack failure or manual intervention according to error class.

### Testing Requirements

- Unit/contract tests for Forgejo review-comment decoding, distinguishing absent `resolver` from present `null`, pagination, resolve endpoint path, and unsupported endpoint errors.
- Compatibility tests proving native resolution is default on for manual Forgejo Fixer and missing `resolver` fails before repair.
- Fixer prompt tests proving Forgejo native comment IDs, fingerprints, and source labels are present, and Forgejo comments are not described as GitHub threads.
- Fixer output validation tests for one result per native candidate or missing-as-deferred behavior.
- Fixer safety tests proving no resolve call happens before validation and push success.
- Drift tests for already-resolved, deleted/not-found, changed fingerprint, unsupported endpoint, and 5xx/timeouts.
- Regression tests proving GitHub fixer thread resolution behavior is unchanged.
- Manual/conditional sandbox E2E against a Forgejo instance known to support native resolve.

### Out of Scope

- Forgejo Reviewer native inline comment publishing.
- Forgejo Fixer auto-discovery/scheduler behavior for native comments.
- Unresolve API support.
- Shipping a `gitea` provider kind.
- Full GitHub thread-resource parity for Forgejo.
- Coordinator, auto-merge, branch protection, webhooks, or routed network mode for Forgejo.
- Durable outbox/persistent authority for comment resolved state.
- Runtime capability probing.
