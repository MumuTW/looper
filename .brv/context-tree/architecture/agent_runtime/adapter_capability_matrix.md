---
title: Adapter Capability Matrix
summary: Adapter capability matrix records per-vendor support for session identity, headless resume, takeover, live events, and tool-network restriction
tags: []
related: []
keywords: []
createdAt: '2026-07-30T23:00:58.295Z'
updatedAt: '2026-07-30T23:00:58.295Z'
---
## Reason
Capture the vendor capability matrix and adapter-specific support declarations from Issue #100 PR1

## Raw Concept
**Task:**
Document the runtime adapter matrix and vendor-specific capability declarations from Issue #100 PR1

**Changes:**
- Added Claude Code, Codex, OpenCode, Cursor CLI, Grok Build, and Devin Experimental entries
- Characterized per-vendor support for session identity, resume, takeover, live events, and network restriction
- Marked research-only and unsupported behaviors explicitly in the matrix

**Files:**
- internal/agent/adapter.go
- internal/agent/runtime_contract_test.go

**Flow:**
vendor selection -> adapter declaration -> capability evidence -> matrix validation -> executor behavior gating

**Timestamp:** 2026-07-30T23:00:21.598Z

## Narrative
### Structure
Each adapter declaration pairs a command with a runtime contract and resolver functions. Claude, Codex, OpenCode, and Cursor expose partial continuation capabilities, while Grok Build remains unsupported and Devin Experimental is explicitly characterized as experimental.

### Dependencies
The matrix depends on adapter resolver functions and contract tests that compare declared support against spawn behavior.

### Highlights
Codex is the most capable adapter in the matrix, with live JSONL events and network restriction support. Claude Code has both headless resume and interactive takeover, but no structured live events or network restriction support. Research-only vendors are excluded from normal adapter admission.

### Rules
A normal configured adapter must have a non-empty command, non-interactive prompt construction, model selection, terminal-result translation, and common supervisor guarantees. Optional lifecycle and security behavior is enabled only when contract evidence is supported.

## Facts
- **claude_code_capabilities**: Claude Code uses command claude and supports translated session identity plus native headless resume and interactive takeover [project]
- **codex_capabilities**: Codex uses command codex and supports translated session identity, native headless resume, native interactive takeover, native structured live events, and Looper-enforced tool-network restriction [project]
- **opencode_capabilities**: OpenCode uses command opencode and supports translated session identity and native headless resume [project]
- **cursor_cli_capabilities**: Cursor CLI uses command agent and supports translated session identity and native headless resume [project]
- **grok_build_capabilities**: Grok Build uses command grok and has unsupported capability evidence across the matrix [project]
- **devin_experimental_capabilities**: Devin Experimental uses command devin and marks executable discovery, non-interactive prompt, and model selection as experimental for devin-3000.3.22 [project]
