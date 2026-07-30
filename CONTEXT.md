# Looper

Looper is a daemon (`looperd`) plus CLI (`looper`) that runs autonomous agent **Roles** against a configured forge repository's issues and pull requests.

## Language

### Roles

A **Role** is a configured agent that performs one specific job in the issue/PR lifecycle.

**Provider**:
A configured forge integration that owns remote Issues, Pull Requests, labels, comments, reviews, webhooks, and identity for a Project. Git remains separate and owns local repositories, refs, and worktrees.
_Avoid_: forge, host, remote.

**Project**:
A durable local registration that binds one repository to one Provider and supplies the project-level policy consumed by Roles. The SQLite project record is the runtime Authority for whether a Project exists and for its repository/Provider binding; `[[projects]]` is a startup import, not a parallel runtime Authority.
_Avoid_: config project, runtime binding.

**Project Catalog**:
The startup-built, immutable view of active Projects materialized from SQLite records after configuration import. Runtime modules consume the Project Catalog through the existing normalized project configuration interface; they do not consult the original `[[projects]]` input.
_Avoid_: registry, live config projects.

**Planner**:
A reactive Role that produces a Spec from an Issue.
_Avoid_: designer, architect.

**Triager**:
An internal proactive Role for the personal GitHub workflow. It consumes new
and reopened Issue events, persists a structured Triage Report, and projects
accepted low-risk reports directly into Planner work. It has no configurable
trigger labels and does not replace Fixer's review-feedback source.
_Avoid_: coordinator (a separate label/network control-plane role).

**Reproducer**:
An internal Role that runs between Triager and Planner for bug-classified
Issues. It authors a failing reproduction in its own worktree and agent session,
proves the reproduction command fails on the current base before accepting it,
commits it to the branch Planner will adopt, and persists a Reproduction Record.
When it cannot make the bug fail it produces a `cannot reproduce` record and
parks the Issue for a human instead of reaching Planner. It is bug-only and
default-disabled.
_Avoid_: reproducer agent, test writer, verifier.

**Worker**:
A reactive Role that implements a Spec or an Issue, producing a Pull Request.
_Avoid_: implementer, builder, coder.

**Reviewer**:
A reactive Role that reviews a Pull Request and posts review comments.
_Avoid_: critic, checker.

**Fixer**:
A reactive Role that addresses review feedback on a Pull Request.
_Avoid_: patcher, responder.

**Merge Gatekeeper**:
A reactive, agent-free policy Role that re-fetches current Pull Request state and writes an observe-only **Gate report**. It never reviews code, repairs a Pull Request, resolves comments, or merges.
_Avoid_: merger, reviewer, fixer.

**Coordinator**:
A proactive, LLM-driven Role for the legacy label-mediated intake path that performs Triage on fresh Issues and executes Dispatch. In Network mode, Coordinator is also the control plane for Issue admission, PR review assignment, and exact Node targeting, gated by the Network Lease. The internal Triager stands down while Coordinator is enabled for a Project so the two intake authorities cannot race.
_Avoid_: manager, commander, maintainer.

### Issue lifecycle

**Triage**:
The act of forming an opinion about a fresh Issue. In the personal GitHub path,
Triager persists the opinion as a Triage Report. In the legacy
Coordinator path, Coordinator applies classification labels, posts a triage
comment, and commits a Disposition. Each path deduplicates by its own durable
authority, and they do not run concurrently for one Project.
_Avoid_: assessment.

**Triage Report**:
Triager's durable structured record containing classification, scope, risk,
confidence, missing information, recommended next Role, rationale, the source
Issue event, idempotency key, and policy outcome. It is stored as a
`triage.report` event and is the semantic Authority for automatic Planner
routing. GitHub labels may project its outcome but cannot replace it.
_Avoid_: routing label, inferred issue state.

**Triage enrollment**:
Triager's durable record that a specific new/reopened source event entered the
workflow before any LLM call. `triage.enrolled` provides retry identity across
agent outages and source-lookback expiry; it does not authorize Planner.
`triage.routed` acknowledges an accepted projection, while `triage.retired`
settles a source that closed or was superseded.
_Avoid_: routing authority, report.

