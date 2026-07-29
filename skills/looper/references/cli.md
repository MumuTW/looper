# CLI reference (stripped surface)

## Operator verbs

| Command | Purpose |
| --- | --- |
| `looper version` | Print CLI version |
| `looper init` | Write a commented starter config to the selected path. Refuses to overwrite; prints the path. The only verb that needs no daemon. |
| `looper status` | Report the selected config file and whether it loaded, daemon reachability and health, and registered projects. Non-zero exit when the config fails to load or the daemon is unreachable. |
| `looper project add <path>` | Register a git repository **root** with the running daemon. Refuses a non-root path or an already-registered checkout. |
| `looper project list` | List registered projects |
| `looper stop <selector>` | Stop the active run (`all` = every run) |
| `looper close <selector>` | Stop and close the loop |
| `looper start <selector>` | Start a known loop now |
| `looper pause <selector>` | Pause a loop |
| `looper retry <selector>` | Requeue a paused/parked/failed loop. Dirty managed worktrees fail closed unless `--discard-worktree-changes --confirm`. |
| `looper takeover <selector>` | Park a loop for manual worktree work |
| `looper handback <selector>` | Return a parked loop to the daemon |
| `looper respond <selector> "<answer>"` | Answer a human-gated loop and resume it |

Global flags (before or after the verb): `--config <path>`, `--host <host>`, `--port <port>`.

A **selector** is a loop sequence number (`12`) or loop id (`loop_…`). Pull request URLs are rejected. `stop all` is not a loop named "all" — it is a distinct selector that routes to the daemon's bulk-stop endpoint.

Argument parsing, which decides whether a command reaches the daemon at all:

- **An unrecognised flag is refused, never read as a selector.** `looper stop --bogus 12` exits 2 with `unknown flag "--bogus"` rather than treating `--bogus` as the loop to stop.
- **`--` ends flag parsing; everything after it is an operand.** This is the only way to pass an argument that begins with a dash. `looper respond 12 -oops` fails with `unknown flag "-oops"`; `looper respond -- 12 -oops` reaches the daemon. Reach for it whenever a `respond` answer could start with a dash.

## Machine-only

| Command | Purpose |
| --- | --- |
| `looper review submit <repo>#<n> --event … --commit-id …` | Trusted reviewer publish path. Daemon proxy only. Never suggest to operators. |

## Removed (do not invent)

`bootstrap`, `daemon *`, `upgrade`, `config *`, `webhook *`, `provider *`, `network *`, `ps`, `logs`, `jump`, `plan`, `review` (operator), `work`, `loop start`, `pr *`, `takeover <owner>/<repo>#<n>`, `takeover list`.

## Install scripts

- `scripts/install.sh` — installs **only** the `looper` CLI binary.
- `looperd` is a separate release artifact (or `go build ./cmd/looperd`).
- `scripts/takeover.sh` — **removed**.

## Replacements for common removed flows

| Old | Now |
| --- | --- |
| `looper bootstrap` | `looper init`, edit the config, install and run `looperd`, `looper project add` |
| `looper daemon start` | `looperd` (foreground) or user's process manager |
| `looper project add --id/--repo/--base-branch …` | The CLI verb takes a path only; the other fields are dashboard / `POST /api/v1/projects` |
| `looper ps` / `logs` / `jump` | Dashboard / API; worktree path from dirty-retry dialog |
| `looper plan` / `review` / `work` | Label/assign on the forge; daemon discovery claims the work |
