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
