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

Dashboard: `http://127.0.0.1:<port>/dashboard/` (mint bootstrap codes via `POST /api/v1/dashboard/bootstrap/code` when `server.authMode=local-token`).

## Runtime layout

- Config: `~/.looper/config.toml`
- DB / logs / worktrees: under `~/.looper/` by default
- Worktree cleanup is daemon-scheduled (`daemon.worktreeCleanup`); no CLI `worktree cleanup` verb

## Fail-fast startup

`looperd` exits non-zero on invalid config, missing required tools (`git`, and `gh` when any project needs GitHub), or unwritable runtime paths.
