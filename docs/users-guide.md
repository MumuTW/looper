# Looper Quick User Guide

This guide is for everyday users. It focuses on how `coordinator`, `planner`, `reviewer`, `fixer`, and `worker` interact with forge issues and PRs. GitHub is fully supported; Forgejo support includes planner, worker, native reviewer requests/reviews, summary-comment compatibility, and the manual/direct native-review-comment fixer path.

> **CLI strip (read first).** The full `looper` CLI was removed ahead of the role-model rewrite. The **only** operator verbs are:
>
> `start` · `pause` · `retry` · `stop` · `close` · `takeover` · `handback` · `respond` · `version`
>
> (plus machine-only `review submit`). There is no `bootstrap`, `init`, `status`, `project add`, `daemon *`, `ps`, `logs`, `jump`, `plan`, `review`, `work`, or `webhook`/`provider`/`network` admin. Where this guide still shows an old command, treat it as **intent** and do the equivalent via forge labels, the config file, the dashboard, or the daemon HTTP API. Current install surface: [installation.md](installation.md) and the repository README.

## 1. Prerequisites

Make sure these work first:

```bash
command -v looperd
command -v looper
gh auth status  # GitHub projects only
curl -sS "http://127.0.0.1:17310/api/v1/healthz"   # daemon must be running
```

If the project is not registered yet, use the dashboard or:

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/projects" \
  -H 'Content-Type: application/json' \
  -d '{"repoPath":"/absolute/path/to/repo"}'
```

Webhook mode is configured in the config file (`webhook.mode` and per-project overrides). Observe health with `GET /api/v1/webhook/status` or the dashboard. Clearing stale GitHub CLI forwarder hooks is a manual `gh api` operation after you confirm the dry-run payload — there is no `looper webhook cleanup`.

Also make sure:

- `looperd` is running (foreground, or under your own process manager)
- your local repo can `git fetch` and `git push`
- GitHub projects: `gh` is authenticated with the target GitHub account
- Forgejo projects: put provider auth in the config file (`tokenEnv` or `teaLogin`) and export any required env vars in the daemon environment
- each coding role you want to run can resolve a vendor: set global `agent.vendor` in the config (for example `vendor = "opencode"`), or supply vendor via `agent.profiles` / `roles.<role>.agent` as described in [Multi-role agent vendor and model](configuration.md#multi-role-agent-vendor-and-model). A single global vendor is the zero-diff default that covers planner, worker, reviewer, and fixer until you add per-role bindings. Coordinator triage always uses the global agent only and is skipped when global vendor is unset.

Forgejo projects are onboarded by editing the config file's `providers` and `projects` sections (or importing `[[projects]]` at daemon startup). There is no `looper bootstrap --provider forgejo` and no `looper provider` CLI. See [configuration](configuration.md#provider-support).

Provider health is visible on the dashboard and via `GET /api/v1/status`; the stripped CLI has no `looper status` verb.

### Grok Build (xAI)

For xAI Grok Build, configure `agent.vendor = "grok-build"`; Looper runs the `grok` executable. Authenticate the daemon with `grok login --device-auth` or by providing `XAI_API_KEY` in its environment—never commit an API-key value. Looper defaults to `--always-approve --sandbox off` so Grok can update Git metadata outside a linked worktree; configure `--sandbox` explicitly if a stricter profile works with your repository layout.

Configured Grok arguments take precedence: `--permission-mode` can prompt or fail unattended work, a non-`plain` `--output-format` can break direct completion-marker parsing, and `-p`/`--single` replaces Looper's generated task prompt. Grok Build has no daemon native resume or interactive park-for-handwork path beyond the generic `looper takeover <selector>` loop park. Retries start with a fresh checkpoint prompt; Looper never uses ambient `--continue`.

## 1a. Local-only vs Routed projects

Looper supports two project modes:

- `network.mode=off` — local-only. Worker claims `looper:worker-ready` Issues assigned to the local GitHub user, Reviewer claims PRs with a review request for the local GitHub user, and `looper:target:*` labels do nothing.
- `network.mode=routed` — multi-Node. `loopernet` receives centralized webhook ingress, fans out wakeups to Nodes, and exposes the Coordinator lease used for fencing.

Routed mode keeps authorities separate:

- GitHub remains the work-intent authority.
- `looper:target:<node_name>` is only the exact-Node authority.
- the lease only decides which Coordinator may mutate GitHub.

That means `loopernet` never becomes the source of truth for Issue admission or PR review assignment, and it must not mutate GitHub directly.

## 1b. Routed setup and recovery

Before enrolling Nodes, deploy exactly one active `loopernet` instance per Network. For container examples, persistence requirements, and the current non-HA constraint, see [loopernet deployment](loopernet-deployment.md).

Typical Routed rollout:

1. join each Node to `loopernet` by configuring network membership in the config file / dashboard (the `looper network join` CLI was removed)
2. disable unsupported routed auto-discovery (`planner` and `fixer`) before opting projects into `network.mode=routed`
3. keep Worker and Reviewer identities stable per Node; duplicate GitHub identities are safe only because the exact target label disambiguates which Node may claim
4. restart `looperd` and confirm membership, identity, and lease state on the dashboard or `GET /api/v1/network/status`

Operator recovery rules:

- if the target label is removed, duplicated, changed, or stale before work starts, Worker/Reviewer will not claim it
- if `looper:worker-ready`, the GitHub assignee, or the review request disappears before processing starts, the queued item becomes unclaimable
- if webhook ingress or SSE wakeups degrade, polling continues as a fallback so Coordinator can repair drift
- if a stale target label remains after lease loss or a partial GitHub mutation, let Coordinator reconciliation repair or remove it before retrying

## 2. Project auto-detection from the current directory

Looper can often infer the target project from your current working directory.

In practice, this means that if you run commands from inside a registered project repo, you can usually omit `--project`.

This works best when:

- your current directory is inside exactly one registered project repo
- that project has a configured GitHub repo mapping

If no project matches the current directory, or multiple projects match, pass `--project` explicitly.

## 3. What each role does

| Role | Purpose | Common entrypoint |
| --- | --- | --- |
| `coordinator` | Proactively triages fresh issues and commits a Disposition with durable labels | runs automatically inside `looperd` |
| `planner` | Generates a spec from an issue and opens a spec PR | Label issue `looper:plan` + assign (or `POST /api/v1/planners`) |
| `reviewer` | Reviews a PR or spec PR and publishes GitHub reviews | Review-request / label discovery (or dashboard / API) |
| `fixer` | Fixes PR issues based on review comments and tries to resolve threads | `looper fix <repo>#<pr>` |
| `worker` | Implements the actual work from a spec or issue, and can reuse an existing PR | Label `looper:worker-ready` / `looper:spec-ready` (or `POST /api/v1/workers`) |

