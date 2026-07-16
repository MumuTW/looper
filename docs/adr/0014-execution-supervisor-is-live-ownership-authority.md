# ADR-0014: Execution Supervisor is live ownership Authority

## Context

Looper starts agent and validation processes from Planner, Reviewer, Fixer,
Worker, Coordinator, feedback, and takeover paths. Live ownership is currently
split between `exec.Cmd` handles, an in-memory registry, persisted
`agent_executions` rows, and PID/process-group probes. Those representations can
disagree during spawn, native-resume fallback, terminal persistence, loop stop,
daemon shutdown, and startup recovery.

The concrete failures are an unregistered process surviving stop or shutdown,
a stale `running` write overwriting terminal state, a claimed queue item having
no live owner, and a numeric process-group ID being treated as identity after it
can be reused.

## Decision

The in-memory Execution Supervisor is the Authority for live execution
ownership in a running daemon. It owns admission, the live process handle, stop
delivery, confirmed wait, and release. Callers reserve ownership before they
claim or start work and cannot publish a half-started execution.

SQLite `agent_executions` rows are durable observations for operators and
startup recovery. They do not authorize a running daemon to release a live
handle or start overlapping work. Each execution serializes its durable writes;
the terminal observation is ordered after all live observations.

PID and process-group inspection is drift detection during startup recovery,
not live Authority. A backend may release ownership only after its containment
contract confirms that no runnable owned process remains. Backends that cannot
make that guarantee fail loudly instead of inferring success from signal
delivery.

Queue admission is ordered before claim. A reservation owns every claim from
the successful claim transition until the runner durably completes, cancels,
or requeues it. Loop stop and daemon shutdown close admission before waiting for
existing reservations.

## Trade-off

This prevents stop, shutdown, recovery, persistence, and Scheduler paths from
independently inferring ownership and removes compensation code for claims that
were created without an owner.

The cost is one in-memory Supervisor lifecycle, explicit reservation release,
serialized execution persistence, and platform-specific containment adapters.
A daemon crash loses the in-memory Authority, so startup must conservatively
reconcile durable observations before enabling mutations. A containment adapter
failure can block shutdown rather than reporting success.

A simpler extension of the existing registry is insufficient because it only
registers some agent executions after spawn and does not own queue admission or
terminal persistence. More PID/PGID validation is also insufficient because a
numeric identifier is reusable and a probe followed by a signal is not atomic.

## Authority

The Authority for a live action is the Execution Supervisor reservation and its
owned containment handle; SQLite and process inspection are recovery evidence
only.

## Consequences

- Every execution-producing Role uses the same Supervisor Module.
- Loop stop and daemon shutdown close admission before cancellation or waiting.
- Queue claims cannot exist without a reservation that owns their finalization.
- Execution persistence has one ordered writer and cannot regress terminal
  state to active state.
- Startup recovery must complete before mutating requests or Scheduler claims.
- Tests assert ownership through the Supervisor interface and lifecycle
  contracts rather than internal flags.
