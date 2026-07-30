# DESIGN — one-command human takeover of a loop's agent session

Status: **DONE + e2e-verified** (branch `feat/human-takeover`). Verified end to
end against a live daemon:
- `looper resume <seq>` parks the loop (stays `human_takeover` across ticks — the
  scheduler race guard released a stray claimed item), returns the exact
  `cd <worktree> && codex resume <sid>` command, and kills the run.
- `looper handback <seq>` re-arms it (clearing any queue item that survived the
  takeover race) and the daemon's next run resumes the SAME session:
  `codex exec … resume <human-sid> "A human … continue from where they left off"`
  — so the daemon sees the human's turns, not a fresh session.

Requires `agent.nativeResume.enabled: true` for the daemon-side handback resume
(the human's own takeover has full conversation continuity regardless).

Owner-facing goal:

> A looper owner, from a Feishu thread, copies **one command** into their own
> terminal and lands *inside the exact agent session* for that task — same
> context, same worktree, same conversation — drives it by hand, then hands it
> back so the daemon keeps following up **seeing everything the human did**.

## Why this works — verified resume semantics (2026-07, codex 0.142.4 / claude 2.1)

The whole design rests on one property: **resuming an agent session preserves
the session id and appends to the same conversation.** Measured with a 3-turn
probe (seed a fact → resume + add a fact + recall the first → resume + recall
both):

| vendor | session id across resumes | history accumulates | verdict |
|---|---|---|---|
| **codex** (`codex [exec] resume <id>`) | stable (same UUID all 3 turns) | yes — one on-disk rollout file per id, grows each turn | **verified** |
| **claude** (`claude --resume <id>`) | stable (same UUID all 3 turns) | yes — recalled both facts | **verified** |
| opencode (`opencode run -s <id>`, `--fork` opt-in) | design defaults to continue | not run — local DB error in test env | **gated off until verified** |

Consequence: **no new-session-id capture is needed on handback.** looper already
stores the loop's session id and resumes it headlessly (`nativeResume`,
`executor.go`). The human resumes that *same* id; the daemon's next
native-resume then loads the human's turns for free.

