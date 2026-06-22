# Journal

## 2026-06-22: Step 8 pair-mode kickoff

Status: in progress.

Goal:

- Run local `looperd`/`looper` builds against the real Forgejo sandbox with real agent execution rather than the e2e fake agent.
- Cover the Forgejo MVP worker path first.
- Cover the Forgejo MVP comment-only reviewer path second.

Pair-mode split:

- AI operations: build local binaries, prepare ignored/local runtime config, run supported preflights, execute `looperd`, observe local logs/results, classify failures, and record command shapes/results without secrets.
- Human operations: confirm the real local agent command, maintain the ignored token file, create/approve sandbox issues or PRs, make Forgejo-side permission/config changes if blocked, and authorize or perform any remote destructive cleanup.

Current workspace note:

- Historical Step 7 journal content is tracked in `journal-sandbox.md`.
- This file records the Step 8 real-agent local run requested after Step 7 completed.

Next AI operations:

1. Build `dist/looperd` and `dist/looper` from local source.
2. Inspect config shape for an isolated Forgejo MVP real-agent runtime.
3. Ask the human to choose/confirm the real local agent command before any live real-agent run.

## 2026-06-22: Step 8 local binary build

Status: completed.

AI operations completed:

- Built local daemon binary: `go build -o dist/looperd ./cmd/looperd` — passed.
- Built local CLI binary: `go build -o dist/looper ./cmd/looper` — passed.

Notes:

- Build outputs are under `dist/`, which is generated output and not committed.
- No live Forgejo operation or real agent invocation has run yet.

## 2026-06-22: Step 8 worker config preparation

Status: completed.

Human operation completed:

- Selected `codex` as the real local agent command for the Step 8 run.

AI operations completed:

- Confirmed local commands are present: `codex`, `opencode`, `claude`, `agent`, and `git`.
- Confirmed `e2e/.env` has the required Forgejo env keys without printing the token value.
- Created isolated runtime outside the repository at `/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent`.
- Cloned the Forgejo sandbox repository into the isolated runtime and configured its local `origin` for token-backed HTTPS push without recording the token.
- Wrote worker-only local config at `/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent/config-worker.toml`.
- Configured only Forgejo MVP-supported behavior for the worker run: polling, worker discovery by `looper:worker-ready`, current-user assignment requirement, reviewer discovery disabled, fixer disabled, coordinator disabled, osascript disabled, and auto-merge disabled.
- Ran config validation: `source e2e/.env && dist/looper --config <config-worker.toml> config validate` — passed.

Notes:

- A first config generation attempt failed before writing a usable config because of shell/Python quoting around TOML path expressions; it was corrected before validation.
- The config references `tokenEnv = "LOOPER_E2E_FORGEJO_TOKEN"`; no token value is stored in tracked files or the journal.
- No live Looper daemon run or real agent invocation has started yet.

## 2026-06-22: Step 8 worker preflight

Status: completed.

AI operations completed:

- Re-ran config validation: `source e2e/.env && dist/looper --config <config-worker.toml> config validate` — passed.
- Checked isolated sandbox clone status: `main...origin/main`, clean.
- Checked read access to remote git HEAD with `git ls-remote --exit-code origin HEAD` — passed.
- Checked isolated runtime/worktree directories are writable — passed.
- Checked selected `codex` command and `git` are executable — passed.
- Checked Forgejo API access with token-backed read-only `GET /api/v1/user` and `GET /api/v1/repos/core/looper-sandbox` — passed.

Observed non-secret Forgejo API facts:

- Current token user: `nettee`.
- Sandbox repo: `core/looper-sandbox`.
- Sandbox default branch: `main`.

Notes:

- A first temporary Go preflight snippet failed due shell quoting before making a successful API request; it was replaced with read-only `curl` requests.
- No daemon run, issue mutation, PR mutation, git push, or real agent invocation has started yet.

## 2026-06-22: Step 8 worker issue ready

Status: completed.

Human operation completed:

- Created Forgejo sandbox worker issue: `https://code.powerformer.net/core/looper-sandbox/issues/10`.

AI operations completed:

- Confirmed issue #10 is open.
- Confirmed issue #10 title is `Looper real-agent worker smoke test`.
- Confirmed issue #10 is assigned to current token user `nettee`.
- Confirmed issue #10 has trigger label `looper:worker-ready`.

Next AI operation:

- Start local `looperd` with the worker-only real-agent config and observe the worker run.

## 2026-06-22: Step 8 worker real-agent run

Status: partially completed; remote worker path succeeded, local recovery state failed after an induced shutdown.

AI operations completed:

- Started local `looperd` with `/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent/config-worker.toml`.
- Observed scheduler claim one worker item for issue #10.
- Observed Forgejo PR creation: `https://code.powerformer.net/core/looper-sandbox/pulls/11`.
- Verified PR #11 is open and titled `Looper real-agent worker smoke test`.
- Verified PR #11 changed exactly the intended worker smoke file: `looper-real-agent-worker-smoke.txt`.
- Verified PR #11 has one commit: `cfd0a66db365` with message `Implement looper real-agent worker smoke test`.
- Verified the PR head file content is `worker real-agent smoke test 2026-06-22`.
- Re-ran local `looperd` after the first monitor script terminated the daemon too early, to let recovery settle.

Failure classification:

- Worker remote/live path result: passed. Real `codex` agent execution produced the intended file change, Looper pushed a branch, and Forgejo PR #11 was created.
- Local daemon completion state: failed due an induced recovery-path defect. The first monitor script stopped `looperd` immediately after seeing the PR, before the worker queue item finished local bookkeeping.
- Recovery result: `looperd` marked the interrupted run stale/orphaned, requeued work, then both worker queue items reached `manual_intervention` with `upsert run: UNIQUE constraint failed: runs.loop_id`.
- Current local DB state includes one old run marked `interrupted` and one newer run left `running`; queue items are `manual_intervention`.

Notes:

- The recovery failure appears to be a local Looper recovery/idempotency defect exposed by the test harness shutdown timing, not a Forgejo provider API failure and not a real agent failure.
- Remote side currently has one open PR for this worker issue; no duplicate PR was observed.
- Issue #10 still has `looper:worker-ready`, and the local isolated runtime should not be reused for the reviewer case without resetting or using a separate DB.

## 2026-06-22: Step 8 reviewer config preparation

Status: completed.

AI operations completed:

- Created a separate reviewer runtime under `/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent/reviewer-runtime`.
- Wrote reviewer-only local config at `/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent/config-reviewer.toml`.
- Configured only Forgejo MVP-supported reviewer behavior: polling, label-based discovery via `looper:review`, `requireReviewRequest = false`, self-review allowed for this sandbox PR, comment-only clean/blocking events, thread resolution disabled, fixer disabled, worker disabled, coordinator disabled, osascript disabled, and auto-merge disabled.
- Configured `codex exec` with read-only sandbox for the reviewer run.
- Ran config validation: `source e2e/.env && dist/looper --config <config-reviewer.toml> config validate` — passed.

Notes:

- The reviewer run will use a separate DB from the worker run because the worker DB intentionally preserves the recovery failure evidence.
- The config references `tokenEnv = "LOOPER_E2E_FORGEJO_TOKEN"`; no token value is stored in tracked files or the journal.
- No reviewer daemon run or reviewer agent invocation has started yet.
