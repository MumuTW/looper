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
Defined at `triager.ReportEventType` in `internal/triager`, whose doc comment
carries the semantics: Triager's durable structured record, stored as a
`triage.report` event, and the semantic Authority for automatic Planner
routing.
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
Defined at `internal/coordinator/triage.Disposition`, whose doc comment carries
the semantics: the high-level conclusion (`valid` / `out-of-scope` / `unclear`),
distinct from the classification labels applied only when `valid`.
_Avoid_: verdict, outcome, status.

**Dispatch**:
The act of putting an Issue into a state where Planner or Worker will discover it: applying the role's trigger label and assigning the configured user. Performed by Coordinator either on human slash-command (human-gated mode) or autonomously after a grace window (autonomous mode).
_Avoid_: handoff (overloaded — see below), route, promote, enqueue.

**Trigger label**:
The label a reactive Role watches for to claim an Issue or Pull Request. Configured per Role under `roles.<role>.triggers.labels`; the defaults are `labels.DefaultPlanTrigger` and `labels.DefaultWorkerReadyTrigger` in `internal/labels`, the single definition point for role trigger defaults, spec PR lifecycle labels, and holds. Target labels live in `internal/network/protocol` because forming one requires validating a Node name, and comment markers are a separate mechanism kept with the protocol that emits them. Runtime discovery must read the configured value, not the default.
_Avoid_: queue label, pickup label, routed label, dispatched label, target label.

**Veto signal**:
A human-applied state on an Issue that blocks Coordinator's autonomous Dispatch. Examples: removing the `dispatch/*` label, applying `looper:hold`, or applying the trigger label manually. (Prose-only: the signal is human GitHub state, not a Looper type.)

**Blocker**:
Defined at `internal/coordinator/depgraph.Blocker`, whose doc comment carries
the semantics: an Issue in another Issue's GitHub-native `blocked_by` set, the
named **Authority** for the dependency gate.

**Dependency gate**:
Defined at `internal/coordinator/depgraph.DependencyGraph`, whose doc comment
carries the semantics: the **Dispatch** precondition over **Blockers**.

**Ready set**:
Defined at `internal/coordinator/depgraph.DependencyGraph` (the `ReadySet`
method), whose doc comment carries the semantics: the tracked Issues whose
**Dependency gate** is currently released.

**Acceptance criterion**:
A checkbox item under an Issue's `## Acceptance criteria` section. Reviewer's auto-merge gate verifies each criterion has a satisfying-evidence pointer in the diff before submitting APPROVE.

**Auto-merge scope**:
The Looper-only constraint identifying which PRs Looper may opt into auto-merge: `looper:` label AND tracked-Issue link, both required. Encoded in `roles.reviewer.autoMerge.scope = "looper-only"`.

**Merge-pending state**:
The GitHub-native state of a Pull Request after `gh pr merge --auto` has been called and before GitHub merges or a **Veto signal** arrives. The PR's `auto_merge` field is non-null in this state. Coordinator's merge-watch classifies merge-pending PRs into WatchActions. (Prose-only: a GitHub-native state; the classifier over it is `internal/coordinator/mergewatch.WatchAction`.)

**Watch marker**:
Defined at `internal/coordinator/mergewatch.PriorWatchMarker`, whose doc
comment carries the semantics: the Issue-rooted HTML-comment marker that
carries merge-watch retry-counter state across ticks.

**Gate report**:
Defined at `gatekeeper.GateReportEventType` in `internal/gatekeeper`, whose doc
comment carries the semantics: Merge Gatekeeper's durable evaluation event —
audit evidence, not merge authority.

### Authority and statelessness

**Authority**:
For any side-effecting action, the named, durable, structured signal that justifies the action. Per `AGENTS.md`: "What is the authority for this action, and why is it not the agent's own structured output?" Coordinator's authority for Dispatch is the durable `dispatch/*` label on the Issue, which is the agent's structured output committed to GitHub.
Triager's authority for Triage routing is the persisted Triage Report; the
policy outcome is stored before Planner projection and replayed after partial
failures. When policy requires a human, the report plus its persisted
write-authorized confirmation is the Authority.

**Stateless Role**:
A Role whose memory lives entirely in GitHub (labels, comments with markers, event timeline). It owns no private database tables. Coordinator is stateless. Worker, Planner, Reviewer, and Fixer are not — they persist runs in the local SQLite database.

### Comment markers

**Stamp**:
Defined at `disclosure.Stamper` in `internal/disclosure`, whose doc comment
carries the semantics: the standard `<!-- looper:stamp v=1 -->` HTML comment
plus visible footer on every agent-authored comment.

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
Defined at `internal/network/protocol.CoordinatorLease`, whose doc comment
carries the semantics: the durable Authority for Network Coordinator
control-plane leadership, fencing-token validated at every GitHub side-effect
boundary.

### Testing

**Live sandbox**:
A dedicated remote repository on a real Provider used for live end-to-end tests. It is isolated from product and developer repositories, but still performs real provider mutations.
_Avoid_: local sandbox, mock sandbox.

## Relationships

- A **Triager** performs **Triage** on a new or reopened GitHub **Issue**, producing a persisted **Triage Report**
- An accepted low-risk **Triage Report** authorizes **Triage routing** directly to **Planner**
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

- **Disposition (name collision)** — `criteria.AggregateDisposition` (pass/fail/unverifiable) and `depgraph`'s blocker disposition reuse the word for unrelated concepts; only `internal/coordinator/triage.Disposition` is the glossary's Disposition. Qualify on first use anywhere the packages meet.
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