Forgejo MVP role support:

- Planner and Worker are supported over the Forgejo REST API.
- Reviewer supports native review requests and native `APPROVE`, `REQUEST_CHANGES`, and `COMMENT` reviews. A configured `summary_comment` publish mode retains the top-level Reviewer Summary compatibility protocol.
- Fixer is supported through two Forgejo-specific paths: Reviewer Summary items still flow through the top-level Fixer Summary PR comment, and manual/direct `looper fix` runs also read unresolved native Forgejo PR review comments and can resolve those native comments after validation, push, and post-push verification.
- Coordinator, auto-merge, routed network mode, and webhook modes remain unsupported for Forgejo.
- A Forgejo-only daemon can start without `gh`; mixed or GitHub projects still require `gh`.

## 4. Recommended flow

### Overview

1. Create a GitHub issue
2. Add the `looper:plan` label
3. Assign the issue to the currently authenticated `gh` user
4. Start `planner` so it creates a spec PR
5. Let `reviewer` review the spec PR
6. Let `fixer` address review comments until the review is clean
7. The PR gets the `looper:spec-ready` label
8. `worker` takes over that PR and continues implementation

This is the smoothest current Looper workflow.

## 5. Coordinator: proactive triage on fresh issues

Coordinator is Looper's intake role. It is proactive, not trigger-driven: on each poll it scans fresh open issues, runs a shallow repository-aware triage pass, then commits a durable Disposition back to GitHub.

### What Coordinator writes

For each fresh issue inside the configured bootstrap window, Coordinator picks one Disposition:

