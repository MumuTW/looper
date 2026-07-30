# Daemon reference (stripped install)

## Binary

- Entry: `looperd` / `go run ./cmd/looperd`
- Default config discovery: `~/.looper/config.toml` (also yaml/json; see configuration docs)
- Override: `looperd --config <path>` or `LOOPER_CONFIG`

## Start

```bash
looperd
```

Foreground and unsupervised by default. `looperd service install` creates a
previously absent launchd agent (macOS) or systemd user unit (Linux) generated
from `daemon.*`; it requires `daemon.mode` to be `launchd` or `systemd` first,
and `looperd service print` shows the unit without writing it. It refuses to
overwrite an existing unit; inspect it, then explicitly uninstall before
replacing it. A per-user service starts at login, not at boot: for an unattended
machine also enable automatic login (macOS) or `loginctl enable-linger` (Linux).

Managed path `~/.looper/bin/looperd` may still exist from older installs; new installs place `looperd` wherever you put it on `PATH`.

## Upgrading a running daemon

Never build or copy over the binary a running `looperd` was launched from. The daemon keeps executing the image it already loaded, so nothing fails at that moment — but the build an operator chose has been replaced, and the next restart drops every in-flight agent run onto a binary nobody selected.

`scripts/update-daemon.sh` is the supported path: it builds from a git ref and **stages** the result next to the installed binary. `--promote` installs it, and refuses while any process is executing the target. Stop the daemon first; restarting is always the operator's call because it interrupts in-flight runs.

A daemon that detects the swap after the fact reports it: `looper status` prints a `binary:` line and the `daemon_binary_swapped` degraded reason, and the daemon logs `daemon executable changed underneath the running daemon`.

## Health

```bash
curl -sS "http://127.0.0.1:17310/api/v1/healthz"
curl -sS "http://127.0.0.1:17310/api/v1/status"
curl -sS "http://127.0.0.1:17310/api/v1/version"
```

`GET /api/v1/healthz` is **liveness** only (process up + storage/migration check). It can still be HTTP 200 when reviewer publishing is fail-closed or quarantined orphan runs remain.

`GET /api/v1/status` (and the ops lines on `looper status`) is the **readiness / ops** surface: admission state, `tools.looperPath` + review-publish capability, and live `service.recovery.outstanding` quarantine/orphan debt beyond the one-shot startup recovery snapshot.

Dashboard: run `looper dashboard` and open the URL it prints. When `server.authMode=local-token`, the command authenticates with the selected config token or `LOOPER_TOKEN` and prints a short-lived one-shot login URL; it never puts the long-lived token in the URL.

## Runtime layout

- Config: `~/.looper/config.toml`
- DB / logs / worktrees: under `~/.looper/` by default
- Worktree cleanup is daemon-scheduled (`daemon.worktreeCleanup`); no CLI `worktree cleanup` verb

## Fail-fast startup

`looperd` exits non-zero on invalid config, missing required tools (`git`, and `gh` when any project needs GitHub), or unwritable runtime paths.
