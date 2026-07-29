---
name: looper
description: Use when installing, configuring, starting, verifying, operating, or troubleshooting Looper, looperd, the looper CLI, ~/.looper config, or runtime paths; when setting up Looper with opencode, claude-code, codex, cursor-cli, or Grok Build; when registering repos or configuring planner/reviewer/fixer/worker loops; or when diagnosing daemon reachability, git, gh, LOOPER_TOKEN, writable path, or startup issues.
---

# Looper

Use this skill when an agent needs to install, configure, start, check, operate, or troubleshoot Looper (`looper` CLI, `looperd` daemon, or files under `~/.looper`).

> **The CLI is tiny, and installation is manual.** The whole operator surface is:
>
> `looper start|pause|retry|stop|close|takeover|handback|respond|version`
>
> plus machine-only `looper review submit` (never suggest it to a user). Global flags (`--config`, `--host`, `--port`) work before or after the verb. A **selector** is a loop sequence number or a loop id — never a pull request URL.
>
> There is **no** `bootstrap`, `init`, `status`, `project add|list`, `daemon install|start|…`, `upgrade`, `config show`, `webhook`/`provider`/`network` administration, or `ps`/`logs`/`jump`/`plan`/`review`/`work`. Those lived in the CLI removed ahead of the role-model rewrite. Do anything else by editing the config file, through the dashboard (`/dashboard/`), through the daemon's HTTP API, or with the user's process manager — never invent a `looper` verb.

Webhook mode is configured in the config file and observed at `GET /api/v1/webhook/status` or on the dashboard. Stale GitHub CLI forwarder hooks have to be removed with `gh api` by hand after the user confirms.

`looperd` watches its selected config while running. Curated hot-safe policy changes (including `agent.vendor`) apply to claims made after publication without restarting; active runs keep the snapshot they started with. Use the Configuration page at `/dashboard/config` for field-level edits. Read [`references/config.md`](references/config.md) before deciding a restart is required.

**When NOT to use this skill:** developing on the Looper codebase itself (`cmd/`, `internal/`, `pkg/`). Follow `AGENTS.md` and standard Go tooling.

## Looper in one paragraph

Looper is a local daemon (`looperd`) that polls GitHub/Forgejo and runs agent loops in their own git worktrees, gated by forge labels:

| Role | Default discovery | Hands off via |
| --- | --- | --- |
| 🧭 **Planner** | Open issues with `looper:plan`, assigned to current user | Spec PR labeled `looper:spec-reviewing` |
| 🔍 **Reviewer** | PRs where current user is review-requested, plus `looper:spec-reviewing` | Clean review promotes to `looper:spec-ready` |
| 🔧 **Fixer** | Open non-draft PRs by current user with actionable threads | Pushes fixes; reviewer re-runs |
| 🚢 **Worker** | Issues with `looper:worker-ready` (assigned), or PRs labeled `looper:spec-ready` | Implements until checks pass |

Creating a loop out of band is **not** a CLI capability: use the dashboard or `POST /api/v1/planners`, `POST /api/v1/workers`, `POST /api/v1/loops`. `looper start <selector>` only starts a loop the daemon already knows about.

## Install and configuration

Manual sequence — confirm destructive steps with the user.

### Step 0 — Preflight

Supported hosts: macOS `darwin-arm64`, Linux `linux-amd64`.

```bash
command -v git
command -v gh
gh auth status
```

Install missing tools only with the user's OK. Authenticate `gh` if needed.

### Step 1 — Detect agent vendors

| `agent.vendor` | Detect with |
| --- | --- |
| `claude-code` | `command -v claude` |
| `codex` | `command -v codex` |
| `opencode` | `command -v opencode` |
| `cursor-cli` | `command -v cursor-agent` or `command -v agent` |
| `grok-build` | `command -v grok` |

If none are installed, stop and ask which the user wants.

### Step 2 — Install both binaries

```bash
curl -fsSL https://raw.githubusercontent.com/mumutw/looper/main/scripts/install.sh | sh
```

That only places `looper`. Also install `looperd` from the same release's `looperd-<target>.tar.gz`, or:

```bash
go build -o ~/.local/bin/looperd ./cmd/looperd
```

### Step 3 — Write config

Canonical path: `~/.looper/config.toml`. Never overwrite an existing file without the user's OK.

```toml
[server]
host = "127.0.0.1"
port = 17310

[agent]
vendor = "claude-code"   # or codex / opencode / cursor-cli / grok-build

[defaults]
baseBranch = "main"
```

For full schema see [`references/config.md`](references/config.md) and [docs/configuration.md](../../docs/configuration.md).

### Step 4 — Start the daemon

```bash
looperd
```

Foreground only; nothing supervises it. Keep it running. Health check:

```bash
curl -sS "http://127.0.0.1:17310/api/v1/healthz"
```

### Step 5 — Register a project

With the daemon up, either open the dashboard at `http://127.0.0.1:17310/dashboard/`, or:

```bash
curl -sS -X POST "http://127.0.0.1:17310/api/v1/projects" \
  -H 'Content-Type: application/json' \
  -d '{"repoPath":"/absolute/path/to/repo"}'
```

`repoPath` must be a git repository root (contains `.git`). For Forgejo/Plane providers, put the provider + project binding in the config file (see [docs/configuration.md](../../docs/configuration.md) and [docs/plane-provider.md](../../docs/plane-provider.md)); the project API will not rewrite file-managed projects.

### Step 6 — Verify

```bash
curl -sS "http://127.0.0.1:17310/api/v1/status"
curl -sS "http://127.0.0.1:17310/api/v1/projects"
looper version
```

Loops start from forge labels/assignments once projects and credentials are good — there is no `looper plan` / `review` / `work`.

## Operating loops

```bash
looper stop <selector>              # "all" stops every active run
looper close <selector>
looper start <selector>
looper pause <selector>
looper retry <selector>
looper takeover <selector>          # park an existing loop for manual worktree work
looper handback <selector>
looper respond <selector> "<answer>"
```

Inspect loops in the dashboard or via `GET /api/v1/loops`. Worktree path for a dirty retry is shown in the dashboard (copyable `cd -- '…'`); there is no `looper jump`.

## Troubleshooting matrix

| Symptom | Check |
| --- | --- |
| CLI can't reach daemon | Is `looperd` running? Correct `--config`/`--host`/`--port`? `curl` healthz |
| Unknown command | Verb not in the list above — do not invent; use config/dashboard/API |
| PR URL rejected as selector | Expected — use loop seq or loop id only |
| Reviewer can't publish | Daemon must spawn the trusted `looper review submit` wrapper; never run it by hand without the proxy |
| Daemon fails on startup | Config validation, missing `git`/`gh`, or unwritable `~/.looper` |
| Want daemon across reboot | User's `launchd`/`systemd`/`tmux` — not a Looper feature |

## Anti-patterns

- Reaching for `looper bootstrap`, `daemon start`, `project add`, `ps`, `logs`, `plan`, `review`, `work`, `jump`
- Telling a user to run `looper review submit`
- Using a PR URL as a selector
- Force-pushing or inventing flags the strip CLI does not have

See also: [`references/cli.md`](references/cli.md), [`references/daemon.md`](references/daemon.md), [`references/config.md`](references/config.md).
