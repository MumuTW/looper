# Looper

[![CI](https://github.com/mumutw/looper/actions/workflows/ci.yml/badge.svg)](https://github.com/mumutw/looper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go)](go.mod)

**An autonomous AI dev team for your GitHub and Forgejo repos — plan, review, fix, and ship PRs, on a loop.**

> *"LLMs are exceptionally good at looping until they meet specific goals... Don't tell it what to do, give it success criteria and watch it go."*
> — Andrej Karpathy

Looper turns that idea into a local AI dev team. Register the repos you want it to watch; Looper picks up assigned, labeled issues and runs specialized agents — **planner → reviewer ↔ fixer → worker** — each looping against its own success criteria until the PR is ready for human merge. Your forge stays the source of truth; Looper handles the spec, review cycle, and implementation in isolated worktrees.

![Looper technical architecture](assets/looper-technical-architecture.png)

Looper ships two binaries:

- `looperd` — the background daemon that polls GitHub or Forgejo, runs loops, and manages worktrees
- `looper` — a thin CLI for onboarding (`init`, `status`, `project add|list|discover`) and loop control (`stop`, `close`, `start`, `pause`, `retry`, `takeover`, `handback`, `respond`) against a running `looperd`

## Four loops, four success criteria

Each role is an agent that keeps looping until *its* exit condition is met — no fixed step counts, just goals.

- 🧭 **Planner** — *loops until the spec is reviewable.* Reads the issue, explores the repo, drafts a spec, critiques it, and revises until the plan is concrete enough to open a spec PR. Done when the spec PR is open and labeled `looper:spec-reviewing`.
- 🔍 **Reviewer** — *loops until the PR meets the bar.* Re-reads the PR on every new commit, posts inline threads, and keeps re-reviewing as the fixer pushes changes. Done when no actionable threads remain and the review comes back clean.
- 🔧 **Fixer** — *loops until reviewer threads are handled.* Pulls open review comments, addresses them in the worktree, pushes, and waits for the reviewer's next pass. Ping-pongs with the reviewer until the PR converges. Done when every actionable thread is resolved, or replied to when human input is needed.
- 🚢 **Worker** — *loops until the PR is ready for merge.* Takes the `looper:spec-ready` spec PR, implements the spec on top of it, runs checks, and iterates on its own output. Done when checks pass and the PR is ready for human review and merge.

The loops compose: planner hands off to reviewer↔fixer, reviewer↔fixer hands off to worker, and `looperd` gates each transition on GitHub labels — so you can pause, intervene, or take over at any boundary.

## Features

- 🚢 **Start from an issue, not a prompt.** On GitHub, the internal Triager turns a clear, low-risk new or reopened issue directly into Planner work; risky or underspecified reports wait with an auditable local triage report. Label-and-assign discovery remains available as an explicit manual route. Once a spec reaches `looper:spec-ready`, implementation begins.
- 🐙 **Durable workflow state stays inspectable.** Issues, PRs, reviews, and assignees remain the shared forge workflow; local `triage.report` events are the semantic authority for internal GitHub intake, so routing labels are optional projections rather than hidden prerequisites. GitHub is fully supported; Forgejo supports planner, worker, native reviewer requests/reviews, and summary-comment compatibility flows.
- 🛰️ **Many repos, one daemon.** Register your projects once — Looper watches them together and runs loops across repos in parallel.
- 🌳 **Parallel-safe by design.** Every loop runs in its own git worktree, so agents work across issues and repos without stepping on each other.
- 🤖 **Bring your own agent.** Pluggable vendor layer (`opencode`, `claude-code`, `codex`, `cursor-cli`, `grok-build`) so you're not locked into one model or CLI.
- 🧰 **Local, inspectable, stoppable.** Daemon on your machine, thin CLI and dashboard to drive it. `looper stop` / `pause` / `retry`, plus the local dashboard — no hosted control plane.

## Quick start

### For agents

If you're an AI coding agent (Claude Code, OpenCode, Codex, Cursor, etc.) helping a user set up Looper, fetch and follow the install + configure tutorial in the bundled skill:

```
https://github.com/mumutw/looper/blob/main/skills/looper/SKILL.md
```

It contains a step-by-step flow (preflight → install both binaries → write config → start the daemon → register a repo → first loop) plus a troubleshooting matrix. Confirm destructive steps with the user before running them.

### For humans

Setup is manual: install both binaries, write a config, start `looperd`, then register projects. There is no bootstrap wizard and no managed daemon lifecycle.

```bash
# 1. CLI (macOS darwin-arm64 or Linux linux-amd64)
curl -fsSL https://raw.githubusercontent.com/mumutw/looper/main/scripts/install.sh | sh

# 2. Put the install directory on PATH for this shell. Piped through `sh` the
#    installer has no terminal to ask with, so it cannot edit your profile and
#    cannot change this shell — it installs to ~/.local/bin and prints the line
#    it would have added. Skip only if ~/.local/bin is already on PATH.
export PATH="$HOME/.local/bin:$PATH"

# 3. Daemon — same release's looperd-<target>.tar.gz onto PATH, or:
#    go build -o ~/.local/bin/looperd ./cmd/looperd

# 4. Config — writes a commented ~/.looper/config.toml, never overwrites one
looper init
#    then edit it (agent vendor, base branch); see docs/configuration.md

# 5. Run the daemon (foreground)
looperd
```

In another shell, register a local git checkout — the directory containing `.git`, not a subdirectory:

```bash
looper project add /absolute/path/to/your/local/repo
looper status                                          # config, daemon, projects
```

`looper project add` posts to the running daemon, so the project is live immediately. The dashboard at `http://127.0.0.1:17310/dashboard/` and `POST /api/v1/projects` do the same thing, and are where the options the CLI does not expose (explicit id, base branch, provider) live.

Once the daemon is healthy and forge credentials are configured (`gh auth status` for GitHub, or a configured Forgejo token environment variable), loops start from the forge itself: label an issue and assign it, and `looperd`'s discovery picks it up. The CLI controls loops the daemon already owns:

```bash
looper stop 12
looper pause 12
looper retry 12
looper takeover 12     # park an existing loop for manual worktree work
looper handback 12     # return a parked loop to the daemon
looper respond 12 "lgtm — ship it"
```

Full install detail: **[docs/installation.md](docs/installation.md)**. Label conventions and role hand-offs: **[docs/users-guide.md](docs/users-guide.md)**.

## Take over a single PR

Want to babysit *one* pull request until it merges?

**The simplest path is one prompt.** Paste this into whatever coding agent you already run (Claude Code, Codex, opencode, Gemini, …):

> Take over this PR until it merges — read https://raw.githubusercontent.com/mumutw/looper/main/skills/pr-takeover/SKILL.md and follow it.

That points the agent at the [`pr-takeover` skill](skills/pr-takeover/SKILL.md). Live mode (default) drives the PR with `gh` + `git` in your session.

> **The one-command background PR takeover is gone.** The old `looper takeover <owner>/<repo>#<pr>` — which installed the daemon, registered the repo scoped to one PR, and ran reviewer + fixer unattended — lived in the CLI that was removed ahead of the role-model rewrite, along with `scripts/takeover.sh`.

`looper takeover <selector>` still exists, but it now **parks an existing loop** so you can work in its worktree by hand; `looper handback <selector>` returns it to the daemon. It does not create loops, adopt a PR URL, or accept `--merge`.

To get unattended reviewer/fixer loops on a PR today, register the repo and let the daemon's discovery claim the PR through the normal label and review-request flow.

Requirements: `git`, an authenticated `gh`, and one supported agent CLI installed locally (the agent runs on your machine with your own credentials). Grok Build from xAI uses `agent.vendor = "grok-build"` and the `grok` executable; see [Grok Build configuration](docs/configuration.md#grok-build-xai) for daemon authentication and automation limits.

## Agent skills

Looper ships installable agent skills:

- **`looper`** — install, config, starting the daemon, registering projects, and troubleshooting guidance. Note: the skill text still mentions some pre-strip CLI verbs in places; prefer **[docs/installation.md](docs/installation.md)** and the command cheatsheet above for the current surface.
- **`pr-takeover`** — drive a single PR to merge (read review feedback → fix → resolve threads → dismiss unreasonable change requests → merge when approved and green). Prefer **live** mode (`gh` + `git` in your session); the old unattended `looper takeover <pr>` background path was removed with the CLI strip.

```bash
npx skills add ./skills/looper
npx skills add ./skills/pr-takeover
```

Or install directly from GitHub:

```bash
npx skills add https://github.com/mumutw/looper/tree/main/skills/looper
npx skills add https://github.com/mumutw/looper/tree/main/skills/pr-takeover
```

See [`skills/looper/SKILL.md`](skills/looper/SKILL.md) and [`skills/pr-takeover/SKILL.md`](skills/pr-takeover/SKILL.md) for details.

## How it works

The four loops above are the conceptual model. Here's the GitHub label state machine `looperd` actually drives:

```
issue (looper:plan, assigned)
       │
       ▼
   planner ──► spec PR (looper:spec-reviewing)
                       │
                       ▼
                reviewer ⇄ fixer
                       │  clean
                       ▼
              PR labeled looper:spec-ready
                       │
                       ▼
                    worker
                       │
                       ▼
              PR ready for human merge  🎉
```

Each role runs in its own worktree, coordinated by `looperd` and gated by labels. The planner opens the spec PR, the reviewer and fixer loop on it until it's clean, and `looper:spec-ready` is the signal that hands work to the worker — which implements on the same PR rather than opening a new one.

Looper is poll-driven by default: keep `looperd` running and forge credentials available for the loop to fire. GitHub projects still use `gh`; Forgejo projects use the configured REST provider and do not require `gh` in Forgejo-only installs. Everything runs locally — no hosted control plane required.

## Networked operation

Looper supports two project modes:

- `network.mode=off` — local-only behavior. Worker still claims `looper:worker-ready` Issues assigned to the local GitHub user, Reviewer still claims review requests for the local GitHub user, and any `looper:target:*` labels are ignored.
- `network.mode=routed` — multi-Node behavior. `loopernet` centralizes webhook ingress and event fan-out, but GitHub remains the authority for work intent.

In Routed mode:

- Coordinator, not `loopernet`, mutates GitHub for Issue admission and PR review assignment.
- `looper:worker-ready` and GitHub review requests express work intent.
- exactly one `looper:target:<node_name>` label is the exact-Node authority, and Coordinator writes it last.
- the `loopernet` Coordinator lease is only a fencing gate for mutation rights; if the lease is stale, Coordinator must stop mutating GitHub.
- polling stays enabled as drift recovery if webhook ingress or SSE wakeups are missed; it is not the primary wakeup path.

For setup, identity strategy, recovery steps, and `loopernet` deployment, see **[docs/users-guide.md](docs/users-guide.md)**, **[docs/configuration.md](docs/configuration.md)**, and **[docs/loopernet-deployment.md](docs/loopernet-deployment.md)**. The formal authority rules live in ADRs **[0007](docs/adr/0007-coordinator-admission-assignment-authority.md)** through **[0011](docs/adr/0011-coordinator-control-plane-for-routed-projects-v1.md)**.

## Command cheatsheet

This is the whole operator CLI after the strip. Every verb except `init` and `version` talks to a running `looperd` over its local HTTP API.

```bash
looper version
looper init                            # write a starter config; never overwrites
looper status                          # config, daemon reachability, projects
looper project add <path>              # register a git repository root
looper project list
looper project discover <id>           # retry post-commit worktree/PR discovery
looper stop <selector>                 # "all" stops every active run
looper close <selector>
looper start <selector>
looper pause <selector>
looper retry <selector>
looper takeover <selector>             # park an existing loop for manual work
looper handback <selector>
looper respond <selector> "<answer>"   # answer a loop waiting on a human
```

A selector is a loop sequence number (`looper stop 12`) or a loop id (`looper stop loop_1cf3`). Pull request URLs are rejected — they cannot be placed in the path the daemon parses.

Global flags, accepted before or after the verb: `--config <path>`, `--host <host>`, `--port <port>`.

There is one more command that is not for operators: `looper review submit`, which publishes a reviewer agent's pull request review. Reviewer agents reach it through a wrapper the daemon writes; run directly it has no provider credentials and fails.

**Not in the CLI.** Bootstrap, managed daemon install/start, plan/review/work, ps/logs/jump, provider/webhook/network administration, and upgrade lived in the CLI that was removed. Loop inspection is available through the dashboard and the daemon's HTTP API; install and supervise `looperd` yourself.

## Configuration

- Canonical default path: `~/.looper/config.toml`
- Supported formats: `.toml`, `.yaml`, `.yml`, `.json`
- Config source selection precedence: `--config` → `LOOPER_CONFIG` → default-path discovery
- State directory: `~/.looper`, overridable with `LOOPER_HOME` (takes precedence over `HOME`). It moves the whole set of default-derived paths together — config discovery, database, logs, and worktree roots — so a second instance never writes into the first one's state
- Provider support: legacy GitHub projects keep working through `gh`; Forgejo projects require an explicit provider, `baseUrl`, `repo`, and either `tokenEnv` (`auth=token-env`) or `teaLogin` (`auth=tea`)
- All role-specific config lives under `roles.<role>`; canonical reviewer behavior lives under `roles.reviewer.behavior.*`
- Loading legacy `~/.looper/config.json` emits one informational note per process telling users that `~/.looper/config.toml` is now the preferred default path
- `agent.vendor` is required to run loops (no default)
- If `server.authMode=local-token`, set `server.localToken` and export `LOOPER_TOKEN` for the CLI

Every field, env var, CLI flag, validation rule, and troubleshooting note lives in **[docs/configuration.md](docs/configuration.md)**.

## Development

From the repo root:

```bash
go run ./cmd/looperd
go run ./cmd/looper <args>
go build ./...
go vet ./...
go test ./...
```

Provider e2e checks:

```bash
go test ./internal/e2e/forgejocontract -count=1
go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1
```

Forgejo live sandbox e2e is local/manual only and skipped unless explicitly enabled. Use a dedicated existing sandbox repo; tests create and clean run-scoped issues, branches, PRs, labels, and comments:

```bash
LOOPER_E2E_FORGEJO=1 \
LOOPER_E2E_FORGEJO_BASE_URL=https://code.example.com \
LOOPER_E2E_FORGEJO_SANDBOX_REPO=owner/repo \
LOOPER_E2E_FORGEJO_TOKEN=$TOKEN \
go test ./internal/e2e -run '^TestForgejoSandbox' -count=1
```

GitHub live sandbox tests prefer `LOOPER_E2E_GITHUB_SANDBOX_REPO`; legacy `LOOPER_E2E_SANDBOX_REPO` remains accepted only as a compatibility alias, and conflicting values fail fast.

Build artifacts go to `dist/` and are gitignored — don't edit generated files.

## Runtime notes

- `looperd` fails fast on invalid config; runtime paths must be writable
- You install and supervise `looperd` yourself; there is no managed daemon install path
- Daemon-managed worktrees live under `~/.looper/worktrees/`, grouped by repo and project
- Worktree cleanup runs inside the daemon on the `daemon.worktreeCleanup` schedule; the CLI no longer has a `worktree cleanup` verb
- When `notifications.osascript.enabled=true`, `osascript` must resolve on startup
- Automation is poll-driven by default — keep `looperd` running and provider credentials available; GitHub projects require `gh`, while Forgejo-only installs do not