- `valid`
- `out-of-scope`
- `unclear`

It then:

1. clears any prior coordinator-owned labels (`kind/*`, `area/*`, `complexity/*`, `dispatch/*`, `wontfix`, `needs-info`)
2. applies the new labels for the chosen Disposition
3. posts or edits a triage comment marked with `<!-- looper:coordinator:triage -->`
4. applies `triaged` last as the durability commit

The `triaged` label means Coordinator has formed an opinion about the issue. Because that label is written last, the triage action is safe to re-run after a partial failure.

### Current triage outcomes

- `valid` adds one each of `kind/*`, `area/*`, `complexity/*`, and `dispatch/*`
- `out-of-scope` reuses the existing `wontfix` label and leaves the issue open
- `unclear` adds `needs-info` and asks the author for clarification

### Re-triage loop for `needs-info`

If an issue is in the `unclear` state and the original author replies after `needs-info` was applied, Coordinator removes both `needs-info` and `triaged` and immediately re-runs triage in the same poll. That lets the issue move back through triage without requiring the author to know Looper's label vocabulary.

### Dispatch after triage

Once an issue is already `triaged` and carries exactly one `dispatch/*` label, Coordinator can hand it off in one of two modes:

- **human-gated** (default)
- **autonomous**

#### Human-gated slash commands

Coordinator watches issue comments for slash commands at the **start of a line**:

- `/plan` → applies the planner trigger label from `roles.planner.triggers.labels[0]`
- `/implement` → applies the worker trigger label from `roles.worker.triggers.labels[0]`

The commenter must either:

- have repository permission `write`, `maintain`, or `admin`, or
- be listed in `roles.coordinator.dispatch.humanGate.allowedUsers`

On success, Coordinator:

1. assigns `roles.coordinator.dispatch.assignTo` when configured
2. applies the derived trigger label as the durability commit
3. reacts 👍 on the slash-command comment

If the trigger label is already present, Coordinator treats the command as an idempotent re-issue and still reacts 👍.

If the issue is missing `triaged` or a matching `dispatch/*`, Coordinator reacts with GitHub's `confused` reaction and posts one short failure comment marked with `<!-- looper:coordinator:dispatch-failure -->`.

#### Autonomous mode

When `roles.coordinator.dispatch.mode = "autonomous"`, Coordinator no longer waits for a slash command. Instead it dispatches after the issue has stayed `triaged` for `roles.coordinator.dispatch.autonomous.delayMinutes`.

Autonomous dispatch still derives the trigger label from Planner or Worker config and still writes that trigger label last.

#### Veto signals

Autonomous dispatch stops immediately when any veto signal is present:

- the `dispatch/*` label is gone
- the global hold label `looper:hold` (or the configured override) is present
- the destination trigger label is already present because a human dispatched manually

`looper:hold` is the operator-facing global hold contract for Coordinator dispatch.

`looper:hold` blocks Coordinator's autonomous Dispatch, but it does not directly block Reviewer's `gh pr merge --auto` call once a PR is already open. If `looper:hold` causes the linked Issue to skip re-Triage, merge-watch's `BranchProtectionChanged` re-Triage action becomes a silent no-op, which is the intended hold semantic.

## 6. Planner: from issue to spec PR

### Start it manually

```bash
# intent: start planner for issue 123 — label looper:plan + assign, or POST /api/v1/planners
```

This creates a `planner` loop targeting that issue.

For `plan`, it is safest to pass `--project` explicitly.

### Auto-discovery conditions

For planner auto-discovery, an issue must:

- have the `looper:plan` label
- be assigned to the current GitHub user
- belong to a repo that maps uniquely to a local project

So the most common GitHub-side trigger is:

1. create an issue
2. add `looper:plan`
3. assign it to yourself

### What planner does

Planner will:

- add the current GitHub user as an issue assignee when the issue is claimed, preserving any existing assignees
- create a worktree
- write the spec file
- push a spec PR
- add the `looper:spec-reviewing` label to that PR
- request reviewers when appropriate

If planner cannot assign the issue in GitHub, it reports a retryable failure rather than continuing with ambiguous ownership.

## 7. Reviewer: review a spec PR or a normal PR

