# Devin CLI 3000.3.22 integration evidence

Captured on 2026-07-30 from a real authenticated self-serve account. Identity
and unrelated model families were removed from the checked-in fixtures.

## Verified surface

- Executable/version: `devin 3000.3.22 (d5152ff5)`
- Authentication diagnostic: `devin auth status` exits successfully and reports
  the active account and team without requiring an interactive prompt.
- Non-interactive prompt: `devin --model glm-5-2 --permission-mode auto
  --respect-workspace-trust false --print "<prompt>"` exits successfully and
  writes the assistant response to stdout.
- Structured model discovery: `devin models list --format json`
- Conversation export: `--export <path>` writes ATIF JSON containing
  `schema_version`, `session_id`, `agent`, `steps`, and `final_metrics`.
- Headless resume is exposed as `--resume <session-id>`, but Looper does not
  enable it until ATIF capture and resume continuity have contract coverage.
- ACP is exposed as `devin acp` over stdio JSON-RPC, but is outside this
  experimental plain-output adapter.

The live smoke prompt requested an exact constant response and prohibited tool
use. It returned the requested response and produced an ATIF session ID. No
repository or daemon process was involved.

## Model and cost evidence

The sanitized fixtures are under `internal/agent/testdata/`:

- `devin-3000.3.22-models.json`: relevant model-discovery families
- `devin-3000.3.22-print.txt`: captured stdout from the no-tool smoke
- `devin-3000.3.22-atif.json`: reduced ATIF shape from that same session

At capture time:

- `glm-5-2`: Free
- `swe-1-7`: Free, beta
- `swe-1-7-medium`: Free, beta
- alias `swe`: resolves to paid `swe-1-7-lightning`

These are observations, not durable pricing promises. Operators must rerun
`devin models list --format json` before relying on a free tier.

## Go/no-go

Go for an explicitly experimental, fresh-run-only adapter using Looper's
existing completion-marker translation. No-go for claiming native resume,
interactive takeover, structured live events, ACP, strict tool-network
containment, or permanent free pricing.
