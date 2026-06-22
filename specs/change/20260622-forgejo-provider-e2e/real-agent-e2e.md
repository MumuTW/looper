# Forgejo Real-Agent E2E Notes

This document records the practical workflow for running Looper locally against a real Forgejo sandbox with a real agent command. It complements the fake-agent live sandbox e2e in `internal/e2e/forgejo_sandbox_test.go`.

## Scope

The current Forgejo MVP supports real-agent local runs for:

- Worker: assigned issue → real agent → commit → push branch → Forgejo PR.
- Reviewer: labeled PR → real agent review → normal Forgejo PR comment.

The current Forgejo MVP does not support fixer, coordinator/dependency gates, native review requests, inline review threads, thread resolution, auto-merge, or webhook/routed mode. Do not include those paths in a Forgejo real-agent e2e until the product capability exists.

## HITL workflow

Use pair mode.

AI operations:

1. Build local `looperd` and `looper` binaries.
2. Prepare an isolated runtime outside the repository.
3. Write local-only config files that read tokens from environment variables.
4. Validate config before live daemon startup.
5. Run read-only preflights for local tools, runtime paths, git remote access, and Forgejo API access.
6. Start `looperd` with a bounded observation window.
7. Wait for queue/run terminal state before stopping the daemon.
8. Record command shapes, artifact URLs/IDs, result classification, and failures without secrets.

Human operations:

1. Choose the real local agent command and confirm it is authenticated.
2. Maintain the ignored token env file, for example `e2e/.env`.
3. Create or approve the worker issue prompt before real agent execution.
4. Assign the worker issue to the token user and apply `looper:worker-ready`.
5. Prepare or approve the reviewer PR and apply `looper:review`.
6. Perform any Forgejo-side permission, token-scope, branch-protection, or cleanup action.
7. Decide whether remote destructive cleanup is allowed. By default, remote branch/issue/PR/comment deletion is human-owned.

## Local build

Build from the repository root:

```bash
go build -o dist/looperd ./cmd/looperd
go build -o dist/looper ./cmd/looper
```

`dist/` is generated output and should not be committed.

## Token and environment

Keep live Forgejo credentials in an ignored file such as `e2e/.env`:

```bash
export LOOPER_E2E_FORGEJO=1
export LOOPER_E2E_FORGEJO_BASE_URL=https://code.example.com
export LOOPER_E2E_FORGEJO_SANDBOX_REPO=owner/repo
export LOOPER_E2E_FORGEJO_TOKEN=replace-with-token
```

The local config should contain only `tokenEnv = "LOOPER_E2E_FORGEJO_TOKEN"`, never the token value.

Before a run, source the env file and verify keys without printing secrets:

```bash
source e2e/.env
for v in LOOPER_E2E_FORGEJO_BASE_URL LOOPER_E2E_FORGEJO_SANDBOX_REPO LOOPER_E2E_FORGEJO_TOKEN; do
  test -n "${!v}" || { echo "$v missing"; exit 1; }
  echo "$v present"
done
```

## Isolated runtime layout

Use a temporary directory outside the repository. The Step 8 run used:

```text
/var/folders/1d/0byj0hb96vd30xbwb4b4b3800000gn/T/opencode/looper-forgejo-real-agent/
  config-worker.toml
  config-reviewer.toml
  repos/looper-sandbox/
  runtime/
    looper.sqlite
    logs/
    backups/
  worktrees/
  reviewer-runtime/
    looper.sqlite
    logs/
    backups/
  reviewer-worktrees/
```

Use separate SQLite DBs for independent cases when a previous run preserves failure evidence. Do not reuse a DB that is in `manual_intervention` or contains interrupted/running recovery state unless the purpose is explicitly to test recovery.

Clone the sandbox repo into the isolated runtime and configure `origin` for token-backed HTTPS push. Avoid printing the remote URL because it may contain the token.

## Worker config shape

The worker config should enable only Forgejo MVP worker behavior:

```toml
[daemon]
logDir = "/tmp/looper-forgejo-real-agent/runtime/logs"
workingDirectory = "/tmp/looper-forgejo-real-agent/runtime"

[storage]
mode = "sqlite"
dbPath = "/tmp/looper-forgejo-real-agent/runtime/looper.sqlite"
backupDir = "/tmp/looper-forgejo-real-agent/runtime/backups"

[scheduler]
pollIntervalSeconds = 20
maxConcurrentRuns = 1

[agent]
vendor = "codex"

[agent.params]
command = "/absolute/path/to/codex"
args = ["--sandbox", "workspace-write"]

[notifications]
inApp = false

[notifications.osascript]
enabled = false

[tools]
gitPath = "/usr/bin/git"
looperPath = "/absolute/path/to/dist/looper"

[[providers]]
id = "forgejo-main"
kind = "forgejo"
baseUrl = "https://code.example.com"
tokenEnv = "LOOPER_E2E_FORGEJO_TOKEN"

[roles.coordinator]
enabled = false

[roles.worker]
autoDiscovery = true

[roles.worker.triggers]
labels = ["looper:worker-ready"]
labelMode = "all"
requireAssigneeCurrentUser = true

[roles.reviewer.discovery]
autoDiscovery = false

[roles.fixer]
autoDiscovery = false

[[projects]]
id = "forgejo-sandbox"
name = "Forgejo Sandbox"
repoPath = "/tmp/looper-forgejo-real-agent/repos/looper-sandbox"
provider = "forgejo-main"
repo = "owner/repo"
baseBranch = "main"
worktreeRoot = "/tmp/looper-forgejo-real-agent/worktrees"
```

