# Let agents create their own git commits, with looperd as fallback

## Problem

Today the primary commit path still belongs to `looperd`, especially in the Fixer flow. After the agent finishes editing files, `looperd` inspects the worktree and creates the commit itself when there are uncommitted changes. That keeps the push path reliable, but it has two drawbacks:

- commit messages are generated from generic loop-owned templates instead of the agent's actual repair context;
- the agent is implicitly discouraged from committing, because prompts say Looper will handle follow-up repository actions after the edits.

This means the system gets consistent pushes, but lower-quality commit history.

## Goals

- Make agent-authored commits the preferred outcome for Fixer work.
- Preserve `looperd` as the reliable authority for the final push path.
- Fall back to a `looperd`-authored commit when the agent leaves code changes but does not create a commit.
- Keep the change narrowly scoped to commit ownership and post-agent reconciliation.

## Non-goals

- Letting agents push branches or mutate remote review state directly.
- Redesigning the broader Fixer or Worker lifecycle.
- Changing existing protections around `allowAutoPush`, protected branches, or remote-head verification.

## Current state

### Fixer

- `apps/looperd/src/fixer/index.ts` runs `repair -> reconcile-commits -> validate -> push`.
- `reconcileCommits()` already inspects the git head before and after repair.
- If the worktree is dirty after repair and `allowAutoCommit` is enabled, Fixer always calls `git.commit(...)` with a loop-owned message.
- The checkpoint records whether commits already existed (`committedByAgent`) and whether `looperd` created a commit (`committedByLooperd`), but agent-created commits are treated as compatibility, not the primary path.
- The Fixer prompt currently says: "Avoid pushing branches or changing remote review state; Looper will handle follow-up repository actions after your edits." That wording biases the agent away from creating a local commit.

### Worker

- Worker already separates local execution from the final push/open-PR phase.
- Its prompt asks the agent to leave the branch ready, but it does not clearly state whether a local commit is expected or preferred.

## Proposed approach

### 1. Change the contract: agents may create a local commit, but may not push

Update agent-facing prompts so the allowed repository actions are explicit:

- the agent may run local git commands needed to inspect status and create a commit;
- the agent should create a commit when it finishes meaningful changes;
- the agent must not push, open PRs, or change remote review state;
- if the agent decides no code changes are needed, it may leave the branch without a new commit.

For Fixer, this becomes the primary expectation instead of an incidental capability.

### 2. Keep `looperd`'s post-agent git inspection as the source of truth

Do not trust agent self-reporting alone. After agent execution, `looperd` should continue to inspect the worktree and compare it with the pre-repair base head.

The post-repair reconciliation should distinguish three states:

1. **Agent created one or more commits and left the worktree clean**  
   Accept the agent-authored commit(s) and continue to validation/push.
2. **Agent changed files but created no commit**  
   Use the existing fallback path: `looperd` stages all changes, creates a commit, and proceeds.
3. **Agent created commit(s) but left additional uncommitted changes**  
   Treat this as an incomplete handoff. `looperd` should either:
   - create one fallback follow-up commit if `allowAutoCommit` is enabled; or
   - fail with manual intervention if auto-commit is disabled.

This preserves the existing reliability model while still preferring the agent's commit message whenever the agent completed the commit step.

### 3. Reframe `allowAutoCommit`

`allowAutoCommit` should no longer imply "the loop owns the primary commit step." Instead it should mean:

- `looperd` is allowed to repair an incomplete post-agent git state by creating the fallback commit when needed.

When `allowAutoCommit = false`:

- agent-created commits are still valid and should continue through the flow;
- dirty uncommitted changes after agent execution remain a manual-intervention failure.

This is the key behavior change needed to stop Fixer from depending on `looperd` for the primary commit path.

### 4. Keep push ownership in `looperd`

No change to final push authority:

- remote-head checks remain in `looperd`;
- protected-branch safeguards remain in `looperd`;
- `allowAutoPush` still gates network mutation;
- comment resolution and other remote side effects still happen only after validation/push succeeds.

The only ownership shift is who authors the local commit when the agent already did the work.

### 5. Tight implementation scope

Implement only the pieces needed for the new contract:

- Fixer prompt updates so local commits are clearly allowed and preferred;
- Worker prompt updates if needed for consistency, without changing Worker's push/open-PR responsibilities;
- reconciliation logic updates so agent-authored commits are the first-class success path;
- tests covering agent-commit, fallback-commit, and dirty-after-commit edge cases.

Do not introduce a new loop type, a new git phase, or agent-managed push behavior.

## Detailed behavior

### Fixer happy path

1. Prepare worktree at the expected PR head.
2. Agent performs repair changes.
3. Agent creates a local commit with its own message.
4. `looperd` inspects the head:
   - sees new commit(s);
   - sees no uncommitted changes.
5. `looperd` records that the agent committed and continues to validation and push.

### Fixer fallback path

1. Prepare worktree at the expected PR head.
2. Agent performs repair changes but does not commit.
3. `looperd` inspects the head:
   - sees no new commits;
   - sees uncommitted changes.
4. If `allowAutoCommit` is enabled, `looperd` creates the fallback commit and continues.
5. If `allowAutoCommit` is disabled, the run fails for manual intervention.

### Mixed path

1. Agent creates one or more commits.
2. Additional uncommitted changes still remain.
3. `looperd` treats the branch as not fully reconciled.
4. Fallback behavior follows `allowAutoCommit` as described above.

## Risks

- **Prompt ambiguity:** if the prompt says the agent may commit but does not make the boundary with push explicit, agents may attempt unsupported remote actions.
- **False success on dirty trees:** accepting an agent-created commit without re-checking for leftover modifications would weaken reliability.
- **Behavior drift across loops:** if Fixer and Worker prompts describe commit ownership differently, agent behavior will stay inconsistent.
- **Config confusion:** operators may still read `allowAutoCommit` as a primary-path toggle unless its semantics are reflected in code comments/tests and user-visible descriptions.

## Validation

- Unit tests for Fixer reconciliation when:
  - the agent created a commit and the worktree is clean;
  - the agent left changes uncommitted and fallback commit is enabled;
  - the agent left changes uncommitted and fallback commit is disabled;
  - the agent created commit(s) but also left extra uncommitted changes.
- Prompt-building tests verifying Fixer instructions allow local commits but forbid push/remote actions.
- If Worker prompt text is updated, add or adjust prompt tests there as well.
- Regression coverage confirming the later validation/push path still runs through `looperd` unchanged.

## Acceptance mapping

- **Fixer no longer depends on `looperd` for the primary commit step**  
  Achieved by making an agent-authored local commit the preferred success path.
- **Agents can create commits themselves with their own commit messages**  
  Achieved by explicit prompt contract changes plus reconciliation that accepts those commits.
- **`looperd` detects when no commit was created and performs the fallback commit/push flow**  
  Achieved by retaining post-agent git inspection and conditional fallback commit behavior.
- **The fallback path remains reliable and does not break existing automation**  
  Achieved by leaving validation, remote checks, push, and review-state mutations under `looperd` control.