**Triage routing**:
The projection of an accepted Triage Report into Planner's durable loop and
queue without consulting Planner's label/assignee discovery filters. This is
distinct from Coordinator Dispatch, which remains the label-mediated action
defined below. A report held by policy may instead be authorized by a later
`triage.confirmed` record derived from an exact `/plan` comment by a repository
collaborator with write access.
_Avoid_: Dispatch (reserved for Coordinator).

**Disposition**:
Coordinator's high-level conclusion about an Issue. One of `valid`, `out-of-scope`, `unclear`. Distinct from `kind`/`area`/`complexity`, which are classification labels applied only when the Disposition is `valid`.
_Avoid_: verdict, outcome, status.

**Dispatch**:
The act of putting an Issue into a state where Planner or Worker will discover it: applying the role's trigger label and assigning the configured user. Performed by Coordinator either on human slash-command (human-gated mode) or autonomously after a grace window (autonomous mode).
_Avoid_: handoff (overloaded — see below), route, promote, enqueue.

**Trigger label**:
The label a reactive Role watches for to claim an Issue or Pull Request. Configured per Role under `roles.<role>.triggers.labels`; the defaults are `labels.DefaultPlanTrigger` and `labels.DefaultWorkerReadyTrigger` in `internal/labels`, the single definition point for role trigger defaults, spec PR lifecycle labels, and holds. Target labels live in `internal/network/protocol` because forming one requires validating a Node name, and comment markers are a separate mechanism kept with the protocol that emits them. Runtime discovery must read the configured value, not the default.
_Avoid_: queue label, pickup label, routed label, dispatched label, target label.

**Veto signal**:
A human-applied state on an Issue that blocks Coordinator's autonomous Dispatch. Examples: removing the `dispatch/*` label, applying `looper:hold`, or applying the trigger label manually.

**Blocker**:
An Issue listed in another Issue's GitHub-native `blocked_by` set. The Blocker's `state` and `state_reason`, together with `blocked_by` itself, are the named **Authority** for the dependency gate.

**Dependency gate**:
The **Dispatch** precondition that all **Blockers** be `state==closed AND state_reason==completed`. The gate is blocked when any Blocker is open or closed-not-completed, and released when every Blocker satisfies the condition.

**Ready set**:
The subset of tracked Issues whose **Dependency gate** is currently released — the Issues that may be **Dispatched** this tick, subject to the existing PRD #334 conditions.

**Reproduction Record**:
Reproducer's durable structured record containing the reproduction command, the
reproduction test file paths with their content hashes, the reproduction commit
SHA, the base SHA the command was observed failing on, the **Expected failure
signature**, and the idempotency key. It is stored as a `reproduction.recorded`
event and is the Authority for "this bug is reproduced". A copy travels with the
branch as the committed **Reproduction manifest**; the record, not the copy, is
what the completion gate is checked against. The record is scoped to the Triage
Report that authorized it through the idempotency key: a record, **Cannot
reproduce**, or waiver settles only its own report, so a report minted from a
superseding comment or edit is attempted afresh.
_Avoid_: failing test, red test, repro.

**Expected failure signature**:
The structured pair — a test identifier and a single-line failure message — that
names *which* failure the reproduction claims. A non-zero exit alone is not
proof: a syntax error, a failed setup step, or an unrelated already-failing test
all exit non-zero, and repairing any of those would turn the command green
without fixing the bug. Reproducer accepts a candidate only when the test
identifier appears in a declared reproduction file's content *and* both halves
appear in the observed command output. Both halves are bounded and single-line,
because the signature is the only part of the command's output persisted into
the committed manifest.
_Avoid_: observed failure, error message, output.

**Reproduction manifest**:
The reproduction.json file under the branch's .looper directory, committed in the reproduction commit. It
carries the Reproduction Record with the branch so Planner, Worker, and Fixer
receive the reproduction's identity as explicit input rather than re-deriving it
from the diff. The reproduction commit's changed-file set is exactly the declared
reproduction files plus the manifest; a commit that swept in undeclared work, or
that omitted a declared file, is not recorded as the Authority. It carries the
**Expected failure signature**, never raw command output.
_Avoid_: sentinel, ask file.