Validate before starting the daemon:

```bash
source e2e/.env
dist/looper --config /tmp/looper-forgejo-real-agent/config-worker.toml config validate
```

## Worker HITL trigger

Human creates a small assigned issue:

- Issue is open.
- Issue is assigned to the token user.
- Issue has `looper:worker-ready`.
- Prompt asks for one minimal harmless change.

Example prompt:

```text
Create or update looper-real-agent-worker-smoke.txt at the repository root.
Add one line: worker real-agent smoke test YYYY-MM-DD
Keep the diff minimal. Do not modify unrelated files.
```

Start the daemon:

```bash
source e2e/.env
dist/looperd --config /tmp/looper-forgejo-real-agent/config-worker.toml
```

Observe until local queue/run state reaches a terminal state. Do not stop as soon as the PR appears; remote side effects can happen before local bookkeeping finishes.

The Step 8 worker run proved the remote path: issue #10 led to PR #11 with the expected file and commit. It also exposed that stopping immediately after observing PR creation can leave an interrupted run that recovery moved to `manual_intervention` with `upsert run: UNIQUE constraint failed: runs.loop_id`.

## Reviewer config shape

Use a separate runtime/DB for reviewer-only validation if the worker DB preserves failure state.

Reviewer config should enable only Forgejo MVP comment-only reviewer behavior:

```toml
[agent]
vendor = "codex"

[agent.params]
command = "/absolute/path/to/codex"
args = ["--sandbox", "read-only"]

[roles.worker]
autoDiscovery = false

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
includeDrafts = false
requireReviewRequest = false
labels = ["looper:review"]
labelMode = "all"
enableSelfReview = true

[roles.reviewer.behavior]
scope = "changed_ranges"
publishMode = "single_review"

[roles.reviewer.behavior.loop]
enabledByDefault = true
quietPeriodSeconds = 1
minPublishIntervalSeconds = 1
maxIterationsPerPR = 1
maxIterationsPerHead = 1
maxWallClockSeconds = 600
maxConsecutiveFailures = 1
maxAgentExecutionsPerPR = 1
stopOnApproved = true
stopOnReadyLabel = false
stopOnIdenticalOutput = true

[roles.reviewer.behavior.reviewEvents]
clean = "COMMENT"
blocking = "COMMENT"

[roles.reviewer.behavior.threadResolution]
enabled = false

[roles.reviewer.autoMerge]
enabled = false

[roles.fixer]
autoDiscovery = false
```

Keep the same provider/project rules as the worker config: explicit Forgejo provider, `tokenEnv`, sandbox `repoPath`, `repo`, `baseBranch`, and isolated `worktreeRoot`.

## Reviewer HITL trigger

Human prepares a small open PR and adds `looper:review`.

Start the reviewer daemon:

```bash
source e2e/.env
dist/looperd --config /tmp/looper-forgejo-real-agent/config-reviewer.toml
```

Observe until the reviewer queue item reaches `completed` and the run reaches `success`. Verify the remote artifact is a normal Forgejo PR comment. The Step 8 reviewer run produced comment ID `50` on PR #11 with outcome `clean`.

## Observability checklist

Useful local checks:

```bash
# Config validation
source e2e/.env
dist/looper --config <config> config validate

# Git clone state
git -C <sandbox-clone> status --short --branch
git -C <sandbox-clone> ls-remote --exit-code origin HEAD >/dev/null

# Queue state
sqlite3 <runtime>/looper.sqlite 'select id,type,status,attempts,last_error from queue_items order by created_at desc limit 10;'

# Run state
sqlite3 <runtime>/looper.sqlite 'select id,loop_id,status,summary,error_message from runs order by started_at desc limit 10;'
```

Useful Forgejo checks use token-backed read-only API calls. Do not print the token. Check:

- `/api/v1/user`
- `/api/v1/repos/{owner}/{repo}`
- `/api/v1/repos/{owner}/{repo}/issues/{number}`
- `/api/v1/repos/{owner}/{repo}/pulls/{number}`
- `/api/v1/repos/{owner}/{repo}/issues/{number}/comments`

When collecting logs, redact token values if logs are read through scripts. Keep raw runtime logs outside tracked files.

## Lessons learned

- Real-agent e2e is feasible for Forgejo MVP worker and comment-only reviewer paths.
- Fake-agent sandbox e2e validates provider data-plane behavior, but it does not prove real agent CLI invocation, real agent prompt behavior, or daemon lifecycle around long-running agent executions.
- Real-agent e2e must treat remote side effects and local completion as separate checks. A PR/comment can exist while local queue bookkeeping is still in progress.
- Bounded observers should wait for terminal queue/run state before stopping `looperd`.
- Use separate isolated runtimes for separate cases, especially after a failed or interrupted run.
- Keep HITL prompts tiny and low-risk; the real agent can push to the sandbox repository.
- Keep remote destructive cleanup human-owned unless explicitly authorized.
