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

## Memory verification (2026-07-31)

Memory is the reason for putting Hermes in front of Devin at all, so it was
tested in both directions against the repo profile (see below). **Recall
works; tool-driven writes do not.**

- **Recall (works).** Hermes injects `memories/MEMORY.md` into the system
  prompt, which is entirely Hermes-side and backend-independent. With the
  profile's memory seeded, a fresh session answered a memory-only question
  correctly, and a later real coding task independently used the recalled
  facts: it ran `run_tests.py` (never mentioned in the prompt) and cited
  the LOOPER-SLUG convention while fixing the bug.
- **Real coding task (works).** Given a `slugify` helper failing 2 of 3
  checks, the session diagnosed the missing edge-strip, applied
  `text.strip("-")`, and the suite went to PASS. Verified independently by
  `git diff` and rerunning the tests.
- **Tool-driven memory writes (do NOT work).** Every attempt to make the
  model store a note via Hermes's `memory` tool failed, across the default
  and `summarizer` agent types and both `glm-5-2` and `swe-1-7`. Nothing was
  ever written to `memories/MEMORY.md`.

Root cause, isolated with a raw ACP probe: **Devin's ACP server owns the tool
loop.** Hermes's shim can only describe tools in prompt text and parse
`<tool_call>` blocks back out of the *message* channel, but Devin's model
resolves tool intent natively inside its own agent. With the default agent
type it calls Devin's own tools (which is why file edits land); with
`summarizer` (no tools) it reasons about calling `memory`, emits nothing to
the message channel, and the turn ends. The probe confirmed the mechanism: an
adversarial prompt asserting "you have NO native tools, write the JSON as
plain text" produced a correctly-shaped tool-call object in the message
channel on the first try — so the parsing path is fine, the model just never
chooses it on its own. Trying to force this globally via `SOUL.md` was tested
and rejected: it did not fix memory writes and it broke the coding path,
because the default agent type genuinely does have native tools.

Practical consequence: **treat repo memory as read-mostly.** Hermes recalls
and reasons over `memories/MEMORY.md` normally; writes should be made by a
non-ACP Hermes session or by editing the file directly. A `memory`-toolset
session on this backend will claim success without persisting anything.

One further friction: shell commands from the ACP session are intermittently
rejected/skipped mid-task (observed as "the command was skipped"), so a
session may apply a correct fix and then be unable to rerun the verification
it just wrote. Verify externally.

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

Go for interactive/experimental use of Hermes-on-Devin via the repo profile,
with memory treated as read-mostly: recall and repo-scoped memory both work,
which was the point of the exercise.

No-go for treating it as a production provider until the per-turn process
spawn, zero usage reporting, and — the blocker that matters most — the
inability to drive Hermes-side tools are addressed. The last one is
structural, not a config gap: as long as Devin's ACP server owns the tool
loop, Hermes tools stay unreachable. Fixing it means either a carried Hermes
patch (a Devin-aware ACP shim that maps Hermes tools onto Devin's own tool
surface) or an upstream change neither project has planned.

## Per-repo profile

Hermes has no per-repo profile discovery — a profile is just an independent
`HERMES_HOME` directory (`~/.hermes/profiles/<name>/`), selected by exporting
`HERMES_HOME`. Note that `HERMES_PROFILE` is *not* the selector: it is read by
a few kanban subcommands only, and a session started with it silently writes
to the default profile's memory (observed during this spike — a note meant for
the repo profile landed in `~/.hermes/memories/MEMORY.md`).

`scripts/hermes-profile.sh` is the repo's checked-in definition of that
selection, so every clone drives the same profile and the same backend:

```bash
scripts/hermes-profile.sh --bootstrap   # create/repair the profile (once)
source scripts/hermes-profile.sh        # export HERMES_HOME for this shell
hermes
```

The profile directory itself (config.yaml, .env, SOUL.md, memories/) is user
state and is not checked in; `--bootstrap` rewrites config.yaml and .env while
leaving `memories/` untouched. Repo-scoped memory is a real benefit here: the
looper profile's memory stays separate from the default profile's.

## Reproduce (without the profile script)

```bash
export HERMES_COPILOT_ACP_COMMAND=devin
export HERMES_COPILOT_ACP_ARGS="acp --model glm-5-2"
hermes --provider copilot-acp -m copilot-acp
```