**Reproduction gate**:
The completion precondition, additional to repository validation, that the
recorded reproduction command passes and every recorded reproduction file still
hashes to its recorded content. Worker and Fixer both enforce it, and Fixer
resolves the governing Issue onto its loop before its agent runs, so deleting the
manifest is detected as tampering rather than being a way out. A hash mismatch or
a missing file fails the run with a distinct, non-generic reason rather than as
an ordinary validation failure. Only a genuine non-zero exit is a verdict:
timeouts, cancellations, and containment failures are retryable command errors,
matching the existing validation failure policy.
_Avoid_: validation gate, suite gate.

**Cannot reproduce**:
Reproducer's decision that the reported bug could not be made to fail, recorded
as a `reproduction.unreproducible` event with what was attempted, what was
observed instead, and what information is missing. It is a decision, not a
failure: it increments no attempts, trips no retry, and parks the Planner loop
in `awaiting_human` with a HITL ask, on a worktree first returned to its
committed state so a later waiver cannot publish the sentinel or the agent's
unadopted experiments. A human answer either waives the reproduction
(`reproduction.waived`) or leaves the Issue stopped — the latter is a
non-resuming answer, so it settles the ask without requeueing Planner. A record
the agent wrote with an unsupported version or no actionable detail still
escalates, but says so instead of parking the Issue with nothing to act on. It
is a verdict only when the command genuinely ran: a timed-out, cancelled, or
contained proof leaves the attempt open for the next tick.
_Avoid_: reproduction failure, crash.

**Acceptance criterion**:
A checkbox item under an Issue's `## Acceptance criteria` section. Reviewer's auto-merge gate verifies each criterion has a satisfying-evidence pointer in the diff before submitting APPROVE.

**Auto-merge scope**:
The Looper-only constraint identifying which PRs Looper may opt into auto-merge: `looper:` label AND tracked-Issue link, both required. Encoded in `roles.reviewer.autoMerge.scope = "looper-only"`.

**Merge-pending state**:
The GitHub-native state of a Pull Request after `gh pr merge --auto` has been called and before GitHub merges or a **Veto signal** arrives. The PR's `auto_merge` field is non-null in this state. Coordinator's merge-watch classifies merge-pending PRs into WatchActions.

**Watch marker**:
The `<!-- looper:coordinator:merge-watch retries=N -->` HTML-comment marker Coordinator places on the linked Issue rather than the PR, keeping Coordinator's state rooted on the Issue, to carry merge-watch retry-counter state across ticks. Public, durable, idempotent — preserves ADR-0001's stateless property.

**Gate report**:
The durable `pull_request.merge_gate.evaluated` event written by Merge Gatekeeper. It records `eligible` or `blocked`, stable reasons and evidence, and the observed head SHA. It is audit evidence, not merge authority: a future merge path must rerun every gate immediately before merging because holds, reviews, threads, and Project policy can change without moving the head.

### Authority and statelessness

**Authority**:
For any side-effecting action, the named, durable, structured signal that justifies the action. Per `AGENTS.md`: "What is the authority for this action, and why is it not the agent's own structured output?" Coordinator's authority for Dispatch is the durable `dispatch/*` label on the Issue, which is the agent's structured output committed to GitHub.
Reproducer's authority for "this bug is reproduced" is the persisted
Reproduction Record — command, commit SHA, file hashes — written before Planner
is reached and verified by actually running the command, not the agent's claim
that it wrote a test.
Triager's authority for Triage routing is the persisted Triage Report; the
policy outcome is stored before Planner projection and replayed after partial
failures. When policy requires a human, the report plus its persisted
write-authorized confirmation is the Authority.

**Stateless Role**:
A Role whose memory lives entirely in GitHub (labels, comments with markers, event timeline). It owns no private database tables. Coordinator is stateless. Worker, Planner, Reviewer, and Fixer are not — they persist runs in the local SQLite database.

