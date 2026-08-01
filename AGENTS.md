# AGENTS.md

## Commands

- Supported implementation is Go-first. Root commands are the source of truth:
  - `go run ./cmd/looperd`
  - `go run ./cmd/looper <args>`
  - `go build ./...`
  - `go vet ./...`
  - `go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 -tests=false -checks='U1000,SA1006,SA4004,SA4006' ./...`
  - `go test ./...`

## Repo structure

- `cmd/looperd` — supported `looperd` daemon entrypoint.
- `cmd/looper` — supported `looper` CLI entrypoint.
- `internal/` and `pkg/` — active Go implementation packages.

## Configuration & runtime

- Default daemon config path: `~/.looper/config.toml` — what `DiscoverDefaultConfigPath` selects when `~/.looper` holds no config, and what `looper init` writes. `.yaml`, `.yml`, and `.json` in `~/.looper` are still loaded if present; a legacy `~/.looper/config.json` loads with a migration notice.
- Precedence: defaults → config file → env → CLI flags.
- looperd fails fast on config-validation errors and requires writable runtime paths.
- Tool paths (`git`, `gh`, `osascript`) are auto-detected unless explicitly configured.
- When `notifications.osascript.enabled` is true, `osascript` must resolve or startup fails.
- Default runtime artifacts: `~/.looper/` (`looper.sqlite`, `backups/`, `logs/`).
- Worker/Fixer validation is project-owned: configure `projects[].validation.commands`, explicitly set `projects[].validation.optOut = true`, or temporarily inherit deprecated `defaults.validationCommands` during migration. Missing project policy fails closed when those coding roles are configured.

## Conventions

- Build output lives in `dist/`; do not edit generated files.
- CI (`.github/workflows/ci.yml`) `verify` runs on PR updates: dashboard (`pnpm install`/`test`/`build` + artifact checks) → `gofmt -l .` → `go vet ./...` → production-only staticcheck → `go test ./...` → `go build ./...`.
- Commit messages and PR titles must use semantic prefixes, for example `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, or `ci:`.

## Review guidelines

- Report every issue found. Do not prioritize, triage, or omit.
- Continue reviewing after finding issues. Early termination is a defect.
- Review systematically across correctness, performance, maintainability, and style.
- Treat missing regression coverage for a P0/P1 bug fix as a review blocker.
- For cross-component lifecycle, worktree, GitHub command, daemon boot, and resolve-comments regressions, prefer contract/invariant integration coverage over unit-only tests.
- Escalate only real GitHub auth, scope, thread mutation, or rate-limit regressions to sandbox E2E.

## Design guidelines

### New concepts require an explicit trade-off

Introducing a new concept (evidence record, content hash, lock, ledger, status field, intermediate state) must answer two questions in the PR description:

> Delete this six months from now — what breaks?

> What does it still not catch?

Answer the first with the concrete failure it prevents and why a simpler move — delete a layer, trust agent output, fail loud — does not. "Nothing anyone would notice" means do not build it. If it adds persisted state, the diff must also contain the test that fails when that state goes stale; the cost of keeping it in sync is a guess until something checks it.

Validation and gates are not free — each expands the surface where reality and the model can disagree. Neither is prose: a weak answer stays weak at any length, so two sentences that answer both questions are done.

### Name the authority before enforcing it

Any PR that adds a gate, validation step, persisted field, or "verify before acting" check on an agent-driven action must answer in its description, in one sentence:

> What is the authority for this action, and why is it not the agent's own structured output?

If the honest answer is "the agent's output, but we don't trust it", make the agent's output more structured instead of building an inference layer on top of infra state. Infra signals are for drift detection, not for authority.

### A second fix to the same subsystem is a design signal

A subsystem that receives a second `fix:` PR shortly after the first — near-identical titles on the same area, or a patch to the mechanism the first fix introduced — means this PR's description must say which of these it is. Two fixes separated by ordinary maintenance intervals are not this signal; the trigger is proximity, not lifetime history.

- **The abstraction.** The mechanism needs a new special case for every situation it meets. The patches are growing outward rather than converging.
- **The execution.** The design was right and a branch it already implied was missed — often because the first PR was too large to hold at once, or was scoped past what the agent writing it could cover.
- **Something upstream.** The first fix was a local patch to a systemic defect, and this is another symptom of the same defect.
- **An unrelated path.** Two independent defects that happen to live in the same subsystem, and whose titles look alike because the subsystem is what titles name. This is the answer a busy subsystem produces honestly, and it carries no design conclusion at all.

Only the first is a revert signal. The second is finished by completing it — reverting there discards a correct design and reopens the original bug, and the thing to correct is the scope of the next task, not the code. The third means the redesign target sits *above* the fix being blamed; reverting it moves nothing. The fourth means there was no signal here, but say so rather than leaving the reader to assume it.

When it is the abstraction, choose an exit and record it in the description:

- **Revert the first fix and redesign** when reverting would not reintroduce a defect that is still reachable. The test is reproducibility, not whether anyone is currently complaining: a fix that works has no live victims *because it works*, and "nobody is hitting it" would license reverting exactly the load-bearing fixes that must not be reverted.
- **Redesign forward** otherwise, landing the replacement in one diff.

Say which of the four this is, and — when it is the abstraction — which exit you took and why. Stacking a third patch without that is not an option.

### Prefer deletion over another layer

Before adding durability, recovery, persistence, confirmation, or a new gate, first attempt the opposite direction: can a layer be removed to make the problem disappear? Record the result in the PR description, even if the conclusion is "no". A strongly positive net diff on a path that has already been patched is suspect by default.

### Test-file growth is a design smell

Test growth that is large *relative to the production change it covers* is a design smell. Explosive test growth usually means an internal state machine is being propped up rather than simplified.

This is a prompt to look, not a threshold to clear. There is deliberately no line count here: a fixed number invites the two useless outcomes — splitting a file to get under it, and arguing about whether a diff qualifies — while saying nothing about whether the design is actually wrong. What matters is which of these the growth reflects:

- **Propping up.** Each new state, flag, or output field multiplies the cases the table has to enumerate. The fix is to remove states, not rows. A wide matrix over many boolean outputs is the signal.
- **Covering.** Logic that already shipped untested is getting its first assertions, or a pure function was extracted out of a runner and is now testable at all. Production files shrinking in the same diff is the tell. Cutting these tests makes the codebase worse.

Say which one it is in the PR description when the growth is conspicuous, and name what you tried to remove. "The surface shrank by N states and the remaining rows are the surviving behavior" is a complete answer; "it is under the limit" is not.