### One-time review

```bash
# intent: review that PR — discovery via review-request or looper:spec-reviewing
```

If you are already inside the registered repo, this usually also works:

```bash
# intent: review PR 42 in the registered repo
```

### Continuous review

```bash
# intent: continuous review — ensure review-request/discovery; use dashboard for loop control
```

Use this when new commits are expected to keep landing on the PR.

### Reviewer auto-discovery rules

Reviewer mainly watches two kinds of PRs:

- open PRs where the current GitHub user was requested as a reviewer
- manually-started reviewer loops from this machine, including `# intent: continuous review — ensure review-request/discovery; use dashboard for loop control`

For the default review-requested path, Looper asks GitHub for PRs requested from the current user instead of only filtering the first page of open PRs locally.

For spec PRs, `looper:spec-reviewing` marks the review phase, but it does not by itself authorize other users' Looper instances to run. Request review from the intended GitHub user to trigger that user's automatic reviewer.

For Forgejo projects, reviewer auto-discovery defaults to review requests. Configured labels can be used independently or combined with review requests; combined results are deduplicated deterministically. Reviewer publishes native `APPROVE`, `REQUEST_CHANGES`, or `COMMENT` reviews according to configuration and preserves Looper disclosure/idempotency markers. Self-authored PRs are skipped by default; when self-review is enabled, an attempted clean approval is explicitly downgraded to `COMMENT`. Set reviewer `publishMode` to `summary_comment` to keep the legacy Reviewer Summary/Fixer Summary workflow.

### What happens after reviewer finishes

If reviewer considers the spec review clean, it will:

- remove `looper:spec-reviewing`
- add `looper:spec-ready`

"Clean" means:

- there are no unresolved review threads
- the review decision is not `CHANGES_REQUESTED`

### Reviewer auto-merge

Reviewer can close a Looper code Issue end-to-end without a human pressing Merge.

Prerequisites:

- branch protection on the base branch with required checks
- repo-level **Allow auto-merge** enabled
- repo-level merge strategy enabled for the configured strategy (`squash`, `merge`, or `rebase`)
- linked Issue body includes a `## Acceptance criteria` section

Configuration lives under `roles.reviewer.autoMerge.*`, with the usual `projects[].roles.reviewer.autoMerge.*` overrides for project-specific repos.

When auto-merge is enabled and the PR is in scope, Reviewer verifies every acceptance-criteria checkbox against the diff before it submits APPROVE. The approval body includes a per-criterion evidence section pointing at files and lines. If every criterion passes, Reviewer calls `gh pr merge --auto`; GitHub branch protection remains the authority for whether the PR actually merges.

Comment markers used by this flow:

- `<!-- looper:reviewer:criteria-fail -->` — Reviewer found at least one acceptance criterion without satisfying evidence in the diff and returned the linked Issue to re-Triage
- `<!-- looper:reviewer:automerge-refused -->` — Reviewer approved the PR, but GitHub repo settings or branch protection refused the auto-merge opt-in
- `<!-- looper:coordinator:merge-watch retries=N -->` — Coordinator is watching a merge-pending PR and carrying retry state on the linked Issue

Human override is silent: if someone clicks **Disable auto-merge** on the PR, Looper respects it and does not re-enable auto-merge just because an earlier Reviewer pass opted in.

Auto-merge is not engaged for Spec PRs, PRs whose linked Issue has no `## Acceptance criteria` section, or PRs outside the configured auto-merge scope.

## 8. Fixer: repair a PR based on review feedback

The most direct way to use fixer is to start it for a specific PR:

```bash
looper fix owner/repo#42
```

If you are already inside the registered repo, you can usually use the PR number by itself:

```bash
looper fix 42
```

Use this when you want to force a repair pass on demand before waiting for any automatic fixer trigger.

Fixer will:

- read pending review comments and threads on the PR
- create a worktree and apply each listed repair; when the link is clear, prefer a complete coherent fix of that root cause across the affected dependency chain (unrelated cleanup and speculative hardening stay out of scope)
- run validation
- push back to the same PR branch
- after validation and push succeed, try to resolve only the review threads that were both verified by Looper and explicitly confirmed by the fixer agent

