# Looper Quick User Guide

This guide is for everyday users. It focuses on how `coordinator`, `planner`, `reviewer`, `fixer`, and `worker` interact with GitHub issues and PRs.

> **CLI strip (read first).** The full `looper` CLI was removed ahead of the role-model rewrite. The **only** operator verbs are:
>
> `init` · `status` · `project add|list|discover` · `start` · `pause` · `retry` · `stop` · `close` · `takeover` · `handback` · `respond` · `version`
>
> (plus machine-only `review submit`). There is no `bootstrap`, `daemon *`, `ps`, `logs`, `jump`, `plan`, `review`, `fix`, `work`, or `webhook`/`provider`/`network` admin. Beyond `init`, `status`, and `project add|list|discover`, every verb the CLI kept acts on a loop that **already exists**; nothing in the CLI creates one. Loops start from forge state — labels, assignment, review requests — picked up by daemon discovery, or from the create endpoints (`POST /api/v1/planners`, `POST /api/v1/workers`, or `POST /api/v1/loops` for any role). Inspection is the dashboard and the HTTP API. Where a removed verb still appears below, it is marked as removed and paired with what to do instead. Current install surface: [installation.md](installation.md) and the repository README.

## 1. Prerequisites

Make sure these work first:

```bash
command -v looperd
looper status   # config, daemon liveness, review/orphan ops lines, projects
gh auth status  # GitHub projects only
```

If the project is not registered yet:

```bash
looper project add /absolute/path/to/repo
```

That path must be the repository root — the directory containing `.git`. For advanced registration, `POST /api/v1/projects` accepts the same `repoPath` plus optional `id`, `name`, `baseBranch`, `worktreeRoot`, `repo`, and `snapshotMode` fields. The dashboard only lists registered projects; it does not create them. Provider bindings are config-file-only: declare the complete project under `[[projects]]` and restart the daemon.

Webhook mode is configured in the config file (`webhook.mode` and per-project overrides). Observe health with `GET /api/v1/webhook/status` or the dashboard. Clearing stale GitHub CLI forwarder hooks is a manual `gh api` operation after you confirm the dry-run payload — there is no `looper webhook cleanup`.

Also make sure:

