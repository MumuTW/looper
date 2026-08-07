# Installation and Upgrade Guide

This document contains the detailed install, upgrade, uninstall, and source-build flows for Looper.

## Requirements

For the default supported install path:

- macOS (`darwin-arm64`) or Linux (`linux-amd64`)
- `git`
- `gh` for GitHub projects

For source development:

- Go `1.22`
- `git`
- `gh` for GitHub projects
- `osascript` if macOS notifications stay enabled

`looperd` auto-detects tool paths from `PATH`, but startup validation fails if required tools cannot be resolved. `git` is always required. `gh` is required when any configured project uses the GitHub provider.

## Install

Looper uses Go binaries as the default supported implementation. Installing is manual: you place two binaries, write a config, and run the daemon yourself.

> **There is no managed daemon install and no setup wizard.** `looper bootstrap`, `looper daemon install|start|status|logs|restart`, remain out of the managed-install path. Controlled upgrade starts with read-only `looper upgrade preflight`; nothing silently self-upgrades `looperd` for you.

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
# One of: claude-code, codex, opencode, cursor-cli, grok-build, devin-experimental
# devin-experimental is fresh-run only.
vendor = "claude-code"

[defaults]
baseBranch = "main"

# Optional: import projects at daemon startup. Prefer `looper project add` or
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

Keep it running — every `looper` control verb talks to it. In the foreground nothing restarts it after a crash or a reboot. `looperd --config <path>` selects a non-default config.

To have the machine supervise it instead, set `daemon.mode` (`launchd` on macOS, `systemd` on Linux) and pin `tools.gitPath` and `tools.ghPath` in your config, then:

```bash
looperd service install
```

`looperd service print` shows the exact unit first without writing anything. `status` reports whether it is installed, and `uninstall` removes it — both address the canonical location and read no configuration, so they work even when the config does not load.

Installing refuses rather than guessing:

- **`daemon.environment` is refused.** The unit carries no environment, so anything the daemon needs belongs in the configuration file.
- **`daemon.plistPath` is refused.** The unit always goes to the canonical per-user location, so activation, status, and uninstall address the same thing.
- **Auto-detected tool paths are refused.** A supervisor starts the daemon with a minimal `PATH`, so a `git` or `gh` found through your shell would be searched for again and may resolve differently.
- **An existing unit is refused.** Replacing one is `uninstall` then `install`, so no active service is silently left on an old definition.

**A per-user service is not the same as always-on.** On macOS a LaunchAgent runs only while the user is logged in, so an unattended machine also needs automatic login. On Linux a systemd user unit behaves the same unless you run `loginctl enable-linger $USER`; automatic login is not required there.

### 5. Register projects

With the daemon up, register a local git repository root either:

- with `looper project add /absolute/path/to/repo`, then `looper project list` to confirm, or
- with `POST /api/v1/projects` and a JSON body like `{"repoPath":"/absolute/path/to/repo"}`.

`looper project add` is the API call with the mistakes checked first. It asks the client machine's `git` for the repository root and refuses anything that is not one — a subdirectory, a broken or empty `.git`, a bare repository — and it refuses both a checkout that is already registered and a directory name that would derive an existing project's id (`/work/acme/api` after `/work/other/api`). The daemon normalizes and checks the derived id atomically, so concurrent adds cannot rebind the first project. Setting an explicit id, name, base branch, or worktree root is available on the API, not on the CLI. The dashboard only lists projects and opens their filtered loops. A `provider` binding is not available on the CLI or API — it is file-managed, so declare it in `[[projects]]` and restart the daemon.

Registration completes as soon as the project is validated, committed, and published. Worktree and pull request discovery then runs as post-commit work in the daemon — even on a repository with many open pull requests `looper project add` returns immediately, reporting discovery as pending. Discovery status is stored on the project record; if it fails, retry it with `looper project discover <id>` (or `POST /api/v1/projects/{id}/discover`) without re-registering the project.

Do not use `looper project add` for a project that needs an explicit provider binding; the CLI and API schemas cannot express one, so those belong in `[[projects]]` in the config file. Registering one through the API first and adding it to the config afterwards makes `looperd` fail to start while the API record remains active.

To recover that mixed-ownership state without editing SQLite, temporarily remove the conflicting `[[projects]]` entry, restart `looperd`, send `DELETE /api/v1/projects/<id>`, and stop the daemon. Restore the complete config entry and restart once more. DELETE archives the API-owned record; config import is allowed to claim only that explicitly archived ID. The old project's loops are terminated and its worktree registrations are retired without touching the physical checkouts, so confirm the target ID before sending DELETE.

Projects registered through the API take effect immediately. Projects listed under `[[projects]]` in the config file are imported at daemon startup instead.

## Verify the install

In another shell, confirm the daemon answers:

```bash
curl -sS "http://127.0.0.1:17310/api/v1/healthz"   # liveness (storage up)
curl -sS "http://127.0.0.1:17310/api/v1/status"    # ops readiness (admission, review publish, quarantine debt)
looper dashboard                                      # open the URL it prints
```

With `server.authMode=local-token`, give the command the matching selected config or export `LOOPER_TOKEN`; it prints a short-lived one-shot URL and never places the long-lived token in the URL.

`healthz` only means the process and storage are up. Use `/status` (or `looper status`) when you care whether reviewer publishing is enabled and whether quarantined orphan runs are still outstanding.

