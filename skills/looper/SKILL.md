---
name: looper
description: Use when installing, configuring, starting, verifying, operating, or troubleshooting Looper, looperd, the looper CLI, ~/.looper config, or runtime paths; when setting up Looper with opencode, claude-code, codex, cursor-cli, Grok Build, or Devin; when registering repos or configuring planner/reviewer/fixer/worker loops; or when diagnosing daemon reachability, git, gh, LOOPER_TOKEN, writable path, or startup issues.
---

# Looper

Use this skill when an agent needs to install, configure, start, check, operate, or troubleshoot Looper (`looper` CLI, `looperd` daemon, or files under `~/.looper`).

> **The CLI is tiny, and installation is manual.** The whole operator surface is:
>
> `looper init|status|project add|project list|project discover|start|pause|retry|stop|close|takeover|handback|respond|version`
>
> plus machine-only `looper review submit` (never suggest it to a user). Global flags (`--config`, `--host`, `--port`) work before or after the verb. A **selector** is a loop sequence number or a loop id — never a pull request URL.
>
> There is **no** `bootstrap`, `daemon install|start|…`, `upgrade`, `config show`, `webhook`/`provider`/`network` administration, or `ps`/`logs`/`jump`/`plan`/`review`/`work`. Those lived in the CLI removed ahead of the role-model rewrite. Do anything else by editing the config file, through the dashboard (`/dashboard/`), through the daemon's HTTP API, or with the user's process manager — never invent a `looper` verb.

Webhook mode is configured in the config file and observed at `GET /api/v1/webhook/status` or on the dashboard. Stale GitHub CLI forwarder hooks have to be removed with `gh api` by hand after the user confirms.

`looperd` watches its selected config while running. Curated hot-safe policy changes (including `agent.vendor`) apply to claims made after publication without restarting; active runs keep the snapshot they started with. Use the Configuration page at `/dashboard/config` for field-level edits. Read [`references/config.md`](references/config.md) before deciding a restart is required.

**When NOT to use this skill:** developing on the Looper codebase itself (`cmd/`, `internal/`, `pkg/`). Follow `AGENTS.md` and standard Go tooling.

## Looper in one paragraph

Looper is a local daemon (`looperd`) that polls GitHub and runs agent loops in their own git worktrees, gated by forge labels:

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
| `devin-experimental` | `command -v devin` |

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

Canonical path: `~/.looper/config.toml`. `looper init` writes a commented starter file there and refuses to overwrite an existing one, so it is safe to run first and read the path it prints. Never overwrite an existing file without the user's OK.

```toml
[server]
host = "127.0.0.1"
port = 17310

[agent]
vendor = "claude-code"   # or codex / opencode / cursor-cli / grok-build
# devin-experimental is available for Devin fresh-run tasks.

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
curl -sS "http://127.0.0.1:17310/api/v1/healthz"  # liveness
curl -sS "http://127.0.0.1:17310/api/v1/status"   # ops readiness (review publish, quarantine debt)
looper status
```

### Step 5 — Register a project

**First decide which of the two registration paths this project uses — they do not mix.**

**Projects that need an explicit provider binding: do NOT run `looper project add`.** Define the project entirely in the config file under `[[projects]]`, alongside its provider, and restart the daemon. `looper project add` has no way to express a provider binding, and the record it creates is marked `source = "api"`. Adding the same project to the config afterwards does not convert it: on the next start `SyncConfigured` sees a configured project whose id already belongs to an API record, fails with `configured project <id> conflicts with an API-managed project`, and **`looperd` refuses to start**. Recovering means removing the API record before the daemon will come back up. See [docs/configuration.md](../../docs/configuration.md).

**GitHub projects, with the daemon up:**

```bash
looper project add /absolute/path/to/repo
looper project list
```

The path must be a git repository **root** — `looper project add` asks the client machine's `git` (`rev-parse --show-toplevel`) and refuses a subdirectory, a directory with a broken or empty `.git`, or a bare repository, naming the real root when it finds one. It also refuses a checkout already registered. The daemon normalizes and checks a path-only request's derived id atomically, so a directory name that would reuse an active project id (`/work/acme/api` after `/work/other/api`) is rejected even when adds race. Always pass an absolute path, and confirm it with the user rather than guessing.

The dashboard at `http://127.0.0.1:17310/dashboard/` and `POST /api/v1/projects` register the same way, and are where the fields the CLI does not expose (explicit id, name, base branch, worktree root, provider) live — use them when you need an explicit id to sidestep a derived-id collision.

`project add` returns as soon as the project is validated, committed, and published — even on a repository with many open pull requests. Worktree/PR discovery runs as post-commit work in the daemon and is reported as pending; its status lives on the project record, and a failed discovery is retried with `looper project discover <id>` (or `POST /api/v1/projects/{id}/discover`), never by re-registering.

### Step 6 — Verify

```bash
looper status   # config file, daemon reachability, registered projects
looper version
curl -sS "http://127.0.0.1:17310/api/v1/status"
```

`looper status` exits non-zero when the config does not load or the daemon is unreachable, and names the config file it selected either way — that is the fastest way to tell "wrong config" from "daemon down".

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
| `configured project X conflicts with an API-managed project` | X was registered with `looper project add` and then added to `[[projects]]`. Pick one: drop it from the config, or remove the API record. Config-managed projects must never be `project add`ed |
| Want daemon across reboot | User's `launchd`/`systemd`/`tmux` — not a Looper feature |

## Anti-patterns

- Reaching for `looper bootstrap`, `daemon start`, `ps`, `logs`, `plan`, `review`, `work`, `jump`
- Passing `looper project add` a subdirectory instead of the repository root, or expecting it to take an id / base branch / provider — those are API and dashboard fields
- Running `looper project add` for a project with an explicit provider binding, then adding it to the config file — that combination stops `looperd` from starting
- Telling a user to run `looper review submit`
- Using a PR URL as a selector
- Force-pushing or inventing flags the strip CLI does not have

See also: [`references/cli.md`](references/cli.md), [`references/daemon.md`](references/daemon.md), [`references/config.md`](references/config.md).