For Forgejo projects, automatic Fixer runs are summary-only because Forgejo's public REST API does not currently expose a native review-comment resolve mutation:

- reviewer-summary items still come from the top-level Reviewer Summary comment
- native Forgejo PR review comments do not trigger automatic Fixer runs, even when their response includes a `resolver` field
- the `resolver` response field describes state; it is not treated as proof that a resolve mutation exists
- explicit manual Fixer runs may inspect native comments, but stop with a manual-intervention error when the provider cannot resolve them

If the PR is still in the spec review phase and the review becomes clean, fixer can also move the labels from:

- `looper:spec-reviewing` → `looper:spec-ready`

In practice, `reviewer` and `fixer` often alternate until the spec PR is ready for `worker`.

## 9. Worker: do the actual implementation

### Start from an issue

```bash
# intent: worker for issue 123 — label looper:worker-ready or looper:spec-ready
```

This is the recommended entrypoint.

If you are already inside the target repo, you can usually omit `--project`:

```bash
# intent: worker for issue 123 — label looper:worker-ready or looper:spec-ready
```

If that issue already has a related planner loop, worker will try to reuse planner output, including:

- `specPath`
- an existing open PR

That means issue → planner → worker can flow through without manually copying the spec path.

When worker claims an issue, it adds the current GitHub user as an assignee and preserves any existing assignees. If GitHub assignment fails, the claim reports a retryable failure instead of silently continuing with ambiguous ownership.

For Forgejo projects, Worker does not claim issues by mutating assignees. The issue must already be assigned to the current Forgejo provider user, and Worker re-checks that assignment before side effects.

### Start directly from a spec

```bash
# intent: worker with explicit spec — use dashboard/API; CLI work verb removed
```

### What happens when worker takes over a `spec-ready` PR

When worker starts against a PR target, it first removes:

- `looper:spec-ready`

That signals the PR has been claimed for implementation.

## 10. How the GitHub label system works

These are the most important labels right now:

| Label | Used on | Meaning |
| --- | --- | --- |
| `triaged` | issue | Coordinator finished triage and committed a Disposition |
| `needs-info` | issue | Coordinator marked the issue `unclear` and is waiting for the author |
| `dispatch/*` | issue | Coordinator's durable dispatch intent for later hand-off |
| `looper:plan` | issue | This issue is eligible for planner auto-pickup |
| `looper:spec-reviewing` | PR | This PR is in the spec review phase |
| `looper:spec-ready` | PR | The spec is approved and ready for worker |
| `looper:needs-human` | PR | Reserved for manual intervention cases |
| `looper:hold` | issue or PR | Block all automatic Looper activity on that item |
| `looper:hold:worker` | issue or PR | Block automatic worker activity on that item |
| `looper:hold:fixer` | issue or PR | Block automatic fixer activity on that item |
| `looper:hold:reviewer` | issue or PR | Block automatic reviewer activity on that item |

Treat these as stage signals, not just descriptive labels.

## 11. How assign and review-request trigger automation

The two most important assignment-related rules are:

### Planner

For planner auto-discovery, the issue must:

- be assigned to the current GitHub user
- also have `looper:plan`

So:

> Adding only the label is not enough. The issue also needs to be assigned to the right person.

### Reviewer

Reviewer automatically pays attention to:

- PRs where the current GitHub user has a review request
- manual reviewer loops created on this machine

The `looper:spec-reviewing` label is a phase marker; automatic review still requires a review request unless the loop was explicitly started locally.

## 12. Hold labels

Looper's official hold labels are:

- `looper:hold`
- `looper:hold:worker`
- `looper:hold:fixer`
- `looper:hold:reviewer`

Semantics:

- `looper:hold` blocks all automatic Looper activity for the labeled issue or PR.
- `looper:hold:worker` blocks only automatic worker activity.
- `looper:hold:fixer` blocks only automatic fixer activity.
- `looper:hold:reviewer` blocks only automatic reviewer activity.
- planner is special: only `looper:hold` blocks planner.
- there is no issue/PR inheritance.
- Looper never adds or removes hold labels.
- removing a hold takes effect on the next normal scan.
- only API create requests with `force=true` (or equivalent dashboard actions) can bypass hold; the old `looper work/review/fix --force` CLI is gone.