### Comment markers

**Stamp**:
The standard `<!-- looper:stamp v=1 -->` HTML comment plus visible footer applied by every agent-authored comment, identifying the comment as Looper-generated. Defined in `internal/disclosure/disclosure.go`.

**Self-dedup marker**:
A Role-specific HTML comment marker (e.g. `<!-- looper:coordinator:triage -->`) used by a stateless Role to recognise its own prior comments and avoid duplicate posts.

### Network

**Network**:
A coordinated set of `looperd` instances that share Coordinator admission/assignment decisions for a configured set of repositories. A Node joins exactly one Network at a time. Hosted by a `loopernet` instance (one Network per `loopernet`).

**Node**:
A single `looperd` instance enrolled in a Network. Identified by an opaque cloud-issued ID and a human-readable Name (short label-safe string; convention is to use a color, e.g. `red`, `blue`, `cyan`).
_Avoid_: peer, member, instance, agent.

**Coordinator control plane**:
The Network-aware Coordinator responsibility that decides Issue admission and PR review assignment, then applies the GitHub state that Worker/Reviewer consume. In Routed projects it also applies an exact target label (`looper:target:<node_name>`) so a specific Node can claim the work.
_Avoid_: router, dispatcher, scheduler, balancer.

**Routed project**:
A project whose `network.mode` is `routed`. Coordinator admission/assignment is performed by the current Network Lease holder. Worker/Reviewer claim only when the exact target label matches the local Node and the role-specific GitHub-native coarse target is present. The complement is a *local-only project*, whose Roles keep existing single-machine behaviour and ignore `looper:target:*` labels.

**Target label**:
Constructed and parsed by `protocol.TargetLabelForNode` and `protocol.ParseTargetLabel` in `internal/network/protocol`, which is where they live because forming one requires validating a Node name. Exactly one valid target label must be present before a Routed Worker/Reviewer may claim; target labels are ignored in local-only projects.
_Avoid_: trigger label, routed label, worker-ready suffix.

**Lease**:
The durable Authority for Network Coordinator control-plane leadership. A row in the `loopernet` database with a fencing token, validated at every GitHub side-effect boundary.

### Testing

**Live sandbox**:
A dedicated remote repository on a real Provider used for live end-to-end tests. It is isolated from product and developer repositories, but still performs real provider mutations.
_Avoid_: local sandbox, mock sandbox.

## Relationships

- A **Triager** performs **Triage** on a new or reopened GitHub **Issue**, producing a persisted **Triage Report**
- An accepted low-risk **Triage Report** authorizes **Triage routing** directly to **Planner**
- A bug-classified **Triage Report** authorizes **Reproducer**, and **Triage routing** to **Planner** is withheld until a **Reproduction Record** or a `reproduction.waived` record scoped to *that report* exists
- Both of **Planner**'s doors consult the same report-aware gate: **Triage routing** and **Planner**'s own label/assignee discovery. An Issue with no accepted bug report is unaffected by either
- The **Reproducer** lane runs after **Triager** and before **Planner** regardless of authored `roles.coding.*.priority` values, since the ordering is what makes "reproduce before planning" true rather than incidental
- A **Reproducer** produces either a **Reproduction Record** plus a reproduction commit on the branch **Planner** will adopt, or a **Cannot reproduce** decision that parks the Planner loop for a human on a worktree returned to its committed state
- **Planner**, **Worker**, and **Fixer** read the **Reproduction manifest** from their worktree as explicit input; **Worker** and **Fixer** additionally enforce the **Reproduction gate** on top of unchanged repository validation
- A **Coordinator** performs label-mediated **Triage** on a fresh **Issue**, producing a **Disposition** plus classification labels
- A **Coordinator** performs **Dispatch** on a Triaged Issue, producing a **Trigger label** that a **Planner** or **Worker** observes
- A **Coordinator** may perform **PR review assignment**, producing a GitHub review request that **Reviewer** observes
- A **Coordinator** consults the **Dependency gate** before performing **Dispatch** when `roles.coordinator.dependencies.enabled = true`
- **Reviewer** opts approved code PRs (carrying **Auto-merge scope**) into GitHub-native auto-merge after verifying each **Acceptance criterion** has satisfying-evidence in the diff
- **Merge Gatekeeper** evaluates every active GitHub-backed Project Pull Request from source discovery, then re-evaluates after head, check, review, thread, or hold changes and writes a head-bound **Gate report**
- **Coordinator**'s per-tick poll classifies **Merge-pending state** PRs into WatchActions, routing mechanical failures (conflict, red CI) to **Fixer** via **Trigger label** and policy failures (branch protection change) to re-Triage by removing the Issue's `triaged` and `dispatch/*` labels
- The **Watch marker** carries merge-watch retry state on the linked Issue, preserving Coordinator's stateless property
- A **Veto signal** from a human overrides Coordinator's autonomous Dispatch but does not override **Triage** itself
- In a **Routed project**, the **Coordinator control plane** applies GitHub-native coarse authority (`looper:worker-ready` plus assignee for Worker, review request for Reviewer) and writes the **Target label** last. The **Lease** gates Coordinator control-plane action; current GitHub issue/PR state remains the claim Authority.

