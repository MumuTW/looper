# CLI reference (stripped surface)

## Operator verbs

| Command | Purpose |
| --- | --- |
| `looper version` | Print CLI version |
| `looper stop <selector>` | Stop the active run (`all` = every run) |
| `looper close <selector>` | Stop and close the loop |
| `looper start <selector>` | Start a known loop now |
| `looper pause <selector>` | Pause a loop |
| `looper retry <selector>` | Requeue a paused/parked/failed loop |
| `looper takeover <selector>` | Park a loop for manual worktree work |
| `looper handback <selector>` | Return a parked loop to the daemon |
| `looper respond <selector> "<answer>"` | Answer a human-gated loop and resume it |

Global flags (before or after the verb): `--config <path>`, `--host <host>`, `--port <port>`.

A **selector** is a loop sequence number (`12`) or loop id (`loop_…`). Pull request URLs are rejected.

## Machine-only

| Command | Purpose |
| --- | --- |
| `looper review submit <repo>#<n> --event … --commit-id …` | Trusted reviewer publish path. Daemon proxy only. Never suggest to operators. |

## Removed (do not invent)

`bootstrap`, `init`, `status`, `project *`, `daemon *`, `upgrade`, `config *`, `webhook *`, `provider *`, `network *`, `ps`, `logs`, `jump`, `plan`, `review` (operator), `work`, `loop start`, `pr *`, `takeover <owner>/<repo>#<n>`, `takeover list`.

## Install scripts

- `scripts/install.sh` — installs **only** the `looper` CLI binary.
- `looperd` is a separate release artifact (or `go build ./cmd/looperd`).
- `scripts/takeover.sh` — **removed**.

## Replacements for common removed flows

| Old | Now |
| --- | --- |
| `looper bootstrap` | Write `~/.looper/config.toml`, install `looperd`, run `looperd` |
| `looper daemon start` | `looperd` (foreground) or user's process manager |
| `looper project add <path>` | Dashboard, or `POST /api/v1/projects` with `{"repoPath":"…"}` |
| `looper status` | `curl …/api/v1/healthz` and `/api/v1/status` |
| `looper ps` / `logs` / `jump` | Dashboard / API; worktree path from dirty-retry dialog |
| `looper plan` / `review` / `work` | Label/assign on the forge; daemon discovery claims the work |
