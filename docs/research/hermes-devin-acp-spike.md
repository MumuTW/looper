# Hermes Agent ↔ Devin ACP spike

Captured on 2026-07-31 against a real authenticated self-serve Devin account.
Goal: use Hermes Agent as the harness (sessions, memory, tool loop) with
Devin CLI's free `glm-5-2` as the model backend, without OmniRoute or a
custom OpenAI-compat shim.

## Verified surface

- Hermes: `v0.19.0 (2026.7.20)`, install dir `~/.hermes/hermes-agent`
- Devin: `devin 3000.3.22 (d5152ff5)`, `devin acp` ACP server over stdio
- Bridge: Hermes's `copilot-acp` provider (`agent/copilot_acp_client.py`).
  Despite the name, the shim speaks generic ACP protocol v1
  (`initialize` → `session/new` → `session/prompt`), collects
  `agent_message_chunk`/`agent_thought_chunk` updates, and denies
  `session/request_permission`. The only Copilot-specific parts are the
  default command and a gh-copilot deprecation check — neither blocks Devin.
- Override hooks (no Hermes patch needed):
  - `HERMES_COPILOT_ACP_COMMAND=devin`
  - `HERMES_COPILOT_ACP_ARGS="acp --model glm-5-2"`
  - `devin acp --model` accepts the same fuzzy names as `/model` and also
    reads `DEVIN_MODEL`.

## Test evidence

1. **Raw handshake replay** (script mirroring the Hermes shim's exact
   JSON-RPC sequence): `initialize` returned protocol v1 with
   `cognition.ai/*` meta capabilities; `session/new` returned a session with
   modes (`accept-edits` default); `session/prompt` returned the exact
   requested constant with `stopReason: end_turn` and real token usage
   (~13.6k input tokens for a trivial prompt — Devin's own system prompt
   overhead). Devin emitted two client methods Hermes rejects with -32601
   (`_cognition.ai/mcp/serversChanged`, `_cognition.ai/agent_stopped`);
   the rejection did not disrupt the session.
2. **Hermes one-shot end-to-end**:
   `hermes --provider copilot-acp -m copilot-acp -z "<constant prompt>"`
   with the two env overrides returned the exact requested constant.
3. **Hermes tool loop**: a `--yolo` one-shot asking Hermes to create and
   read back a file completed correctly (file written with exact content,
   read back, verified). The loop took several rounds: `glm-5-2` behind
   Devin intermittently produced tool-call JSON that Hermes's `write` tool
   path reported as "Parse error" before the model recovered via alternate
   tools. Functional, with visible friction.

## Known costs and caveats

- The shim spawns a fresh `devin acp` process per model request and replays
  the full conversation transcript as one prompt. With a free model the
  cost is latency, not money, but long sessions re-send everything each turn.
- Hermes sees hardcoded zero usage from the shim; token accounting lives
  only in Devin's ATIF/usage side.
- Tool calling is prompt-level emulation (`<tool_call>` blocks), not native
  function calling; expect occasional malformed-call retries.
- Devin's own agent persona wraps the model. Permission requests from
  Devin-side tools are auto-denied by Hermes, so Devin cannot act on its
  own, but its system prompt still colors responses.
- `glm-5-2` free tier is an observation at capture time, not a durable
  promise (see `devin-cli-3000.3.22.md`); rerun
  `devin models list --format json` before relying on it.
- Upstream Hermes closed the "generalize ACP harnesses" request as not
  planned (NousResearch/hermes-agent#16282), so the env-var override is the
  supported-in-practice path; a named `devin-acp` provider would require
  carrying a Hermes patch.

## Go/no-go

Go for interactive/experimental use of Hermes-on-Devin via the two env
overrides. No-go for treating it as a production provider until the
per-turn process spawn, zero usage reporting, and prompt-level tool-call
fragility are addressed (either via a carried Hermes patch or an upstream
change).

## Reproduce

```bash
export HERMES_COPILOT_ACP_COMMAND=devin
export HERMES_COPILOT_ACP_ARGS="acp --model glm-5-2"
hermes --provider copilot-acp -m copilot-acp
```