## Flagged ambiguities

- **classification** — used by humans to mean both Disposition and the kind/area labels. Resolved: Disposition is the high-level conclusion (`valid` / `out-of-scope` / `unclear`); kind/area/complexity are classification *labels* applied during a `valid` Triage. The unqualified word "classification" is avoided in favor of "Disposition" or "label".
- **handoff** — already used in code (`authoritative handoff fields`) for the PR-seed contract between Reviewer and Fixer. Not used for Coordinator's Dispatch action, which is a different concept. Use "Dispatch" exclusively for the Coordinator action.
- **manager / commander / maintainer** — early names considered for the Coordinator Role. Rejected: "manager" implies it directs other Roles (it doesn't, it sets labels), "commander" overpromises authority, "maintainer" is a human role.

## Example dialogue

> **Dev:** When a fresh **Issue** arrives, what does **Coordinator** do?
> **Domain expert:** It performs **Triage**: it reads the Issue, decides a **Disposition**, and if `valid` applies kind/area/complexity/dispatch labels and posts a triage comment. The `triaged` label is applied last as the durability commit.
>
> **Dev:** And then a **Planner** picks it up?
> **Domain expert:** Not directly. **Coordinator** later performs **Dispatch** — applies the planner's **Trigger label** and assigns the user. **Planner** then discovers it on its normal trigger.
>
> **Dev:** Why two steps?
> **Domain expert:** Triage produces structured output. Dispatch consumes it. Splitting them gives humans a veto window between the two — they can remove `dispatch/needs-plan` if they disagree.
>
> **Dev:** Where do dependencies fit?
> **Domain expert:** Before **Dispatch**, **Coordinator** consults the **Dependency gate**. If any **Blocker** in `blocked_by` is still open or was closed as anything other than `completed`, the Issue stays out of the **Ready set** until the gate releases.
>
> **Dev:** What if a human just types `/plan` instead of waiting?
> **Domain expert:** Then **Coordinator** dispatches immediately unless the **Dependency gate** is blocked. Human-gated mode is the default; autonomous mode requires the grace window. Either way the **Authority** for dispatch is the durable label on the **Issue**, never an in-memory decision.
>
> **Dev:** What happens after **Reviewer** APPROVEs a Looper PR?
> **Domain expert:** If **Auto-merge scope** matches and the linked Issue has stated **Acceptance criterion**s, **Reviewer** verifies each criterion against the diff. On all-pass, it submits APPROVE with per-criterion evidence and calls `gh pr merge --auto`. **GitHub branch protection** is the named **Authority** for "safe to merge" — Looper does not check CI itself. **Coordinator**'s per-tick poll then watches the **Merge-pending state** PR and classifies it into WatchActions; the **Watch marker** on the linked Issue carries retry-counter state without private storage.
