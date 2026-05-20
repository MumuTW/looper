# Fixer post-repair comment regression investigation

## Summary

After upgrading Looper to 0.8.1, the fixer can complete and push repair commits for review feedback without posting the expected PR review-thread reply or round-summary comment. The behavior was observed on <https://github.com/nexu-io/vela/pull/21>.

This note records the user-visible symptom, local evidence, and the likely root cause so the fix can be designed separately.

## User-visible symptom

On `nexu-io/vela#21`, the fixer addressed review feedback and pushed commits to the PR branch, but the PR did not receive a Looper fixer comment explaining the repair.

Expected behavior:

- After a successful repair and push, Looper posts a reply to the relevant review thread before resolving it.
- When appropriate, Looper posts or updates a round-summary issue comment on the PR.

Observed behavior:

- Repair commits were pushed.
- Review-thread reply calls did not appear on the PR.
- The PR issue comments list remained empty for fixer round summaries.
- The fixer loop retried from discovery instead of completing `resolve-comments`.

## Evidence from PR 21

The PR branch reached `ada1494b05ab857666ce1ed1f84b30971ecae420`.

The relevant review threads observed from GitHub were:

- `PRRT_kwDOScYL3c6Da3Fp` on `.github/workflows/full-smoke.yml`, resolved.
  - Original Looper reviewer comment: `PRRC_kwDOScYL3c7DD2_l`
  - Human reply: `PRRC_kwDOScYL3c7DD37x`
- `PRRT_kwDOScYL3c6Da3_b` on `apps/cli/internal/config/config.go`, resolved.
  - Codex review comment: `PRRC_kwDOScYL3c7DD4Pi`
- `PRRT_kwDOScYL3c6DbMkF` on `e2e/full-smoke/run-full-smoke.py`, still unresolved at the time of inspection.

The PR had no normal issue comments, so no fixer round-summary issue comment had been created.

## Evidence from local Looper state

The local loop for `nexu-io/vela#21` was `loop_703cf230932d70923486a41fa5000a0e`.

### First fixer run

Run: `run_ca5e28538970839e37c0e81bb6122ba1`

- Status: `failed`
- Current step: `resolve-comments`
- Last completed step: `push`
- Error:
  - `Skipped 1 review thread(s) because the fixer response omitted or invalidated thread decisions`
- Pushed commit:
  - `7b6fbdb55eb4b3fb805dde807edd0569c6cdd5af`
- Repair summary:
  - Replaced the placeholder full-smoke workflow with a real GitHub Actions job.
- Checkpoint `resolvedComments`:
  - `PRRT_kwDOScYL3c6Da3_b` was marked `skipped_missing_agent_decision`.

This run repaired the original Looper reviewer item, but by the time `resolve-comments` reloaded the PR, a different unresolved Codex thread was present. The agent had no decision for that newly discovered thread, so Looper treated the response as a contract violation and did not post a reply.

### Second fixer run

Run: `run_4ac37d4fc5dbc44622ceab6550c0ccb2`

- Status: `failed`
- Current step: `resolve-comments`
- Last completed step: `push`
- Error:
  - `Skipped 1 review thread(s) because review thread content changed during the fixer run`
- Pushed commit:
  - `ada1494b05ab857666ce1ed1f84b30971ecae420`
- Repair summary:
  - Expanded `VELA_HOME` tilde handling in the CLI and added database readiness polling.
- Agent-provided reply explanation:
  - `fixItemId`: `PRRC_kwDOScYL3c7DD4Pi`
  - `threadId`: `PRRT_kwDOScYL3c6Da3_b`
  - `action`: `fixed`
  - `threadCommentsObserved`: `34dc0728942224002f056ac16419b61e7fc0c49f84e5722e44b5fd561023b821`
- Checkpoint `resolvedComments`:
  - `PRRT_kwDOScYL3c6Da3_b` was marked `skipped_thread_drift` with message `Review thread snapshot was missing or changed since the fixer inspected it`.

This run did include a per-thread decision, but Looper rejected it as drift before calling `replyToFixedComment`.

## Relevant code path

The post-repair comment flow is concentrated in `internal/fixer/runner.go`:

- `runResolveCommentsStep` refreshes PR state, collects live unresolved review comments, validates agent decisions, posts replies, and resolves review threads.
- `replyToFixedComment` builds and posts a fixed-thread reply with `AddReviewThreadReply`.
- `replyToDeclinedComment` does the same for declined threads.
- `publishRoundSummaryComment` creates or updates the round-summary issue comment.

The GitHub mutations are wired through:

- `internal/runtime/scheduler.go`, via `fixerGitHubAdapter`
- `internal/infra/github/gateway.go`, via `AddReviewThreadReply`, `ResolveReviewThread`, `CreateIssueComment`, and `UpdateIssueComment`

The relevant guard is:

- `internal/fixer/runner.go:2536`, which skips a fixed decision when `fixedDecisionMissingThreadSnapshot(decision)` or `threadCommentsObservedDrifted(decision, thread)` is true.
- `internal/fixer/runner.go:2841-2846`, where `threadCommentsObservedDrifted` compares the agent-provided `threadCommentsObserved` hash against Looper's live `hashReviewThreadComments(thread)` value.
- `internal/fixer/runner.go:5286-5293`, where the fixer prompt requires the agent to compute `threadCommentsObserved` itself.

## Likely root cause

The 0.8.1 flow appears to make the agent-provided `threadCommentsObserved` hash an authority-bearing precondition for a side-effecting GitHub action. If that hash differs from Looper's live calculation, Looper skips the reply and resolve path even when the repair commit was already pushed.

That is fragile for this action because:

- The agent must reproduce Looper's exact hash input and JSON serialization rules.
- The agent is instructed to fetch review-thread context through GitHub APIs that may expose timestamps, comment ordering, or bot/thread metadata differently from Looper's own `ViewReviewThread` path.
- The mismatch is treated as review-thread drift, not as a malformed or unverifiable agent witness.
- Once the guard fires, Looper does not post the explanatory thread reply or a round-summary comment for the already-pushed repair.

For PR 21, the second run demonstrates this directly: the agent provided a fixed decision for `PRRT_kwDOScYL3c6Da3_b`, but `resolve-comments` classified it as `skipped_thread_drift` and never called `replyToFixedComment`.

## Design implication

The authority for whether review-thread content drifted should probably remain inside Looper, not with the agent's self-computed hash. A safer design would have Looper record the thread snapshot/fingerprint at collection time and use that Looper-owned observation to validate the live thread before posting replies or resolving threads. The agent should provide only the structured decision and explanation.

If Looper intentionally suppresses a thread reply after a repair commit has already been pushed, it should still leave an explicit failure state or PR-visible summary rather than silently making the PR look unacknowledged.