Then exercise a control verb against a known loop once one exists:

```bash
looper version
looper stop <selector>   # fails loudly if looperd is down or the loop is unknown
```

## Upgrade

Before replacing binaries, run a read-only preflight against explicit candidate paths:

```bash
looper upgrade preflight --target-looper /path/to/candidate/looper --target-looperd /path/to/candidate/looperd --json
```

Preflight only calls `GET /api/v1/version` and `GET /api/v1/status` on the running daemon and executes the candidate binaries' identity (and optional `--check-config`) commands. It does not start a second production daemon or mutate the production database. Incomplete build identities never count as a matching CLI/daemon pair.

After a clean preflight, close work admission and wait for the Supervisor to report
that all owned work has drained before taking the rollback snapshot. This ordering
keeps the SQLite backup aligned with the final quiescent runtime state:

```bash
looper upgrade drain --deadline 10m
looper upgrade backup
looper upgrade verify --bundle <directory>
```

`upgrade verify` is offline and fail-closed on missing files, bad checksums, or manifest problems.

Manual cutover after a clean preflight: replace the binaries from matching release artifacts. Download the newer `looper-<target>.tar.gz` and `looperd-<target>.tar.gz` release artifacts (or re-run the install script for the CLI), put them back on your `PATH`, and restart `looperd`. There is no self-upgrade, version check, rollback, or channel switching.

## Compatibility and version policy

- CLI and daemon release artifacts are stamped from the same prepared version, git commit, channel, API version, and release timestamp
- short-lived version skew is allowed while the HTTP API remains compatible
- management endpoints stay under `/api/v1/*`
- `looper version` and `looperd --version` keep their concise semantic-version output
- `looper version --json` and `looperd --version-json` print the complete build identity; `dirty` is `null` when a source-tree probe was unavailable rather than claiming the tree was clean
- `looper version --check-daemon` compares the CLI identity with `GET /api/v1/version` and exits nonzero unless both identities are complete, clean, and every build field matches; dirty or unknown source trees cannot prove equality; add `--json` for a machine-readable `comparable` / `sameBuild` report
- release builds are tag-driven (`vX.Y.Z` / `vX.Y.Z-rc.N`); local default builds use `0.0.0-dev`

## Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/mumutw/looper/main/scripts/uninstall.sh | sh
```

The uninstall script removes the installer-owned CLI binary, any daemon binary under `$LOOPER_HOME/bin/` (default `~/.looper/bin/`), updater state, and the exact PATH stanza added by the installer to `.zprofile`, `.bash_profile`, or `.profile`. Unrelated profile content is preserved.

Before removing user data, it lists every existing path in scope and asks for approval. That optional scope is `config.toml`, `config.json`, `config.yaml`, `config.yml`, `looper.sqlite` plus its `-wal`/`-shm` sidecars, `backups/`, `logs/`, and `worktrees/` under `$LOOPER_HOME`. Declining leaves all of those paths untouched. For an explicitly authorized non-interactive uninstall, set `LOOPER_UNINSTALL_YES=1`; other values do not grant deletion authority. A `looperd` installed elsewhere on `PATH` still has to be removed by hand.

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

### Graceful drain before cutover

`looper upgrade drain --deadline <duration>` moves admission to `draining` (no new claims/mutations) and waits for in-flight supervisor work without hard-killing agents as the routine path.


### Atomic release switch

Stage a matching CLI/daemon pair, then activate via an atomic release pointer:

```bash
looper upgrade stage-release --release-root <dir> --target-looper <path> --target-looperd <path>
looper upgrade activate-release --release-root <dir> --release <id>
looper upgrade verify-start --release-root <dir> --release <id>
```

`verify-start` must succeed before declaring cutover success. `package.autoUpgradeEnabled` is not a supported managed upgrade path (legacy decode only).

### Building a dashboard-serving `looperd`

The dashboard at `/dashboard/` is a React + Vite SPA whose built assets are
`//go:embed`'d into `looperd` from `internal/dashboard/assets`. Those assets are
generated by the frontend build and are gitignored, so a plain `go build` only
embeds them when they already exist on disk.

To produce a `looperd` that serves the usable dashboard, build the dashboard
first, then build the daemon:

```bash
cd web/dashboard
pnpm install --frozen-lockfile
pnpm run build          # writes internal/dashboard/assets + the .production marker
cd ../..
go build -o looperd ./cmd/looperd
```

`scripts/verify.sh` runs the same dashboard build before the Go gates, so a
green local verify produces a dashboard-serving binary the same way CI does.
Release binaries from `.github/workflows/release.yml` always build the dashboard
before building `looperd`, so every published `looperd-<target>.tar.gz` serves
the dashboard.

**Development-mode exception.** A plain `go run ./cmd/looperd` or
`go build ./cmd/looperd` without the dashboard build step embeds only the
fallback placeholder, and `/dashboard/` renders a notice that production
dashboard assets are not embedded. The API stays healthy. This is intentional:
it keeps the Go-only dev loop (edit Go, run) free of a Node toolchain
requirement. The `internal/e2e` smoke `TestSmokeLooperdServesEmbeddedDashboard`
skips under this exception and runs only when the built assets are present, so
CI — which builds the dashboard before `go test ./...` — guards the release
embed path without forcing every local `go test` to install pnpm.
