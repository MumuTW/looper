# Coding-agent runtime contract

Looper integrates coding-agent CLIs through behavior-oriented runtime
contracts. A configured vendor name selects an adapter; the adapter's
`RuntimeContract` declaration is the authority for capability policy. Contract
tests compare declarations with behavior-bearing adapter functions and spawn
arguments so policy does not have to infer support from a vendor name.

This is a characterization of Looper's current integrations, not a promise
about every feature exposed by the upstream CLI. `experimental` means the
observed integration is deliberately weaker or pinned to time-sensitive
evidence. `unsupported` means Looper must fail closed or use a documented
fallback even when the upstream CLI might expose a related feature.

## Support modes

| Mode | Meaning |
| --- | --- |
| `native` | The selected CLI directly provides the behavior Looper invokes. |
| `looper_translated` | Looper translates CLI output into its stable execution result or session identity. |
| `looper_enforced` | Looper's supervisor supplies the guarantee around the child process or tool environment. |
| `experimental` | The behavior is characterized against explicitly versioned evidence but is not a normal supported lifecycle path. |
| `unsupported` | The adapter does not provide an enforceable implementation for this capability. |

## Configured adapters

The source declaration records evidence for every capability, including
unsupported ones. This compact matrix highlights the capabilities that differ
between configured adapters; all rows also receive the common supervisor
guarantees listed below.

| Vendor | Command | Session identity | Headless resume | Interactive takeover | Structured live events | Tool-network restriction |
| --- | --- | --- | --- | --- | --- | --- |
| `claude-code` | `claude` | translated | native | native | unsupported | unsupported |
| `codex` | `codex` | translated from JSONL | native | native | native JSONL | Looper-enforced validation sandbox |
| `opencode` | `opencode` | translated | native | unsupported | unsupported | unsupported |
| `cursor-cli` | `agent` | translated | native | unsupported | unsupported | unsupported |
| `grok-build` | `grok` | unsupported | unsupported | unsupported | unsupported | unsupported |
| `devin-experimental` | `devin` | unsupported | unsupported | unsupported | unsupported | unsupported |

Every configured adapter supports a non-interactive prompt and model
selection. Looper translates the `__LOOPER_RESULT__` terminal marker for each
adapter. Executable discovery, a version probe, and an authentication preflight
are separate capabilities: invocation does not imply that Looper can diagnose
an incompatible version or unauthenticated account before starting a run.
Today no adapter implements the latter two probes.

`devin-experimental` discovery, prompt, and model behavior is characterized
against Devin CLI `3000.3.22`; see [configuration](configuration.md#devin-cli-experimental-fresh-run)
for its fresh-run-only boundary.

## Supervisor-owned guarantees

`ConfiguredExecutor`, rather than each adapter, owns these invariants for all
configured vendors:

- process-group containment and confirmed cancellation;
- bounded stdout/stderr persistence;
- runtime deadlines, durable checkpoints, and worktree authority;
- fresh-session checkpoint retries when native resume is unavailable.

These guarantees do not manufacture CLI capabilities. In particular, a fresh
checkpoint retry is not native session resume, and an unrestricted child
process is not made network-safe merely because Looper supervises it.

## Minimum supported bar

A normal configured adapter must have a non-empty command, non-interactive
prompt construction, model selection, terminal-result translation, and the
common supervisor guarantees. Optional lifecycle and security behavior is
enabled only when its contract evidence is supported. Experimental adapters
may sit below this bar only when their vendor identifier and documentation make
that boundary explicit.

Hermes, Kimi, Pi, and Oh My Pi are research candidates, not configured Looper
adapters. Upstream support, protocol claims, or a locally installed executable
does not create a runtime contract; admission requires an adapter declaration,
behavioral characterization, and contract coverage.