Create-time CLI/API hold validation is best-effort only when the local project repo path or configured `gh` path is unavailable. If those are present but remote `gh` inspection fails, creation fails fast. Automatic discovery and runtime checks still use live remote labels as authority whenever Looper can fetch them.

## 13. Common GitHub / PR commands

Inspect PRs with `gh` (the old `looper pr *` verbs are gone):

```bash
gh pr list
gh pr view 42
gh pr checks 42
```

Create a reviewer task:

```bash
# intent: review that PR — discovery via review-request or looper:spec-reviewing
# intent: continuous review — ensure review-request/discovery; use dashboard for loop control
# intent: continuous review of PR 42 in the registered repo
```

Start fixer for an existing PR:

```bash
# looper loop start  # removed — fixer discovery + dashboard control
```

## 14. How to inspect current activity

```bash
# looper ps  # removed — use dashboard
# looper describe  # removed — use dashboard
# intent: stream logs for loop 12 — open the dashboard loop detail view
# intent: open worktree for loop 12 — copy path from dashboard dirty-worktree dialog
looper stop 12
# looper run reconcile-stale  # removed
```

Typical usage (stripped CLI + dashboard):

- Dashboard loops list: see which loops are running and truncated failure reasons
- Dashboard loop detail: diagnosis, logs, and worktree path
- Dirty-worktree dialog: copy a shell-quoted `cd -- '<path>'` when you need a terminal
- Worktree cleanup runs on the daemon schedule (`daemon.worktreeCleanup`); no CLI verb
- `looper stop <selector>`: stop an active loop
- `looper pause` / `retry` / `takeover` / `handback` / `respond`: remaining control surface
- After sleep/wake, restart `looperd` if loops look stuck

## 15. Minimal end-to-end example

### Option A: start from an issue

1. Create GitHub issue `#123`
2. Add the `looper:plan` label
3. Assign it to the current `gh` user
4. Run:

```bash
# intent: start planner for issue 123 — label looper:plan + assign, or POST /api/v1/planners
```

5. Wait for planner to open a spec PR
6. Run reviewer:

```bash
# intent: review the spec PR — discovery via looper:spec-reviewing / review-request
```

7. If comments appear, start fixer:

```bash
# looper loop start  # removed — fixer discovery + dashboard control
```

8. Once the PR reaches `looper:spec-ready`, start worker:

```bash
# intent: worker for issue 123 — label looper:worker-ready or looper:spec-ready
```

### Option B: manage an existing PR directly

```bash
# intent: continuous review + fixer on PR 42 —
# ensure review-request/discovery on a registered project; control loops via dashboard or:
#   looper stop|pause|retry|takeover <selector>
```

This is useful when you already have a PR and only want Looper to handle the review/fix cycle.

### Option C: take over one PR in a single command

The one-command PR-scoped background takeover (`looper takeover <owner>/<repo>#<pr>` and `scripts/takeover.sh`) was removed with the old CLI. Today: register the repo, let discovery claim the PR via labels/review-requests, and use the surviving control verbs on **existing** loops. For single-PR babysitting without the daemon, use the live [`pr-takeover` skill](../skills/pr-takeover/SKILL.md) with `gh` + `git`.

Historical Option B (`review --loop` + fixer loop start) is also gone as CLI. Prefer discovery + dashboard control. Notes on the old takeover UX (kept for migration context only — commands no longer work):

```bash
# from inside the repo checkout
looper takeover                 # detect the current branch's PR
looper takeover owner/repo#42   # or name it explicitly
looper takeover owner/repo#42 --merge   # also auto-merge once approved + green
```

`takeover`:

1. installs/starts the managed daemon if needed;
2. writes (or reuses) `~/.looper/config.json`, registering the repo as a project whose `planner` / `worker` / `fixer` / `reviewer` discovery loops are all disabled — so the daemon never picks up any *other* PR or issue in the repo;
3. starts a continuous reviewer loop and fixer loop on the target PR (skip the fixer with `--no-fix`);
4. with `--merge`, sets `roles.reviewer.autoMerge.enabled` for the project so the reviewer enables GitHub auto-merge once the PR is approved and checks are green.

