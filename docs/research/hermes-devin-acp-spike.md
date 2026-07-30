# Hermes Agent ↔ Devin ACP spike

Captured on 2026-07-31 against a real authenticated self-serve Devin account.
Goal: use Hermes Agent as the harness (sessions, memory, tool loop) with
Devin CLI's free `glm-5-2` as the model backend, without OmniRoute or a
custom OpenAI-compat shim.

## Safety precondition: this configuration reads and writes your workspace

**Run every command in this document from a disposable worktree or scratch
directory.** Denying `session/request_permission` is NOT a containment
boundary, and nothing here demonstrates a sandbox or network boundary:

- Hermes's shim answers `fs/write_text_file` by writing directly inside the
  session cwd, with no `session/request_permission` round-trip. It enforces
  only "inside cwd" plus Hermes's own write-deny list
  ([v2026.7.20 copilot_acp_client.py#L732-L746](https://github.com/NousResearch/hermes-agent/blob/v2026.7.20/agent/copilot_acp_client.py#L732-L746);
  the permission denial is [#L137-L147](https://github.com/NousResearch/hermes-agent/blob/v2026.7.20/agent/copilot_acp_client.py#L137-L147)).
- The shim's own prompt instructs the ACP backend to "use ACP capabilities
  to complete tasks", so the backend is actively encouraged to act.
- In the default agent type, Devin additionally runs its own native tools
  inside its own loop (see the tool-loop finding below) — those are not
  mediated by Hermes at all.
- `devin acp` is a plain subprocess JSON-RPC server; Devin documents
  sandboxing only when `--sandbox` is explicitly enabled
  ([devin acp](https://docs.devin.ai/cli/reference/commands#devin-acp),
  [sandbox](https://docs.devin.ai/cli/sandbox)). No recipe here enables it.

The evidence below was captured in throwaway `mktemp -d` directories.

## Verified surface

- Hermes: `v0.19.0 (2026.7.20)`, install dir `~/.hermes/hermes-agent`
- Devin: `devin 3000.3.22 (d5152ff5)`, `devin acp` ACP server over stdio
- Bridge: Hermes's `copilot-acp` provider (`agent/copilot_acp_client.py`).
  Despite the name, the shim speaks generic ACP protocol v1
  (`initialize` → `session/new` → `session/prompt`), collects
  `agent_message_chunk`/`agent_thought_chunk` updates, denies
  `session/request_permission`, and services `fs/read_text_file` /
  `fs/write_text_file` itself (see the safety note above). The only
  Copilot-specific parts are the default command and a gh-copilot
  deprecation check — neither blocks Devin.
- Override hooks (no Hermes patch needed):
  - `HERMES_COPILOT_ACP_COMMAND=devin`
  - `HERMES_COPILOT_ACP_ARGS="acp --model glm-5-2"`
  - `devin acp --model` accepts the same fuzzy names as `/model` and also
    reads `DEVIN_MODEL`.

## Test evidence

Versioned artifacts live in `testdata/hermes-devin-acp/`:

- `replay_acp.py` — the probe itself, mirroring the Hermes shim's JSON-RPC
  sequence. Requires `--cwd`; point it at a disposable directory.
- `replay-default-agent.txt` — captured output for claim 1 below.
- `replay-toolcall-probe.txt` + the two `prompt-toolcall-*.txt` inputs —
  captured output for the tool-loop finding.
- `replay-mcp-route.txt` + `probe_mcp_server.py` — the stub MCP server and
  captured output showing Hermes-side tools are reachable via MCP.

Session ids and message uuids in the captures are replaced with
`<REDACTED-*>`; no credentials, tokens, account identity, or repository
content appear in them, and each capture was reread before check-in to
confirm that. Everything else is verbatim. Re-run the script to reproduce
(token counts and cache hit rates will differ run to run).

### Why these captures are checked in (trade-off)

These replay captures are a new persisted evidence record, so per the
repo's design rule they answer two questions:

- **Delete them six months from now — what breaks?** The protocol, token,
  error-code, and tool-loop claims below lose their auditable basis. A
  reader could not re-derive whether Devin still returns `end_turn` with
  real usage, still emits the `-32601`-rejected `_cognition.ai/*` methods,
  or still owns the tool loop; the doc would revert to unverified operator
  notes — exactly the state the first review blocker on this PR rejected.
  They also pin wire shapes that are not documented upstream (e.g.
  `session/request_permission` carries no tool name; bare-JSON tool calls
  parse via the shim's fallback regex), so deleting them loses the only
  record of those defects.
- **What do they still not catch?** They are point-in-time against
  `devin 3000.3.22` / Hermes `v0.19.0 (2026.7.20)`. They do not catch drift
  on either side after capture — a release that changes a wire shape
  silently invalidates them with no signal here. They also do not cover
  behavior the probe does not exercise: the probe deliberately declares no
  `fs` capability, so filesystem write behavior is described in the safety
  section but not captured as an artifact, and the per-turn respawn cost
  and rolling rate limit are observed in prose, not replayed.

A live-only probe (re-run on demand, nothing checked in) needs a live
authenticated Devin account and a working Hermes install at the pinned
versions; a reviewer or future maintainer without those cannot audit the
claims at all. A fail-loud probe alone — the default-mode assertions
`replay_acp.py` now makes — guards future re-runs but records nothing
about the versions it was first run against, so it cannot answer "what did
this used to do?" when something later breaks. The checked-in capture is
the immutable evidence; the probe plus its assertions is the
reproducibility guard. Both are needed, which is why neither alone was
sufficient.

1. **Raw handshake replay** (`replay-default-agent.txt`):
   `initialize` returned protocol v1 with
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
   Not separately captured — trivially re-runnable from the recipe below.
3. **Hermes tool loop**: a `--yolo` one-shot asking Hermes to create and
   read back a file, run in a throwaway directory, completed correctly
   (file written with exact content, read back, verified externally). The
   loop took several rounds: `glm-5-2` behind Devin intermittently produced
   tool-call JSON that Hermes's `write` tool path reported as "Parse error"
   before the model recovered via alternate tools. Functional, with visible
   friction. This is Devin's own tool loop doing the work, not Hermes's —
   see the memory section for why that distinction matters. Transcript not
   captured (it contains scratch paths); the tool-loop *ownership* claim is
   backed by `replay-toolcall-probe.txt` instead, which isolates the
   mechanism directly.

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

Root cause, isolated with a raw ACP probe
(`testdata/hermes-devin-acp/replay-toolcall-probe.txt`): **Devin's ACP server
owns the tool loop.** Hermes's shim can only describe tools in prompt text and parse
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

### The tool gap is closable via MCP, not via the prompt contract

A follow-up probe (`testdata/hermes-devin-acp/replay-mcp-route.txt`) found a
route that does work, and it inverts the fix: instead of fighting Devin for
ownership of the tool loop, hand it the tools and let it call them natively.

Devin's ACP `initialize` advertises `mcpCapabilities`, and a stdio MCP server
registered with `devin mcp add` is discovered and called end-to-end from
inside an ACP session: `tools/list` → `tools/call` → the tool executed with
the correct argument, and the model reported success truthfully. Two
constraints found along the way:

- **`devin mcp add` is the registration path, not `session/new`.** Passing
  the server inline in `session/new`'s `mcpServers` made Devin spawn the
  process and complete the MCP handshake, but its own registry never picked
  it up (`Server ... not found in configuration`) and `tools/list` was never
  called. `devin mcp add` writes a *local project config* keyed to the
  directory, so the ACP session cwd must match — which lines up neatly with
  a per-repo profile.
- **The shim's blanket permission denial blocks the call.** Hermes hardcodes
  `{"outcome": "cancelled"}` for every `session/request_permission`
  ([#L125-L134](https://github.com/NousResearch/hermes-agent/blob/v2026.7.20/agent/copilot_acp_client.py#L125-L134)),
  so a registered, discovered tool still gets rejected at call time. Devin
  offers `allow_once` / `allow_session` / `allow_always` (plus server-scoped
  variants); selecting one is what turned the probe green.

So a Hermes patch to make Hermes-side tools reachable is smaller and less
invasive than "map Hermes tools onto Devin's tool surface": expose the
desired Hermes tools (starting with `memory`, whose `MemoryStore` is already
a plain Python class) as a stdio MCP server, and replace the shim's
unconditional denial with a selective approval for calls to that server.
Neither piece requires touching Hermes's agent loop.

That patch now exists in `tools/hermes-devin/` — see below.

Note the security consequence: relaxing the permission denial widens what the
backend can do, and the denial was never a containment boundary to begin with
(see the safety section). Any such patch should approve narrowly — a
specific server, specific tool names — not switch on `allow_always`.

### End-to-end result (2026-07-31)

With the carried patch applied and the MCP server registered, a plain
`hermes -z` session on the Devin backend stored a note through Hermes's own
memory tool, and a subsequent fresh session recalled it — the full loop the
prompt-contract route could never close:

```
Stored to persistent memory (looper profile, via the hermes-memory MCP backend):
"E2E-PROOF-77: memory writes reach the looper profile through the Devin ACP backend."
Memory store now at 13% (302/2,200 chars), 3 entries.
```

The entry is in the profile's `MEMORY.md`, and a later session answering
"from memory only" reproduced it verbatim. Writes are no longer read-mostly
once both setup steps are done; without them, recall still works and writes
silently no-op.

Historical note, since the rest of this section describes the pre-patch
state: **treat repo memory as read-mostly** on an unpatched install. Hermes recalls
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
- Devin's own agent persona wraps the model. Hermes auto-denies
  `session/request_permission` requests, but that denial is not a
  containment boundary: in the default agent type Devin also runs its own
  native tools inside its own loop, which Hermes does not mediate at all
  and which did write files during this spike (see the tool-loop finding).
  Only permission-gated calls are blocked; unmediated native actions still
  land, and the persona still colors responses.
- `glm-5-2` free tier is an observation at capture time, not a durable
  promise (see `devin-cli-3000.3.22.md`); rerun
  `devin models list --format json` before relying on it.
- **The free tier rate-limits by message count, on a rolling window.**
  Observed as `Reached overall message rate limit ... resets in N minutes`
  surfaced through the shim as a `session/prompt` permission error. Two
  consequences worth knowing before you rely on this: Hermes's stock 3-retry
  policy turns one rate-limited call into three, each burning another unit of
  the same quota (the profile therefore sets `agent.api_max_retries: 1`), and
  because the window is rolling, retrying while limited keeps renewing it —
  back off and leave it alone rather than polling.
- **This is not a supported integration.** Hermes's official provider docs
  define `HERMES_COPILOT_ACP_COMMAND`/`_ARGS` only as overrides pointing at
  the *Copilot CLI* binary and its arguments
  ([v2026.7.20 providers.md](https://github.com/NousResearch/hermes-agent/blob/v2026.7.20/website/docs/integrations/providers.md#L195-L211)),
  and upstream closed the request to generalize ACP harnesses as **not
  planned** ([#16282](https://github.com/NousResearch/hermes-agent/issues/16282)),
  explicitly declining to support non-Copilot ACP backends. Treat what this
  document describes as an observed, version-pinned (Hermes v2026.7.20 /
  devin 3000.3.22), unpatched compatibility workaround with no contract
  behind it: any Hermes or Devin release may break it without notice. A
  named `devin-acp` provider would require carrying a Hermes patch.

## Go/no-go

Go for interactive/experimental use of Hermes-on-Devin via the repo profile,
with memory treated as read-mostly: recall and repo-scoped memory both work,
which was the point of the exercise.

The tool blocker is resolved. Devin's ACP server does own the tool loop, so
Hermes's prompt-level `<tool_call>` contract stays dead — but the MCP route
reaches the same destination from the other side, and the carried patch in
`tools/hermes-devin/` closes the loop end-to-end: a session stores through
Hermes's memory tool and a later session recalls it.

Remaining no-gos for production use are the per-turn process spawn and full
transcript replay, zero usage reporting to Hermes, the free tier's rolling
message rate limit, and the standing fact that this is an unsupported,
version-pinned integration that any release on either side may break. The
patch is carried, not upstream, so a Hermes upgrade will refuse to apply it
until it is rebuilt against the new file.

## The carried patch (`tools/hermes-devin/`)

Two components, plus profile wiring:

- **`memory_mcp_server.py`** — stdlib-only stdio MCP server exposing Hermes's
  own memory store as `hermes_memory_add` / `_replace` / `_remove` / `_read`.
  It wraps Hermes's `load_on_disk_store()` and `memory_tool()` rather than
  reimplementing the format, builds a fresh store per call (Hermes sessions
  write the same files concurrently), re-execs into Hermes's venv (the memory
  import needs `yaml`, which the system interpreter lacks), and refuses to
  write when `HERMES_HOME` is unset instead of silently hitting the default
  profile. `--selftest` exercises it without an MCP client.
- **`acp-permission-allowlist.patch`** + **`apply-hermes-patch.sh`** —
  replaces the shim's blanket denial with an allow-list gate. Deny is still
  the default and the list is empty unless `HERMES_ACP_ALLOWED_MCP_TOOLS` is
  set, so an unconfigured install behaves exactly like stock Hermes. Only
  `allow_once` / `allow_session` can ever be selected: `allow_always` and
  `allow_server_always` outlive the session, `switch_bypass` drops the gate
  entirely, and the `allow_server_*` options approve *every* tool on that
  server — including ones never allow-listed, which would defeat the per-tool
  list. The apply script pins both stock and patched checksums and refuses to
  touch a Hermes that has moved underneath it.

### Two defects the e2e caught

Both were found only by running against the live server, and both are
recorded here because the wire shapes are not documented anywhere:

1. **`session/request_permission` carries no tool name.** Its params hold
   only `{sessionId, toolCall: {toolCallId}, options: [...]}`. The name
   appears earlier, on the `session/update` that announced the tool call, as
   `_meta["cognition.ai/toolName"]` — fully qualified, e.g.
   `mcp__hermes-memory__hermes_memory_add`. The gate therefore tracks
   `toolCallId → name` per run and correlates on arrival. A first attempt
   probed plausible name fields directly on the permission params, found
   nothing, and silently denied every call. The other available signal is the
   human-readable option labels ("Yes, allow calling X on the Y MCP
   server"), which would mean gating security on UI copy — rejected.
   Using the fully-qualified name also pins the server, so another server
   exposing a same-named tool does not inherit the grant.
2. **Devin spawns the MCP server, so `HERMES_HOME` does not propagate.**
   The server correctly refused to write rather than guessing a profile.
   Register it with the profile baked in: `devin mcp add <name> -e
   HERMES_HOME=<profile> -- <path to memory_mcp_server.py>`.

### Setup

```bash
tools/hermes-devin/apply-hermes-patch.sh          # once; --revert to undo
devin mcp add hermes-memory \
  -e HERMES_HOME="$HOME/.hermes/profiles/looper" \
  -- "$PWD/tools/hermes-devin/memory_mcp_server.py"
```

**Register once per repo, from the repo root.** `devin mcp add` writes
`.devin/mcp_config.local.json` into the current directory, and Devin walks
*up* from a session's cwd to find it — so a root-level registration covers
every subdirectory (verified: a session run from `internal/` wrote memory
through the root registration). There is no global fallback; a directory
outside any tree containing `.devin/` sees no servers at all, which is the
failure mode to recognise — memory writes silently no-op while recall keeps
working.

That file is gitignored on purpose: it holds absolute paths and the
`HERMES_HOME` of whoever ran the command, so it is per-clone state, not
shared config. `scripts/hermes-profile.sh --bootstrap` writes the matching
allow-list into the profile's `.env` and prints the exact command for this
checkout.

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

Precondition: run this from a disposable worktree or scratch directory. The
session can read and write that directory (see the safety section), and
neither Hermes nor `devin acp` is configured here with a sandbox or network
boundary.

```bash
REPO="$PWD"; cd "$(mktemp -d)"
export HERMES_COPILOT_ACP_COMMAND=devin
export HERMES_COPILOT_ACP_ARGS="acp --model glm-5-2"
hermes --provider copilot-acp -m copilot-acp
```

To re-derive the protocol claims without running Hermes at all (`$REPO` from
above, since the recipe left you in the disposable directory):

```bash
"$REPO"/docs/research/testdata/hermes-devin-acp/replay_acp.py --cwd "$PWD"
```