- `looperd` is running (foreground, or under your own process manager)
- your local repo can `git fetch` and `git push`
- GitHub projects: `gh` is authenticated with the target GitHub account
- each coding role you want to run can resolve a vendor: set global `agent.vendor` in the config (for example `vendor = "opencode"`), or supply vendor via `agent.profiles` / `roles.<role>.agent` as described in [Multi-role agent vendor and model](configuration.md#multi-role-agent-vendor-and-model). A single global vendor is the zero-diff default that covers planner, worker, reviewer, and fixer until you add per-role bindings. Coordinator triage always uses the global agent only and is skipped when global vendor is unset.

`GET /api/v1/status` reports service, storage, scheduler, agent, webhook, loop, network, safety, notification, and tool state; there is no per-provider health surface. `looper status` reports the config file, daemon reachability, and the registered projects.

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

## 2. How Looper resolves the project

Looper only acts on **registered projects**. Register a project with `looper project add /absolute/path/to/repo`, or use `POST /api/v1/projects` with the repo's absolute `repoPath` when you need the advanced fields. The dashboard only lists registered projects. The daemon polls every registered project on each discovery pass.

There is no current-directory inference and no `--project` flag: the stripped CLI has neither, and the daemon's create endpoints take an explicit project id.

- `POST /api/v1/planners` and `POST /api/v1/loops` require `projectId`; `POST /api/v1/workers` takes `projectId`, or infers it from `repo` + `prNumber`
- read-only lookups such as `GET /api/v1/pull-requests/{repo}/{number}` accept an optional `projectId`, and fail with a "multiple projects match" conflict when the same repo is registered under more than one project
- the surviving control verbs (`looper stop`, `pause`, `retry`, …) take a loop selector, not a project — the loop already knows which project it belongs to

## 3. What each role does

| Role | Purpose | Common entrypoint |
| --- | --- | --- |
| `triager` | Internally classifies new/reopened GitHub issues, persists a triage report, and safely routes eligible reports to Planner | runs automatically when GitHub Planner discovery is enabled and Coordinator is off |
| `coordinator` | Proactively triages fresh issues and commits a Disposition with durable labels | runs automatically inside `looperd` |
| `planner` | Generates a spec from an issue and opens a spec PR | Label issue `looper:plan` + assign (or `POST /api/v1/planners`) |
| `reviewer` | Reviews a PR or spec PR and publishes GitHub reviews | Review request under the default policy; configured label policy or `POST /api/v1/loops` otherwise |
| `fixer` | Fixes PR issues based on review comments and tries to resolve threads | Discovery on open PRs with actionable threads (or `POST /api/v1/loops`) |
| `worker` | Implements the actual work from a spec or issue, and can reuse an existing PR | Label the issue `looper:worker-ready` (or `POST /api/v1/workers`) |

## 4. Recommended flow

### Overview

1. Create a clear GitHub issue
2. Triager applies the configured admission policy, persists its explainable decision, and routes admitted work or classifies work needing assessment
3. When admission requires assessment—or the legacy report is risky, uncertain, or incomplete—Triager comments with the exact `/plan <confirmation token>` command; a collaborator with write access replies with it. Anything typed after the command is passed to Planner as a clarification that supersedes the issue body
4. Planner creates a spec PR
5. Let `reviewer` review the spec PR
6. Let `fixer` address review comments until the review is clean
7. The PR gets the `looper:spec-ready` label
8. Add `looper:worker-ready` to the linked issue and keep it assigned; `worker` then reuses the approved spec PR and continues implementation

Triager reports—not routing labels—are the semantic authority for this automatic GitHub intake path. The existing `looper:plan` label plus assignment remains an explicit label-discovery path and continues to support non-Triager providers.

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

The normal trigger is the label plus the assignment (below). To create the loop immediately instead of waiting for the next discovery poll, post it:

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/planners" \
  -H 'Content-Type: application/json' \
  -d '{"projectId":"<project-id>","issueNumber":123}'
```

This creates a `planner` loop targeting that issue. `projectId` is required — planner has no repo or current-directory inference.

### Triager route

For a GitHub project with Planner auto-discovery enabled and Coordinator disabled, new and reopened issues enter the internal Triager without a routing label. Each poll searches GitHub's recently updated Issues in updated-event order, with a lookback of at least five minutes, so enabling the role does not sweep the historical open backlog. Triager records `triage.enrolled`, then applies `roles.triager` to forge-owned facts: author association, repository visibility, bot type, and the global hold. The resulting `triage.report` records the outcome, author tier, preset, visibility, and exact deciding rule. Replays reuse that durable decision.

With the default `legacy` preset, behavior is unchanged: only an in-scope, low-risk decision with confidence of at least `0.8`, no missing information, and `planner` as the recommended next role routes automatically. Relationship presets instead produce `auto`, `assess`, or `ignore`: `auto` routes without a model call, `assess` optionally classifies and waits for a human, and `ignore` records and stops. A repository collaborator with `write`, `maintain`, or `admin` permission confirms an awaiting report with its exact `/plan <confirmation token>` command. Looper records `triage.confirmed` before routing and `triage.routed` after Planner accepts the projection, so replay does not repeat admission or classification.

### Label-based auto-discovery conditions

For the existing label-based Planner discovery path, an issue must:

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
- when a project configures Planner escalation criteria, explore the repository and persist a structured suitability assessment before authoring the spec
- write the spec file
- push a spec PR
- add the `looper:spec-reviewing` label to that PR
- request reviewers when appropriate

If planner cannot assign the issue in GitHub, it reports a retryable failure rather than continuing with ambiguous ownership.

### Planner escalation to a human

Projects can opt into explicit Planner escalation criteria under `projects[].roles.planner.escalation` (or set global defaults under `roles.planner.escalation`). The policy, rather than the model, is the authority that stops a run. Planner's repository assessment is persisted in the run checkpoint with every fired criterion, its evidence, and the specific decision requested.

```toml
[[projects]]
id = "demo"

[projects.roles.planner.escalation]
maxEstimatedFiles = 12
maxEstimatedPackages = 3
publicApi = true
config = true
cli = true
storage = true
wireFormat = true
adrConflict = true
authorityDecision = true
```

When a configured criterion fires, the loop becomes `awaiting_human`; the run is interrupted without recording a failure or scheduling a retry. Both `looper status` and the dashboard show that state separately from running and failed work. Answer with `looper respond <loop> "authorize proceed"` to continue from the persisted assessment without rediscovery, or `looper respond <loop> "close without a spec"` to complete the Planner loop without writing or publishing a spec. Omitted criteria are disabled, so existing Planner behavior is unchanged by default.

## 7. Reviewer: review a spec PR or a normal PR

### How to get a review

There is no `looper review` verb (`looper review submit` is the machine-only path reviewer agents use to publish, not a way to start one). Reviewer loops come from forge state:

```bash
gh pr edit 42 --add-reviewer <login>              # any PR: request a review
gh pr edit 42 --add-label looper:spec-reviewing   # spec PR: mark the review phase
```

`<login>` is the GitHub user whose Looper instance should do the review: an instance only picks up PRs requested from *its own* authenticated user. GitHub will not let you request a review from a PR's own author. For automatic self-review, set both `roles.reviewer.discovery.triggers.enableSelfReview = true` and `roles.reviewer.discovery.triggers.requireReviewRequest = false`; changing only `enableSelfReview` still leaves the default review-request gate in place. To avoid broadening automatic discovery, create a manual reviewer loop with `POST /api/v1/loops` instead.

To create the loop directly rather than wait for the next poll, post the required project and PR target explicitly:

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/loops" \
  -H 'Content-Type: application/json' \
  -d '{"projectId":"<project-id>","type":"reviewer","targetType":"pull_request","repo":"owner/repo","prNumber":42}'
```

The dashboard controls existing loops; it cannot create one.

There is no longer a one-time vs continuous mode to choose: a reviewer loop stays with its PR through the review/fix cycle, and a fixer push queues a fresh review rather than waiting for the next coordinator pass.

### Reviewer auto-discovery rules

Reviewer mainly watches two kinds of PRs:

- open PRs where the current GitHub user was requested as a reviewer
- reviewer loops that already exist on this machine, which keep following their PR

For the default review-requested path, Looper asks GitHub for PRs requested from the current user instead of only filtering the first page of open PRs locally.

For spec PRs, `looper:spec-reviewing` marks the review phase, but it does not by itself authorize other users' Looper instances to run. Request review from the intended GitHub user to trigger that user's automatic reviewer.

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

For current-head Codex-review enforcement, set `roles.gatekeeper.trust = "auto"`
and require the `Looper Gatekeeper` status in the target branch's protection
rule. Gatekeeper binds the status to the current commit: a push has no inherited
success status, so GitHub and Mergify wait until Codex reviews that new head.

**Caveats:** status publishes on the pull request head SHA only — not on GitHub
native merge-queue merge-group commits. Branch protection must require
`Looper Gatekeeper`; a failing optional status does not block merge. Do not
enable `roles.auditor` on the same project while gatekeeper trust is `auto`;
Auditor requires merge-outcome evidence that status-only auto does not emit
(see [configuration](configuration.md#merge-gatekeeper-trust-level-rolescatekeepertrust)).
Post-merge digest likewise only sees Coordinator merge-watch merges (and
historical Gatekeeper merge-outcome rows) while `auto` is status-only.

Comment markers used by this flow:

- `<!-- looper:reviewer:criteria-fail -->` — Reviewer found at least one acceptance criterion without satisfying evidence in the diff and returned the linked Issue to re-Triage
- `<!-- looper:reviewer:automerge-refused -->` — Reviewer approved the PR, but GitHub repo settings or branch protection refused the auto-merge opt-in
- `<!-- looper:coordinator:merge-watch retries=N -->` — Coordinator is watching a merge-pending PR and carrying retry state on the linked Issue

Human override is silent: if someone clicks **Disable auto-merge** on the PR, Looper respects it and does not re-enable auto-merge just because an earlier Reviewer pass opted in.

Auto-merge is not engaged for Spec PRs, PRs whose linked Issue has no `## Acceptance criteria` section, or PRs outside the configured auto-merge scope.

## 8. Fixer: repair a PR based on review feedback

There is no `looper fix` CLI. Fixer starts via discovery on open PRs with actionable review threads (authored by the configured user), or via `POST /api/v1/loops` with `type: "fixer"` when you need a forced repair pass. The dashboard controls existing loops; it cannot create one.

Fixer will:

- read pending review comments and threads on the PR
- create a worktree and apply each listed repair; when the link is clear, prefer a complete coherent fix of that root cause across the affected dependency chain (unrelated cleanup and speculative hardening stay out of scope)
- run validation
- push back to the same PR branch
- after validation and push succeed, try to resolve only the review threads that were both verified by Looper and explicitly confirmed by the fixer agent

If the PR is still in the spec review phase and the review becomes clean, fixer can also move the labels from:

- `looper:spec-reviewing` → `looper:spec-ready`

In practice, `reviewer` and `fixer` often alternate until the spec PR is ready for `worker`.

## 9. Worker: do the actual implementation

### Start from an issue

Label the issue `looper:worker-ready` and assign it to the current forge user. Discovery picks it up on the next poll. This is the recommended entrypoint. `looper:spec-ready` belongs on an approved spec **PR**; worker issue discovery does not consume that label.

To create the loop directly instead of waiting for discovery:

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/workers" \
  -H 'Content-Type: application/json' \
  -d '{"projectId":"<project-id>","issueNumber":123}'
```

If that issue already has a related planner loop, worker will try to reuse planner output, including:

- `specPath`
- an existing open PR

That means issue → planner → worker can flow through without manually copying the spec path.

When worker claims an issue, it adds the current GitHub user as an assignee and preserves any existing assignees. If GitHub assignment fails, the claim reports a retryable failure instead of silently continuing with ambiguous ownership.

### Start directly from a spec

The removed `looper work` verb took a spec path. The daemon endpoint still does — pass `specPath` (with `title` or `prompt`) instead of `issueNumber`:

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/workers" \
  -H 'Content-Type: application/json' \
  -d '{"projectId":"<project-id>","title":"Implement the cache layer","specPath":"specs/2026-04-17-cache/spec.md"}'
```

### Hand an existing PR to worker

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/workers" \
  -H 'Content-Type: application/json' \
  -d '{"projectId":"<project-id>","prNumber":42}'
```

Worker adopts the PR's head branch and pushes to it rather than opening a new PR. It needs a spec path to work from: normally the one written into the PR body, but you can pass `specPath` alongside `prNumber` when the body carries no spec marker.

The PR does not have to have been discovered or reviewed first — if the daemon has no snapshot of it, it reads the PR from the forge. Merged and closed PRs are refused. The loop takes the same per-PR lock as reviewer and fixer, so it queues behind any run already working that PR instead of racing it.

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
| `auto-merge` | PR | Gatekeeper's eligible route into the Mergify queue |
| `needs-human-review` | PR | Gatekeeper's escalation route; blocks the Mergify queue |
| `looper:hold` | issue or PR | Block all automatic Looper activity on that item |
| `looper:hold:worker` | issue or PR | Block automatic worker activity on that item |
| `looper:hold:fixer` | issue or PR | Block automatic fixer activity on that item |
| `looper:hold:reviewer` | issue or PR | Block automatic reviewer activity on that item |

Treat these as stage signals, not just descriptive labels.

## 11. How assign and review-request trigger automation

The two most important assignment-related rules are:

### Planner

For label-based planner auto-discovery, the issue must:

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

Get a reviewer onto a PR — there is no `looper review` verb, so this is done on the forge:

```bash
gh pr edit 42 --add-reviewer <login>              # review request: the main reviewer trigger
gh pr edit 42 --add-label looper:spec-reviewing   # spec PRs: mark the review phase
```

Get a fixer onto a PR: let the reviewer publish its findings first — fixer discovery reads those and the loop appears on its own. See [section 8](#8-fixer-repair-a-pr-based-on-review-feedback).

Either loop can also be created directly with `POST /api/v1/loops` and the matching `type`, when you do not want to wait for the next discovery poll.

## 14. How to inspect current activity

Inspection moved to the dashboard and the API; the CLI kept only the verbs that change a loop.

```bash
curl -sS "http://127.0.0.1:17310/api/v1/loops"        # what exists
curl -sS "http://127.0.0.1:17310/api/v1/runs/active"  # what is running now
looper stop 12                                        # act on one of them

# looper ps  # removed — use dashboard
# looper describe  # removed — use dashboard
# looper logs  # removed — use the dashboard loop detail view
# looper jump  # removed — copy the path from the dashboard dirty-worktree dialog
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

Every step below is a forge action. Nothing here needs the CLI: the daemon discovers each stage on its next poll, and you watch the loops on the dashboard.

### Option A: start from an issue

1. Create GitHub issue `#123`
2. Add the `looper:plan` label
3. Assign it to the current `gh` user — planner needs both the label and the assignment
4. Planner picks it up and opens a spec PR (or create the loop now with `POST /api/v1/planners`)
5. Request a review on that spec PR from the GitHub user running reviewer. `looper:spec-reviewing` only marks the phase and does not replace the review request under the default policy; alternatively create a manual reviewer loop with `POST /api/v1/loops`
6. If reviewer leaves findings, fixer starts on its own and pushes repairs to the same branch
7. Reviewer and fixer alternate until the review is clean and the PR reaches `looper:spec-ready`
8. Add `looper:worker-ready` to the linked issue and keep it assigned. Worker discovery consumes the issue label and reuses the approved spec PR; alternatively create the worker directly with `POST /api/v1/workers`

### Option B: manage an existing PR directly

You already have a PR and only want Looper to handle the review/fix cycle:

1. make sure the PR's repo is a registered project
2. request a review from the intended forge user on the PR — that is what reviewer discovery watches
3. reviewer runs, fixer follows on its findings, and the two alternate as in Option A
4. steer the resulting loops with `looper stop|pause|retry|takeover <selector>` or the dashboard

### Option C: babysit one PR without repo-wide automation

The one-command background path (`looper takeover <owner>/<repo>#<pr>`, `scripts/takeover.sh`, and `takeover list|stop`) was removed with the old CLI.

Today:

1. Register the repo with `looper project add /absolute/path/to/repo` or `POST /api/v1/projects`.
2. Let discovery claim the PR through a review request (or an explicitly configured label policy), **or** drive the PR live with the [`pr-takeover` skill](../skills/pr-takeover/SKILL.md) (`gh` + `git` in your agent session).
3. Control **existing** loops with `looper stop|pause|retry|takeover|handback|respond <selector>` — where `takeover` now means *park this loop for manual worktree work*, not “adopt a PR”.

## 16. Quick decision guide

- You have an issue but no spec yet: label `looper:plan` + assign → planner discovery
- You have a PR that needs review: request review from the intended forge user → reviewer discovery (`looper:spec-reviewing` only marks a spec PR's phase)
- A PR already has review comments to address: fixer discovery (or create it directly with `POST /api/v1/loops`)
- The spec is ready and implementation should begin: keep `looper:spec-ready` on the spec PR, then add `looper:worker-ready` to the linked assigned issue → worker discovery
- You want a human agent to drive one PR live until merge: [`pr-takeover` skill](../skills/pr-takeover/SKILL.md)
- You need to pause/stop/retry a running loop: surviving CLI verbs above + dashboard

## 17. Authentication

Looper uses `gh` for GitHub access, so `gh auth status` should succeed before planner / reviewer / fixer / worker workflows run under `looperd`.

If the daemon is configured with `server.authMode=local-token`, CLI requests need a matching token. `LOOPER_TOKEN` overrides `server.localToken` through normal environment precedence for whichever Looper process receives it; it is never written back to the config file.

Example:

```bash
export LOOPER_TOKEN=replace-me
looper dashboard
# Open the one-shot /dashboard/?code=… URL printed above.
```

For a copy/paste-only flow without the helper, mint the same one-shot code explicitly (the response is a JSON envelope), then append its `data.code` value to `/dashboard/?code=`:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $LOOPER_TOKEN" \
  "http://127.0.0.1:17310/api/v1/dashboard/bootstrap/code"
```

Codes are short-lived and single-use. The browser exchanges the code for a token kept in session storage; the long-lived token is never placed in the URL.

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
