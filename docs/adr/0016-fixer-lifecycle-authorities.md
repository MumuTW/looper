# ADR-0016: Fixer lifecycle authorities and runner decomposition

**Status:** Proposed / Partially Implemented

**Numbering note:** ADR-0015 is the previous reserved number. This ADR documents
the phased decomposition of `internal/fixer/runner.go` and the authority model
for fixer HEAD/checkpoint fields.

## Context

`internal/fixer/runner.go` currently combines workflow transitions, checkpoint
recovery, Git reconciliation, validation, publishing, GitHub mutations, evidence
recording, and failure policy in one module. The concrete risk is not file
length; it is that lifecycle invariants cross many functions and authorities:

current Git HEAD, validation checkpoint HEAD, reconcile final HEAD, PR detail
HEAD, push evidence HEAD, and resume policy / pause state.

PR #1 required repeated fixes because locally reasonable changes did not preserve
behavior across the complete repair → reconcile → validate → resume → push
lifecycle.

## Decision

Decompose `internal/fixer/runner.go` around deep module boundaries where each
lifecycle rule has **one authority** and a narrow input/output contract. This
ADR is the contract; the PRs that implement it move behavior one boundary at a
time.

The five proposed boundaries are:

1. **Workflow / state transitions** — owns allowed step transitions and
   checkpoint resume behavior.
2. **Commit reconciliation** — owns dirty-worktree inspection and commit
   creation.
3. **Validation** — owns command execution and the validated local HEAD.
4. **Publishing** — owns the validated-HEAD precondition, push/open-PR
   behavior, and remote observation.
5. **Failure policy** — maps structured failures to retry, restart, or manual
   intervention.

This slice (the one that introduces this ADR) extracts the **local worktree
usability** portion of the commit-reconciliation boundary into
`internal/worktreesafety` and the **fixer failure-policy** boundary into
`internal/fixer/failurepolicy`.

## Authority

The authority for a fixer lifecycle field is the component that can observe the
thing directly, not an agent's structured output or an inference from another
layer:

| Field | Authority | Source | Why it is the authority |
|-------|-----------|--------|------------------------|
| current Git HEAD | Local worktree | `git.InspectHead` | Only the local checkout can see uncommitted changes and new commit objects. Remote HEAD is evidence, not authority. |
| validation checkpoint HEAD | Validation boundary | `ValidationResult.HeadSHA` after validation commands and a follow-up `InspectHead` | Validation may produce new commits; the head after validation is the authority, not the command output. |
| reconcile final HEAD | Commit reconciliation boundary | `InspectHeadResult.HeadSHA` after `reconcileCommits` | Reconcile owns dirty→clean and new-commit detection. |
| PR detail HEAD | GitHub / forge gateway | `PullRequestDetail.HeadSHA` from `ViewPullRequest` | The remote PR head is the target for fetch/push and conflict detection. |
| push evidence HEAD | Publishing boundary | `InspectHeadResult.HeadSHA` before/after push plus remote `CompareCommits` | Push must start from the validated head; remote observation confirms reachability. |
| resume policy / pause state | Failure policy boundary | `failurepolicy.Decision` + `loops.NormalizeResumePolicy` | Structured failure kind and existing checkpoint policy, not ad-hoc output keyword parsing. |

## Lifecycle characterization matrix

The matrix below maps each fixer step to its normal and resumed behavior, its
precondition, and the owning authority.

