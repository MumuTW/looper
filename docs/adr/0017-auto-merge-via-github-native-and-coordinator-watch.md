# ADR-0017: Auto-merge via GitHub-native auto-merge and Coordinator watch

- Status: Superseded by the Gatekeeper trust ladder in issue #116
- Date: 2026-05-18
- Related: ADR-0002, PRD #352, slice #357, issue #358
- Note: originally numbered 0005. Two ADRs were merged on 2026-05-18 from
  parallel branches (#399, #400) and both claimed 0005, leaving "ADR-0005"
  ambiguous. ADR-0015 cites 0005 meaning the webhook forwarder decision, so
  that one keeps the number and this one was renumbered. The decision, date,
  and content are unchanged.

> Historical decision: Reviewer no longer enables GitHub-native auto-merge.
> Merge Gatekeeper is the only Looper merge authority and, at `auto`, performs a
> complete confirming evaluation before an immediate head-matched merge. The
> Coordinator watch remains useful for human-created GitHub auto-merge state.

## Context

Reviewer is extending an agent-driven authority path. AGENTS.md requires naming the authority before enforcing it and avoiding inference layers that override the agent's structured output with unrelated infrastructure guesses.

This slice adds two authority-bearing behaviors:

1. Reviewer raises the APPROVE bar for implementation PRs when reviewer auto-merge is enabled.
2. Reviewer can opt an eligible PR into GitHub-native auto-merge.

The watch that observes opted-in PRs belongs to Coordinator, but the concrete poll implementation lands in slice 3. This ADR establishes the authority boundaries now so slice 2 and slice 3 share the same contract.

## Decision

### Authority chain 1: “Is it safe to merge?”

Authority: GitHub branch protection.

Looper opts in with `gh pr merge --auto --{strategy}` and GitHub decides when the PR is mergeable. Looper does not re-check CI, count reviews, or resolve conversations before merging. Those are GitHub's merge gates, not Reviewer's authority.

### Authority chain 2: “Is this Looper's PR?”

Authority: the durable conjunction of:

- a `looper:` label on the PR, and
- a PR→Issue link to a Coordinator-tracked issue.

Either signal alone is too weak. The label alone is broad, and the issue link alone would let unrelated PRs on tracked issues enter Looper's auto-merge scope.

### Authority chain 3: “Did the work satisfy the issue?”

Authority: the linked issue's `## Acceptance criteria` section plus Reviewer's per-criterion satisfying evidence pointers.

Reviewer persists that evidence in the APPROVE review body under `### Acceptance criteria verification` so future readers can audit why the PR qualified.

## Consequences

### Positive

- GitHub remains the merge authority for branch protection and timing.
- Reviewer's APPROVE becomes auditable against the implementing issue instead of being a generic clean signal.
- The Coordinator watch can consume GitHub state later without redefining merge safety or PR ownership.

### Costs

- Reviewer now depends on a linked issue with explicit acceptance criteria before engaging the stricter auto-merge path.
- The system must preserve the PR→Issue link and per-criterion evidence text across retries.
- Slice 3 must watch GitHub auto-merge state without treating that watch as the merge authority itself.

## Rejected alternatives

### Custom Looper merge endpoint (`PUT /pulls/.../merge`)

Rejected because it would make Looper the merge gate authority. That duplicates GitHub's branch-protection logic and violates the “Name the authority before enforcing it” rule.

### Multi-strategy fall-through

Rejected because silently switching merge strategies changes the authority contract. The configured strategy is part of the operator's intent; if GitHub repo settings disallow it, Reviewer refuses auto-merge instead of guessing another merge mode.

### Scope = any PR on a tracked issue

Rejected because an issue link alone is insufficient to prove the PR is Looper-managed. Auto-merge scope requires both the Looper label and the tracked-issue link.

## Coordinator watch boundary

Coordinator owns the merge-watch poll in slice 3. It will observe GitHub-native auto-merge progress and merged/closed outcomes, but that watch is drift detection and orchestration state, not merge authority. The merge authority remains GitHub branch protection and merge execution.
