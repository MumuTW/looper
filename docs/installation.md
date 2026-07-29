# Installation and Upgrade Guide

This document contains the detailed install, upgrade, uninstall, and source-build flows for Looper.

## Requirements

For the default supported install path:

- macOS (`darwin-arm64`) or Linux (`linux-amd64`)
- `git`
- `gh` for GitHub projects; Forgejo-only installs do not require `gh`

For source development:

- Go `1.22`
- `git`
- `gh` for GitHub projects; Forgejo-only development does not require `gh`
- `osascript` if macOS notifications stay enabled

`looperd` auto-detects tool paths from `PATH`, but startup validation fails if required tools cannot be resolved. `git` is always required. `gh` is required when any configured project uses the GitHub provider, but a Forgejo-only config starts without `gh` when the Forgejo provider and token environment variable are valid.

## Install

Looper uses Go binaries as the default supported implementation. Installing is manual: you place two binaries, write a config, and run the daemon yourself.

> **There is no managed daemon install and no setup wizard.** `looper bootstrap`, `looper daemon install|start|status|logs|restart`, and `looper upgrade` were removed along with the old CLI ahead of the role-model rewrite. Nothing installs, supervises, or upgrades `looperd` for you.

### 1. Install the CLI

Either use the install script (macOS `darwin-arm64` or Linux `linux-amd64`):

```bash
curl -fsSL https://raw.githubusercontent.com/mumutw/looper/main/scripts/install.sh | sh
```

Or place it yourself:

1. Download the matching `looper-<target>.tar.gz` release artifact from GitHub Releases.
2. Extract it and rename the binary to `looper` if needed.
3. Place it on your `PATH`, for example `/usr/local/bin/looper` or `~/.local/bin/looper`.

### 2. Install the daemon

The install script only places `looper`. Install `looperd` the same way, from the matching `looperd-<target>.tar.gz` release artifact:

```bash
tar -xzf looperd-darwin-arm64.tar.gz
mv looperd-darwin-arm64 ~/.local/bin/looperd
chmod 0755 ~/.local/bin/looperd
```

Release binaries are unsigned. If macOS Gatekeeper blocks the first launch, allow the binary manually in System Settings. From a source checkout, `go build -o ~/.local/bin/looperd ./cmd/looperd` works instead.

### 3. Write a config

Run `looper init` to write a commented `~/.looper/config.toml` and then edit it. `init` refuses to overwrite an existing config and prints the path it chose, so it is safe to run on a machine that may already be set up; `looper init --config <file>.toml` writes somewhere else. Writing the file by hand is equally fine (or set `LOOPER_CONFIG` / pass `looperd --config`). A minimal starting point:

```toml
[server]
host = "127.0.0.1"
port = 17310

[agent]
# One of: claude-code, codex, opencode, cursor-cli, grok-build
vendor = "claude-code"

[defaults]
baseBranch = "main"

# Optional: import projects at daemon startup. Prefer the dashboard or
# POST /api/v1/projects for projects you want to manage without restart.
# [[projects]]
# id = "my-app"
# repoPath = "/absolute/path/to/repo"
```

Every field and validation rule lives in [configuration.md](configuration.md). `agent.vendor` is required to run loops.

### 4. Run the daemon

`looperd` runs in the foreground and stays attached to your terminal:

```bash
looperd
```

Keep it running — every `looper` control verb talks to it. Nothing restarts it after a crash, logout, or reboot; if you want supervision, wrap it in `launchd`, `systemd`, `tmux`, or whatever your machine already uses. `looperd --config <path>` selects a non-default config.

### 5. Register projects

With the daemon up, register a local git repository root either:

- with `looper project add /absolute/path/to/repo`, then `looper project list` to confirm, or
- through the local operator dashboard (served by `looperd` under `/dashboard/`), or
- with `POST /api/v1/projects` and a JSON body like `{"repoPath":"/absolute/path/to/repo"}`.

`looper project add` is the API call with the mistakes checked first. It asks the client machine's `git` for the repository root and refuses anything that is not one — a subdirectory, a broken or empty `.git`, a bare repository — and it refuses both a checkout that is already registered and a directory name that would derive an existing project's id (`/work/acme/api` after `/work/other/api`). The daemon normalizes and checks the derived id atomically, so concurrent adds cannot rebind the first project. Setting an explicit id, name, base branch, worktree root, or provider is available on the API and the dashboard, not on the CLI; an explicit id is the way past a derived-id collision.

On a repository with many open pull requests the call can take minutes: the daemon records the project and then discovers its worktrees and pull requests. If it times out anyway, run `looper project list` before retrying — the project is recorded before discovery starts.

Do not use `looper project add` for Forgejo or Plane projects. Those need a provider binding the CLI cannot express, so they belong in `[[projects]]` in the config file. Registering one through the API first and adding it to the config afterwards makes `looperd` fail to start, because a configured project cannot take over an id an API-managed record already holds.

Projects registered through the API take effect immediately. Projects listed under `[[projects]]` in the config file are imported at daemon startup instead.

## Verify the install

In another shell, confirm the daemon answers:

```bash
curl -sS "http://127.0.0.1:17310/api/v1/healthz"   # liveness (storage up)
curl -sS "http://127.0.0.1:17310/api/v1/status"    # ops readiness (admission, review publish, quarantine debt)
# or open the dashboard URL printed in the looperd logs / your browser on that host:port
```

`healthz` only means the process and storage are up. Use `/status` (or `looper status`) when you care whether reviewer publishing is enabled and whether quarantined orphan runs are still outstanding.

Then exercise a control verb against a known loop once one exists:

```bash
looper version
looper stop <selector>   # fails loudly if looperd is down or the loop is unknown
```

## Upgrade

Manual: replace the binaries. Download the newer `looper-<target>.tar.gz` and `looperd-<target>.tar.gz` release artifacts (or re-run the install script for the CLI), put them back on your `PATH`, and restart `looperd`. There is no self-upgrade, version check, rollback, or channel switching.

## Compatibility and version policy

- CLI and daemon are published from the same git tag and should normally share the same version
- short-lived version skew is allowed while the HTTP API remains compatible
- management endpoints stay under `/api/v1/*`
- `looper version` prints the CLI's own version; the daemon reports its version at `GET /api/v1/version` and via `looperd --version`
- release builds are tag-driven (`vX.Y.Z` / `vX.Y.Z-rc.N`); local default builds use `0.0.0-dev`

## Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/mumutw/looper/main/scripts/uninstall.sh | sh
```

The uninstall script removes the CLI binary, any daemon binary under `~/.looper/bin/`, and updater state. It asks before deleting config, the SQLite DB, backups, logs, and worktrees. A `looperd` you installed elsewhere on your `PATH` has to be removed by hand.

## From source

Clone the repo:

```bash
git clone https://github.com/mumutw/looper.git
cd looper
```

Then build or run the Go binaries:

```bash
go build -o looper ./cmd/looper
go build -o looperd ./cmd/looperd
go run ./cmd/looperd
```

In another shell, run the CLI from source:

```bash
go run ./cmd/looper version
go run ./cmd/looper stop 12
```