| Step | Normal execution | Resumed execution | Required precondition | Authority |
|------|------------------|-------------------|-----------------------|-----------|
| discover-pr | Start here; list/view PRs and create/refresh loop+queue. | n/a (entry point) | project and repo configured | GitHub API |
| claim-pr | After discover; acquire PR lock and load detail. | n/a | PR eligible and not locked | GitHub API + project policy |
| collect-fixes | After claim; collect review items and native comments. | n/a | PR detail loaded | GitHub API |
| prepare-worktree | After collect-fixes; create or adopt a worktree. | Start here if `Worktree.PreparedAt` is missing and `Worktree.Path` is present. | worktree path is safe and worktree is usable | `worktreesafety` + Git gateway |
| repair | After prepare; run fixer agent. | After prepare; resume requires `Repair` completion only for steps after repair. | agent execution can start | model provider |
| reconcile-commits | After repair; auto-commit uncommitted changes. | After repair; skip if `ReconcileCommits.CompletedAt` is set. | worktree may have uncommitted changes | commit reconciliation (Git gateway) |
| validate | After reconcile; run validation commands. | After reconcile; skip if `Validation` passed. | worktree HEAD is stable | validation boundary (`validationcmd`) |
| push | After validate; push local head. | After validate; skip if `Push.Pushed`. | validation passed and `allowAutoPush` | publishing boundary (Git + GitHub) |
| resolve-comments | After push; reply/resolve review threads. | After push; skip if `ResolvedComments` recorded. | push evidence matches head | GitHub API |
| recheck | After resolve; verify remaining items. | After resolve; skip if `Recheck` recorded. | resolved state stable | GitHub API |

`validateFixerResumeCheckpoint` enforces that resumed runs starting at
`reconcile-commits`, `validate`, `push`, `resolve-comments`, or `recheck` must
have a completed `Repair` checkpoint, because those steps assume the agent has
already produced a result.

## Trade-off

**Prevents:**

- Remote helper stderr being treated as local git checkout authority.
- Validation output keyword parsing leaking into step orchestration.
- Resume policy being inferred from fragmented `loopError` construction.
- One change to a step silently breaking invariants in a later step.

**Costs:**

- New packages and short-term adapter functions in `runner.go`.
- Contract-level tests for each boundary.
- Tighter discipline when adding new HEAD fields: each field must be assigned
  an explicit authority.

**Why simpler alternatives are insufficient:**

- Splitting `runner.go` into multiple files in the same package would keep the
  same shared `fixerCheckpoint` state and would not clarify authority.
- Trusting agent output for lifecycle state (e.g. `git_pr_lifecycle`) is
  insufficient: agents can hallucinate branch names and push status. Infra
  signals are for drift detection, not authority.
- Adding more inline guards in `runner.go` is insufficient: the same problem
  that produced the repeated PR #1 fixes would recur.

**Deletion attempt:**

A broad rewrite that moves most of `runner.go` in one PR was rejected because it
would introduce more regressions than it removes. The safe path is
characterization tests and design docs first, then one boundary at a time.

## Consequences

This slice implements the following consequences:

1. **Worktree usability authority** moves to `internal/worktreesafety`.
   - `IsMissingOrUnusableFixerWorktree`, `LocalFixerWorktreeCheckoutUsable`,
     `ClearUnusableFixerWorktreePath`, and `LocalGitRepositoryMetadataUsable`
     now live in the worktree-safety package with their own contract tests.
   - `runner.go` no longer performs Git output keyword inference for local
     worktree integrity; it calls the worktree-safety boundary.

2. **Fixer failure-policy authority** moves to `internal/fixer/failurepolicy`.
   - `ClassifyError`, `ClassifyValidation`, and `BoundaryForStep` live in the
     failure-policy package with contract tests.
   - `runner.go` no longer classifies validation failures by substring-matching
     the summary; it delegates to `failurepolicy.ClassifyValidation`.

3. Each moved boundary keeps the same observable behavior as before. Tests that
   previously exercised the unexported helpers in `package fixer` now call the
   exported worktree-safety functions or the failure-policy package.

## Future slices

- Extract the full commit-reconciliation step (`reconcileCommits`) into a
  boundary that owns dirty-worktree inspection and commit creation.
- Extract the validation step contract into a validation boundary separate from
  the runner.
- Extract publishing (push + open-PR + remote observation) into a publishing
  boundary.
- Extract workflow/state transitions and resume logic into a workflow boundary.

Each future slice must update this ADR's enforcement matrix and add contract
 tests before changing `runner.go`.
