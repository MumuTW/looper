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

After a clean preflight, create an explicit rollback bundle (daemon-owned SQLite online backup + config + matching binaries + checksums):

```bash
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

Stage a matching CLI/daemon pair, then activate via an atomic release pointer.
`stage-release` computes the release id from the target build identity (it does
not accept an operator-supplied id). `activate-release` and `verify-start`
require the staged release id returned by stage.

```bash
looper upgrade stage-release --release-root <dir> --target-looper <path> --target-looperd <path>
looper upgrade activate-release --release-root <dir> --release <id>
looper upgrade verify-start --release-root <dir> --release <id>
```

`verify-start` must succeed before declaring cutover success. It checks build
identity, the release pointer, admission readiness, storage health, and
quarantine debt. It does **not** fail merely because the restarted daemon has
already claimed queued work (drain leaves the queue intact). `package.autoUpgradeEnabled`
is not a supported managed upgrade path (legacy decode only).

Supported service layouts must launch `looperd` through the activated
`release-root/current` pointer. `looperd service install` rewrites a binary
path under `releases/<id>/` to `current/looperd` automatically when that
pointer exists, so `activate-release` switches the next supervised start
without rewriting unit files. Activation returns `serviceExecutable` with that
path; restart the supervised unit after activate so the new image is loaded.

Also point `tools.looperPath` at `release-root/current/looper` as part of
cutover (and restore the prior value on rollback). Agents publish reviews
through that path; leaving it on a pre-cutover binary breaks the paired release.


### Rollback restore

When post-start verification fails, **stop the candidate daemon first** (it still
holds SQLite open). Then restore the matching pre-cutover config and SQLite
snapshot from a v2 backup bundle (fail-closed if targets are still open):

```bash
# stop supervised looperd (launchctl unload / systemctl --user stop looperd)
looper upgrade restore-preflight --bundle <directory>
looper upgrade restore --bundle <directory> --confirm
```

Supported sequence:

1. `preflight` (read-only compatibility; sandbox probe uses `daemon.workingDirectory`)
2. **Ensure the live pair is staged under `release-root`** as the prior release:
   - **First cutover:** `stage-release` the running CLI/daemon, then `activate-release` that id so `current` exists. Reinstall/update the service so the unit launches `release-root/current/looperd` (not a concrete old path). Set `tools.looperPath` to `release-root/current/looper` and restart once to confirm the service follows `current`. Keep that release id as `previous`.
   - **Later cutovers:** the live pair is already that release id (last successful candidate). Re-run `stage-release` with the same binaries — staging is **idempotent** when the destination already matches (same build + binary hashes). Or skip re-staging and reuse `looper upgrade` / `CurrentReleaseID` as `previous`.
3. `backup` (initial matching config + SQLite + binary evidence; keep the path)
4. `drain` until `drained: true`
5. **`backup` again while drained (mandatory)** — this is the rollback bundle for cutover. Work may still commit between step 3 and drain completion; only the post-drain bundle is guaranteed to include that work. Record this bundle directory as `ROLLBACK_BUNDLE`.
6. `stage-release` the **candidate** pair (new release id)
7. `activate-release` the candidate (switches `current` only — does not restart)
8. Point `tools.looperPath` at `release-root/current/looper` if not already
9. **Restart the supervised daemon with `LOOPER_UPGRADE_VERIFY_HOLD=1`** so it loads `current/looperd` but **keeps admission drained** (no claim/mutation) until verification finishes. Without this hold, the replacement scheduler can complete work before `verify-start`; a later failed verify + restore of `$ROLLBACK_BUNDLE` would discard those writes.
10. `verify-start` (accepts `admissionState` ready **or** draining under the hold)
11. On success: **restart again without `LOOPER_UPGRADE_VERIFY_HOLD`** so admission opens and normal work resumes

On failure after restart (while still held/drained):

1. **Stop** the candidate daemon (required — restore fails closed while SQLite is open)
2. `restore-preflight --bundle $ROLLBACK_BUNDLE` → `restore --bundle $ROLLBACK_BUNDLE --confirm` (use the **post-drain** bundle, not the pre-drain one)
3. `activate-release` the **prior** release id
4. Restore prior `tools.looperPath` if you changed it
5. Restart the supervised daemon **without** `LOOPER_UPGRADE_VERIFY_HOLD`

Backup-copied binaries are evidence, not an executable release; binary rollback is always via a previously staged release under the same `release-root`.


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