Interactive path (the human's TUI) shares the same on-disk session store as the
headless `exec` path (confirmed: codex keeps one `~/.codex/sessions/.../rollout-*-<id>.jsonl`
per id), so `codex resume <id>` / `claude --resume <id>` write to the very file
the daemon later reads.

## Flow

```
Feishu anchor:  💻 本地接管 → looper resume 153
                     │
   looper resume 153 │  (on the daemon host — worktree + session live there)
                     ▼
   • stop the daemon's in-flight run for the loop  (Kill; session id already on disk, preserved)
   • loop.status → human_takeover                  (scheduler stops claiming it)
   • pin the worktree                              (cleanup must not reclaim it)
   • exec: cd <worktree> && codex resume <sid>     (human is now IN the session)
                     │  …human drives by hand, exits…
                     ▼
   looper handback 153
   • loop.status → queued
   • daemon's next run native-resumes <sid> → sees the human's turns, continues follow-up
```

## Build phases

- **Phase 0 — foundation (DONE).** Verified resume semantics (above).
  `agent.InteractiveResumeCommandLine(cfg, worktree, sid)` +
  `agent.InteractiveTakeoverSupported(vendor)` — pure, vendor-aware, unit-tested;
  codex/claude enabled, opencode/cursor gated. (`internal/agent/executor.go`,
  `internal/agent/takeover_test.go`.)
- **Phase 1 — status + state machine.** Add `domain.LoopStatusHumanTakeover`
  (`internal/domain/domain.go`) and the allowed transitions in
  `AssertLoopStatusTransition`: `running|awaiting_human → human_takeover`,
  `human_takeover → queued|running|terminated`. Scheduler must not claim
  `human_takeover` loops.
- **Phase 2 — daemon endpoints.** `POST /api/v1/loops/{seq}/takeover` (stop the
  in-flight run via the ActiveExecutionRegistry, set status, return
  `{worktreePath, sessionId, vendor}`) and `POST …/handback` (status → queued),
  modelled on the existing `…/respond` endpoint.
- **Phase 3 — CLI.** `looper resume <seq>` (cobra subcommand in
  `internal/cliapp`): call `/takeover`, resolve the command via
  `InteractiveResumeCommandLine`, `exec` it (or `--print` to just show it);
  `looper handback <seq>` calls `/handback`. Gate on
  `InteractiveTakeoverSupported`; refuse with a clear message otherwise.
- **Phase 4 — worktree retention.** `internal/runtime/worktree_cleanup.go` must
  skip worktrees whose loop is `human_takeover` (else the session's working tree
  is reclaimed under the human).
- **Phase 5 — surface it.** Anchor card renders `💻 本地接管:looper resume <seq>`
  while the loop is active/awaiting (feishuThreadHeaderCard), and a note that it
  runs on the daemon host.

## Constraints (called out, not solved away)

1. **Daemon-host only.** The worktree and the session files live on the machine
   the daemon ran on. In the per-person model that's the owner's machine — fine;
   surface a clear error if the resume command is run elsewhere (session/worktree
   absent).
2. **Retention window.** Worktrees are reclaimed by `worktree_cleanup.go`; Phase 4
   pins `human_takeover` loops, but a long-idle takeover offer whose loop already
   finished + got cleaned can't be resumed. Only advertise the command while the
   loop is live, and drop it once terminal.
3. **One driver at a time.** Takeover must stop the daemon's run first (Phase 2)
   so the human and the daemon never drive the same session concurrently.
4. **Vendor gating.** Only codex/claude offer takeover until opencode/cursor pass
   the same 3-turn resume check.

## What the hold actually covers

`human_takeover` is a status on one loop. "A human owns this target" is a claim
about a shared filesystem resource — the detached worktree — enforced through
that one loop's status, and the two are not the same thing. State the difference
rather than letting the wording imply the stronger one.

Guaranteed from the moment takeover commits, until `looper handback`:

- **No new claim** is granted on the held loop, or on any other loop sharing its
  checkout — same project + repo + PR number for a PR target; same project +
  target_id for an issue target, among planner loops only. Two target types are
  *not* shared checkouts, and each fences only the held loop:
  - A **project** target: project workers run concurrently by design, each on its
    own branch.
  - A **non-planner issue** loop: planner is the only role whose issue branch is
    issue-derived (`looper/planner/<issue>-<slug>`), so a replacement planner loop
    on the same issue does land on the held checkout. A worker's branch carries
    its own loop hash (`looper/<issue>-<slug>-<loopHash>`), so a worker on the same
    issue never shares the planner's checkout or another worker's. The ordinary
    issue lock serialises the roles while one is running, and takeover cancels the
    held loop's queue item, so no shared resource survives the hold.
- **The selected loop's in-flight run is stopped**, and spawn admission for it
  stays closed.
- **Its worktree is kept out of cleanup**: worktree cleanup skips it, and the
  fixer's terminal-cleanup paths re-read the durable hold before removing a
  checkout.
- **No blind write may take or release the hold.** Only `looper takeover` creates
  one and only `looper handback` (or an explicit stop/terminate) releases it;
  every other write to a held loop is refused inside the writing statement.

Not guaranteed:

- **A sibling loop already claimed and running on the same checkout keeps
  running.** The claim boundary refuses new claims; it cannot revoke one already
  granted, and takeover stops only the loop it was given.
- **`looper retry --discard-worktree-changes` can still reset the checkout.** It
  does not share takeover's target mutex, so it can reach the filesystem before
  the guard refuses its status write — and a filesystem mutation cannot be
  undone by refusing a database write afterwards.

Both gaps need a target/worktree-level lease rather than a per-loop status, which
is the redesign this section exists to scope. Until then the takeover response
names them, because the incident that produced all of this was a control
reporting more than it did.
