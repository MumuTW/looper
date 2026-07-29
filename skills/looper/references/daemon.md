# Daemon reference (stripped install)

## Binary

- Entry: `looperd` / `go run ./cmd/looperd`
- Default config discovery: `~/.looper/config.toml` (also yaml/json; see configuration docs)
- Override: `looperd --config <path>` or `LOOPER_CONFIG`

## Start

```bash
looperd
```

Foreground, unsupervised. Surviving logout/reboot is the operator's `launchd` / `systemd` / `tmux` problem — Looper does not install LaunchAgents or manage lifecycle.

Managed path `~/.looper/bin/looperd` may still exist from older installs; new installs place `looperd` wherever you put it on `PATH`.

## Health

```bash
curl -sS "http://127.0.0.1:17310/api/v1/healthz"
curl -sS "http://127.0.0.1:17310/api/v1/status"
curl -sS "http://127.0.0.1:17310/api/v1/version"
```

`GET /api/v1/healthz` is **liveness** only (process up + storage/migration check). It can still be HTTP 200 when reviewer publishing is fail-closed or quarantined orphan runs remain.

`GET /api/v1/status` (and the ops lines on `looper status`) is the **readiness / ops** surface: admission state, `tools.looperPath` + review-publish capability, and live `service.recovery.outstanding` quarantine/orphan debt beyond the one-shot startup recovery snapshot.

Dashboard: `http://127.0.0.1:<port>/dashboard/` (mint bootstrap codes via `POST /api/v1/dashboard/bootstrap/code` when `server.authMode=local-token`).

## Runtime layout

- Config: `~/.looper/config.toml`
- DB / logs / worktrees: under `~/.looper/` by default
- Worktree cleanup is daemon-scheduled (`daemon.worktreeCleanup`); no CLI `worktree cleanup` verb

## Fail-fast startup

`looperd` exits non-zero on invalid config, missing required tools (`git`, and `gh` when any project needs GitHub), or unwritable runtime paths.