Agent selection: `takeover` reuses the vendor already in your config; otherwise it auto-detects an installed `claude` / `codex` / `grok` / `opencode` CLI, prompts when the choice is ambiguous, and accepts `--agent-vendor` plus `--yes` for non-interactive runs. Auto-merge still depends on the repository allowing it (and, by default, on branch protection with required checks); when GitHub refuses, the reviewer keeps reviewing and reports why instead.

Manage and stop takeovers:

```bash
# looper takeover list  # removed; use the dashboard loops view
# looper takeover stop  # removed; use looper stop <selector> on the loops owner/repo#42   # stop this takeover's reviewer + fixer loops
# looper takeover stop  # removed; use looper stop <selector> on the loops --all           # stop every takeover
```

`takeover list` / `stop` are backed by a local index at `~/.looper/takeovers.json`; stopping closes the underlying loops by id (so it works even while they are idle/waiting between commits).

## 16. Quick decision guide

- You have an issue but no spec yet: use `planner`
- You have a PR that needs review: use `reviewer`
- A PR already has review comments to address: use `fixer`
- The spec is ready and implementation should begin: use `worker`
- You want Looper on just one PR until it merges (no repo-wide automation): use `takeover`

As a rule of thumb:

- inside a registered repo, you can usually omit `--project` for `review` and `work`
- use `--project` when you are outside the repo, or when Looper cannot infer the project uniquely
- for `plan`, prefer passing `--project`

## 17. Authentication

Looper uses `gh` for GitHub access, so `gh auth status` should succeed before you start planner / reviewer / fixer / worker workflows.

If the daemon is configured with `server.authMode=local-token`, the CLI also needs a matching local token. In that setup, export `LOOPER_TOKEN` before running CLI commands.

Example:

```bash
export LOOPER_TOKEN=replace-me
curl -sS "http://127.0.0.1:17310/api/v1/status"
```

This is separate from GitHub authentication.

## 18. Webhook delivery status

When webhook mode is enabled, `curl -sS "http://127.0.0.1:17310/api/v1/webhook/status"` reports the active delivery mode.

In `gh-forward` mode it reports each local `gh webhook forward` subprocess.

- `adopted=true` means this daemon boot safely reattached to a forwarder spawned by a previous boot. Adopted processes do not have stdout/stderr tails because the new daemon did not create their pipes.
- `latched=true` means `gh webhook forward` exited with a terminal error and Looper will not respawn-loop it. `latchReason` contains the matched error and remediation. For `Hook already exists`, delete the conflicting GitHub webhook or fix `gh` authentication, then restart `looperd`.
- polling remains the correctness fallback while a forwarder is latched or degraded.

In `tunnel` mode Looper creates a GitHub repository webhook and listens on `127.0.0.1:<webhook.listenPort>`. You run your own tunnel to that listener:

```bash
cloudflared tunnel --url http://127.0.0.1:8765
```

Then set `webhook.publicBaseUrl` to the stable HTTPS URL for that tunnel and restart `looperd`. GitHub deliveries go to:

```text
{publicBaseUrl}/webhook/{owner}/{repo}
```

Useful tunnel commands:

```bash
curl -sS "http://127.0.0.1:17310/api/v1/webhook/status"
# looper webhook rotate owner/repo  # removed with the old CLI
# looper webhook list-orphans  # removed with the old CLI
# looper webhook delete owner/repo --confirm  # removed with the old CLI
```

- `latched=true` on a tunnel hook means GitHub disabled it repeatedly and Looper stopped re-enabling it; polling fallback continues.
- `orphaned=true` means the repo was removed from config or switched away from `tunnel`. Looper retained the hook id locally so explicit deletion is still by id.
- `rotate` changes the per-repo HMAC secret and updates the existing hook by id.
- `delete --confirm` is the only command that removes a Looper-managed tunnel hook from GitHub.

## 19. One important clarification

In the current implementation, "automatic triggering" is closer to:

- the daemon continuously polling GitHub state
- loops discovering targets based on labels, assignees, and review requests

It is not a GitHub webhook-driven instant trigger.

So if you want the automation chain to work reliably, the most important things are:

- keep `looperd` running
- keep `gh` working
- set labels and assignments correctly
