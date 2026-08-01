---
title: Internal Agent Runtime Contract
summary: Internal agent runtime contract defines adapter-owned capability evidence for CLI behavior, Looper translation/enforcement, and unsupported capabilities without persisted runtime state
tags: []
related: [architecture/agent_runtime/internal_agent_runtime_contract.md]
keywords: []
createdAt: '2026-07-30T23:00:58.290Z'
updatedAt: '2026-07-30T23:00:58.290Z'
---
## Reason
Document the adapter-owned runtime contract and capability policy for Issue #100 PR1

## Raw Concept
**Task:**
Document the internal agent runtime contract that governs adapter capability policy for Issue #100 PR1

**Changes:**
- Added RuntimeContract and RuntimeSupport types
- Added capability evidence for supported, translated, enforced, experimental, and unsupported behaviors
- Moved capability decisions away from vendor-name comparisons
- Bound executor behavior to contract-backed capability checks
- Defined research-only adapter admission requirements

**Files:**
- internal/agent/runtime_contract.go
- internal/agent/runtime_contract_test.go
- internal/agent/adapter.go
- internal/agent/executor.go

**Flow:**
adapter declaration -> capability evidence -> runtime contract lookup -> executor policy decision -> supported or unsupported behavior

**Timestamp:** 2026-07-30T23:00:21.598Z

**Author:** Issue #100 PR1

**Patterns:**
- `^RuntimeSupport(Native|LooperTranslated|LooperEnforced|Experimental|Unsupported)$` - Enumerates the allowed runtime support values

## Narrative
### Structure
The contract layer is owned by each runtime adapter, while the executor retains process containment, persistence, cancellation, checkpoints, and worktree authority. The contract exposes capability support as evidence rather than by vendor heuristics, and RuntimeContractFor returns a defensive copy for consumers.

### Dependencies
Depends on adapter characterization tests, runtime adapter declarations, and executor policy checks. The design explicitly excludes persisted runtime state from the contract layer.

### Highlights
Native resume and takeover are now capability-driven. Codex is the only adapter shown with native structured live events and Looper-enforced tool-network restriction. Research-only vendors remain out of the adapter set until admitted.

### Rules
Contracts add no persisted runtime state. New adapters must declare evidence for every required capability. Core policy no longer compares vendor names for capability support. Research-only Hermes, Kimi, Pi, and Oh My Pi are not adapters until admitted through this contract.

## Facts
- **runtime_contract_authority**: Issue #100 PR1 establishes internal/agent RuntimeContract as the adapter-owned authority for capability policy [project]
- **capability_evidence_modes**: Capability evidence distinguishes CLI-native behavior, Looper translation/enforcement, experimental characterization, and explicit unsupported behavior [project]
- **contract_consulted_capabilities**: Core policy now consults the contract for native resume, interactive takeover, structured live events, and tool-network restriction [project]
- **vendor_name_policy**: Core policy no longer compares vendor names for capability support [project]
- **configured_executor_authority**: ConfiguredExecutor still owns process containment, confirmed cancellation, bounded persistence, checkpoints, and worktree authority [project]
- **contract_state**: Contracts add no persisted runtime state [project]
- **adapter_requirements**: New adapters must declare evidence for every required capability and pass behavior/argv characterization tests [project]
- **research_only_adapters**: Research-only Hermes, Kimi, Pi, and Oh My Pi are not adapters until admitted through the runtime contract [project]
- **runtime_contract_shape**: RuntimeContract version 1 stores vendor, default command, and capability evidence map [project]
- **runtime_support_values**: RuntimeSupport values are native, looper_translated, looper_enforced, experimental, and unsupported [project]
- **runtime_contract_for**: RuntimeContractFor returns a defensive copy and unknown vendors have no contract [project]
