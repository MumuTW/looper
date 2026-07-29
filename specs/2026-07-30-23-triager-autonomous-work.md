# Issue #23 — Triager autonomous work intake

## Goal

Let a personal GitHub repository progress from a new or reopened Issue to
Looper planning without requiring a person to apply a routing label.

## Scope

- Register an internal Triager lane in the source-based discovery scheduler.
- Support GitHub `new` and `reopened` Issue source events.
- Run deterministic preflight before any LLM or workflow side effect.
- Persist one structured, auditable Triage Report per source event.
- Route only high-confidence, low-risk, sufficiently specified reports to
  Planner.
- Leave unsafe, uncertain, out-of-scope, or underspecified reports waiting for
  human confirmation.
- Preserve Planner's existing label discovery and Fixer's review-feedback
  discovery.

External repositories, non-GitHub task sources, autonomous merge, and replacing
the existing Fixer lane are out of scope.

## Source and preflight

Triager is an internal GitHub Issue-source lane ordered ahead of Planner. It
runs only when:

- the Project is active and GitHub-backed;
- Planner resolves to a configured agent and Planner auto-discovery is enabled;
- the legacy Coordinator intake path is disabled for that Project.

Each poll searches GitHub Issues updated within a lookback of at least five
minutes, ordered newest update first. This event window avoids enrolling the
historical open backlog while tolerating ordinary delayed ticks. For each
candidate, Triager re-reads the target and its timeline. It rejects stale source
events, closed targets, pull requests, malformed targets, and the existing
global `looper:hold` before invoking the LLM. The latest `reopened` timeline
event is the source when present; otherwise Issue creation is the source.

The idempotency key hashes Project, repository, Issue number, source-event kind,
timestamp, and event ID. A persisted report with that key suppresses another
LLM decision and is replayed as routing authority.

## Structured decision and policy

The LLM returns strict JSON containing:

- classification;
- scope;
- risk;
- confidence;
- missing information;
- recommended next Role;
- rationale.

The deterministic policy permits automatic Planner routing only when
classification is supported, scope is `in_scope`, risk is `low`, confidence is
at least `0.8`, missing information is empty, the recommended next Role is
`planner`, and rationale is present. Every failed condition is persisted as an
explainable reason with `await_human_confirmation`.

A later Issue comment whose complete body is `/plan` confirms the exact
persisted report only when GitHub reports the author has `write`, `maintain`, or
`admin` repository permission. Triager persists `triage.confirmed` before
projecting the report into Planner. This supplies a confirmation transition
without making a routing label a prerequisite.

## Authority and ordering

The persisted `triage.report` event is the semantic Authority for automatic
Planner routing. It is written before Planner receives work. If Planner
projection fails, the next discovery tick reads the same report and retries the
idempotent projection without calling the LLM again.

Planner's explicit report-authorized entrypoint bypasses label and assignee
discovery filters, then reuses Planner's durable loop and active-queue dedupe.
Its failed-run fingerprint is derived only from the report authority, so edits
to mutable Issue fields cannot revive the same report; a reopened event creates
a new report authority.
GitHub labels are not required and are not written in this slice.

## Trade-off: persisted Triage Report

Concrete failure prevented: without a durable report, a restart or partial
failure can repeat an LLM decision, lose the explanation for a human wait, or
make a label look like the semantic decision.

Cost: reports add event-log records, parsing/version compatibility, source-event
key rules, replay behavior, and a second intake vocabulary beside the legacy
Coordinator path. Corrupt reports fail loudly and require operator repair.

Why the simpler alternative is insufficient: trusting an in-memory structured
LLM response cannot survive restart or authorize a retry after Planner
projection fails. Making a trigger label authoritative would revive the
label-gated workflow this issue removes.

## Deletion attempt

The first alternative considered was deleting the new persistence concept and
feeding the LLM response directly to Planner. That removes report parsing and
replay code, but also removes auditability, durable idempotency, and the named
authority required before Planner side effects. The implementation instead
reuses the existing event log rather than adding a table, migration, ledger, or
status column.

## Authority statement

The Authority for automatic Planner routing is the persisted Triage Report,
not the agent's transient output: deterministic policy is recorded with the
decision before the idempotent Planner projection runs.

## Verification

Focused tests cover eligible routing without labels or assignment, replay
idempotency across mutable Issue edits, new and reopened source events,
historical-backlog exclusion, existing hold behavior, unsafe and uncertain
decisions, write-authorized confirmation, GitHub-only lane support, and
preservation of Fixer's Forgejo discovery support. Repository validation is `gofmt -l .`,
`go vet ./...`, `go test ./...`, and `go build ./...`.
