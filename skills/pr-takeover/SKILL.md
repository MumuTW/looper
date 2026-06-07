---
name: pr-takeover
description: Use when asked to take over, adopt, or babysit a GitHub pull request until it merges — continuously read review feedback, fix it, push, reply to and resolve review threads, dismiss unreasonable change requests with a written reason, and merge once the PR is approved and all required checks pass. Triggers on "take over this PR", "接管这个 PR", "持续修复 review comment 直到合并", or a Looper takeover bot comment. Runs in your own coding agent session (Claude Code, Cursor, Codex, opencode) using gh + git — no daemon, no install.
---

# PR Takeover

Drive a single pull request to merge **autonomously**, from inside your own coding-agent session. You read the live review state, fix what reviewers ask for, resolve the threads, dismiss change requests you can justify as incorrect, and merge once the PR is approved and green — looping until it lands.

This is the **attended / zero-install** path: it uses the agent the user is already running (so GitHub and model credentials are already valid) and needs only `gh` and `git`. For **unattended / background** takeover that survives the user closing their terminal, point them at `looper takeover` instead (see [References](references/github-commands.md#unattended-alternative)).

## When to use

- A PR author wants their agent to shepherd a PR through review → fixes → merge.
- A Looper bot left a comment recommending takeover.
- The user says "take over / 接管 / babysit this PR until it merges".

**Do not** silently start an endless loop on a repo you were not asked to act on. Confirm the target PR first.

## Prerequisites (check, then proceed)

```bash
gh auth status                       # must be authenticated
git rev-parse --show-toplevel        # must run inside the repo checkout
```

- `gh` authenticated as an account with **push access to the PR branch** (the PR author, or a collaborator).
- Merge requires permission to merge into the base repo; dismissing reviews requires write access.
- Run from inside the repository checkout, or pass an explicit `<owner>/<repo>#<number>`.

## Identify the PR

```bash
# explicit ref wins; otherwise resolve the current branch's PR
gh pr view --json number,headRefName,baseRefName,url --jq '{number,head:.headRefName,base:.baseRefName,url}'
```

If no PR exists for the branch, stop and tell the user to open one (or push the branch first).

## The takeover loop

Repeat until the PR is **merged** or a **hard blocker** needs a human. One iteration:

### 1. Snapshot the live state — never act on stale data

```bash
gh pr view <num> --json state,isDraft,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup,headRefOid
```

Also fetch **unresolved review threads** (GraphQL — see [references](references/github-commands.md#list-review-threads)). Work only from what GitHub returns *now*; if a fetch fails (network), retry — do not proceed on assumptions.

Decide the iteration's mode from `state` / `reviewDecision` / checks:
- `state == MERGED` → **done**, report and stop.
- `state == CLOSED` → stop, tell the user.
- unresolved actionable threads exist → go to **2 (fix)**.
- no actionable threads, `reviewDecision == APPROVED`, all required checks green, `mergeable == MERGEABLE` → go to **4 (merge)**.
- otherwise (waiting on CI, waiting on a re-review) → go to **5 (wait)**.

### 2. Address every actionable unresolved thread

For each unresolved thread that asks for a real change:
- Make the change in the worktree. Keep edits **scoped to what the thread asks for** — do not opportunistically rewrite unrelated code.
- Run the repo's tests/linters if they're quick and obvious.
- Commit with a clear subject (`fix: <what>`), then push to the PR branch.

```bash
git add -A && git commit -m "fix: <addresses review comment>"
git push
```

Batch related fixes into sensible commits; one commit per thread is fine but not required.

### 3. Reply, then resolve or dismiss

For each thread you addressed: **reply** with a one-line note on how you fixed it, then **resolve** the thread (GraphQL `resolveReviewThread` — see [references](references/github-commands.md#resolve-a-thread)).

For a change request you judge **incorrect or unreasonable** (factually wrong, out of scope, or contradicting the agreed design): **reply with your reasoning first**, then **dismiss** that review (GraphQL `dismissPullRequestReview` — see [references](references/github-commands.md#dismiss-a-review)). The dismissal `message` is mandatory and must state *why*. Never dismiss silently, and never dismiss a comment that raises a legitimate blocking concern just to get to green.

### 4. Merge when approved and green

Only when **all** hold: `reviewDecision == APPROVED`, every required check is `SUCCESS` (none `PENDING`/`FAILURE`), `mergeable == MERGEABLE`, and the PR is not a draft:

```bash
gh pr merge <num> --squash --delete-branch
# or, to let GitHub merge automatically as soon as requirements are met:
gh pr merge <num> --squash --auto
```

Use the repo's conventional merge method if it differs (`--merge` / `--rebase`). Then report and stop.

### 5. Wait, then re-loop

If you're waiting on CI or a fresh review, poll on an interval rather than spinning:
- **Claude Code**: use the `/loop` skill (`/loop 5m <this instruction>`) or schedule a wake-up; between ticks, stop consuming tokens.
- **Cursor / Codex / others**: re-run this instruction on a timer, or iterate in-session with a sleep between checks.

Re-request review after pushing fixes if the reviewer was dismissed or the PR fell out of `REVIEW_REQUIRED`:

```bash
gh pr edit <num> --add-reviewer <login>   # if a specific reviewer should re-review
```

## Safety rails (apply even in full-auto)

These keep autonomous runs from doing damage — they are not optional:

- **Never merge with a failing or pending required check**, ever. Green-and-approved is the only merge gate.
- **Never force-push**, never rewrite or drop other people's commits, never delete the base branch.
- **Dismiss only with a written justification**; if you can't articulate why a change request is wrong, treat it as valid and address it.
- **Stop and ask a human** when: a reviewer explicitly says "do not merge" / "hold" / applies a hold label; the fix needs a product/design decision you can't infer; the same thread reopens after 2 fix attempts (you're not converging); or a merge conflict needs non-trivial judgement.
- **Cap no-progress iterations**: if N consecutive loops produce no new commit and no state change, stop and summarize what's blocking.
- Keep a short running log of what you changed and why, so the human can audit the session.

## Exact commands

All `gh` / GraphQL recipes (list threads, resolve, dismiss, check status, merge, re-request) are in **[references/github-commands.md](references/github-commands.md)**.

## Copy-paste prompt (for a bot to post under a PR)

> Take over this PR until it merges. Continuously: read all review comments, fix what they ask for and push, reply to and resolve each thread, dismiss any change request you can justify as incorrect (with a written reason), and merge once the PR is approved and all required checks pass. Don't merge on red checks, don't force-push, and stop to ask me if a human says hold or a fix needs a product decision.

For Claude Code, prefix with `/loop 5m` to make it poll until merged.
