# Issue #466 — feat(coordinator): daily post-merge digest

## Problem

With #116 (auto trust) plus #460/#461, the human's role shifts from
pre-merge gatekeeper to post-merge auditor. The failure mode that per-PR
checks cannot catch is compounding drift: thirty PRs each individually
fine, collectively bending the architecture. Catching that requires a
periodic human look at *what merged* — but nothing produces that view
today. The notify gateway exists (`internal/infra/notify/gateway.go`),
Telegram intake is inbound-only, and the dashboard is pull-based with no
daily summary surface. #117 proposes a digest for pipeline *stalls*; this
issue is the complementary half: a digest of pipeline *output*, so
post-merge auditing takes five minutes a day instead of reconstructing the
day from the GitHub timeline.

## Goal

A scheduled daemon-wide job (daily, configurable, disabled by default)
that assembles a post-merge digest from durable storage only and delivers
it through the existing notify gateway plus a dashboard section rendering
the same data. No new GitHub reads at digest time.

## Scope

In scope:

- A periodic digest job owned by the daemon, started and stopped with
  the daemon lifecycle, config-gated and disabled by default. The schedule
  is daemon-wide (one goroutine, one payload, one date key), so its
  configuration lives in global config, not under a project-scoped role
  (see Configuration).
- Assembly of four sections from existing durable records:
  - **Merged** — PR title, originating issue, reviewer verdict summary,
    gatekeeper gate summary at merge time, diff size.
  - **Closed-and-regenerated** (#462/#464) — which PRs, which failure
    fingerprints, whether the retry succeeded.
  - **Awaiting human** — PRs whose latest gate report is blocked by a
    human-actionable reason code, with age. Membership is defined by gate
    reason codes, not by a `needs-human-review` label: no existing
    component produces or durably captures that label, so requiring it
    would be unenforceable in a derived projection that reads gate reports.
  - **Anomalies** — merges where the reviewer flagged non-blocking issues,
    or where the verdict flipped near merge time.
- Delivery through the existing notify gateway (one payload, all configured
  channels) and a new dashboard section backed by a new HTTP route that
  re-assembles the same data on demand.
- Empty-day behavior: quiet no-op or a one-line "nothing merged" per config.
- Delivery-failure safety: the digest is reconstructable from storage and
  always visible in the dashboard, so a failed push does not lose it.

Out of scope:

- #117's stall digest (sibling issue, separate work).
- #118's auditor agent (consumes the same attention surface but is its own
  feature).
- New persisted tables or delivery ledgers. The storage changes are a bounded
  range query on the existing `event_logs` table, a small payload extension to
  `pr.review.posted`, and one additive storage-owned monotonic `event_logs.seq`
  column migration used only to make equal-millisecond ordering survive
  VACUUM/backup restore. The sequence is not a digest state machine or a second
  authority; it is the insertion-order key already used by other durable
  records. The migration backfills existing rows once and assigns the next
  value transactionally on append. No `created_at`-leading index or other
  optimization is added; the bounded query documents its existing-index cost.
- Cron expressions; a fixed daily schedule plus timezone is sufficient.
- Per-PR re-reads from GitHub at digest time.
- Merges that no durable record observes. Only the gatekeeper
  `confirmAndMerge` path emits a durable merge event today; reviewer
  auto-merge and manual maintainer merges are not covered (see Merged).

## Digest window

The digest is a **derived projection** over existing durable records. It
introduces no new persisted state. The window is a **fixed boundary derived
from the digest date**, not the instant the job fires, and it is always a
**completed** window when the job queries it.

The digest date `D` identifies the window `[D 00:00 in tz, (D+1) 00:00 in
tz)` — exactly one calendar day in the configured timezone. The window is
always a calendar day: there is no `LookbackHours` knob. A configurable
non-24-hour window was rejected because it breaks the one-date-one-window
identity the design depends on: with a 48-hour window, consecutive daily
runs overlap and the same merge appears in two digests, while a sub-24-hour
window leaves gaps where a merge appears in none. Restricting the window to
the calendar day makes the date `D` alone identify a complete, non-
overlapping partition of event time, which is what the date component of
the `digest:<D>:<configFingerprint>` dedupe key and the date-only
dashboard route both assume.

When the job fires at `FireTime` on calendar day `F`, it selects `D` = the
most recent date whose window has fully closed before the fire instant
(`(D+1) 00:00 in tz <= fireInstant`). With `FireTime = "09:00"`, firing on
day `F` selects `D = F − 1` and summarizes the completed previous calendar
day; the 09:00 run therefore reports all of yesterday's events, not the
00:00–09:00 slice of today. Tomorrow's run selects `D = F` and covers the
day that has just closed. No "last digest run" timestamp is kept in sync,
and a missed run simply re-runs the date that was missed, since `D` is a
pure function of the fire date and the configured timezone.

The window end `(D+1) 00:00 in tz` is constructed by **calendar arithmetic
in the configured location**, not by adding `24h` to `D 00:00`: adding
`24h` to local midnight is not exactly the next calendar day on a
daylight-saving transition — a 23-hour spring-forward day extends the
window into the next date, and a 25-hour fall-back day omits the final
hour, causing overlap or missing merges. `end(D)` is built via
`time.Date(D.year, D.month, D.day+1, 0, 0, 0, 0, loc)`, which lets the
`time.Location` resolve the DST transition so the window is always the
full calendar day.

The start boundary uses the same calendar-day helper and does not assume that
`time.Date(D.year, D.month, D.day, 0, 0, 0, 0, loc)` is a valid instant. Some
timezones move their clocks forward at local midnight; Go may normalize that
request to the prior date (or another wall time), which would make the window
overlap the preceding digest. The helper validates the returned local date and
wall clock, and when midnight is nonexistent it searches forward from the
transition until the first valid instant whose local date is `D` (the start of
that calendar date). `end(D)` is computed as the same helper for `D+1`, so both
bounds are valid date boundaries even on a midnight transition.

If an IANA zone skips an entire civil date (for example `Pacific/Apia` on
2011-12-30), no instant can satisfy the requested local date. The boundary
helper must search only a bounded interval (at most 48 hours) and return a
typed nonexistent-date error when the date is absent, rather than looping
forever. The dashboard maps that error to its existing 4xx invalid-date
response; the scheduled job records the skipped date as an anomaly and does
not invent a zero-width or overlapping window. Tests cover both a midnight
transition and a wholly skipped date.

A fixed date-derived boundary is required so the date-only dashboard route
can reconstruct the exact window the job pushed: the window is a pure
function of the digest date and the configured timezone, both of which the
handler reads from daemon config. Ending the window at the actual fire
instant would make a late catch-up unreproducible from the date-only route
and would overlap consecutive daily output; the fixed, completed calendar-
day boundary avoids both.

Assembly reads:

- `EventsRepository.ListBetween(ctx, sinceISO, untilISO)` for the window
  (`internal/storage/repositories.go`), a new bounded range query with an
  **exclusive end bound**:
  `SELECT * FROM event_logs WHERE created_at >= ? AND created_at < ? ORDER BY created_at ASC, seq ASC`.
  The explicit storage-owned `seq` is the insertion-order tie-breaker; random
  event IDs and SQLite's implicit `rowid` are not causal and are not durable
  across VACUUM/restore. Every latest/consecutive-event query used by the digest
  (`ListBetween`, `ListByEntity`, and latest-per-entity enumeration) applies the
  same `created_at, seq` ordering, and validation inserts equal-millisecond
  events before and after backup/restore to prove the backward scan selects the
  actual durable insertion order.
  The existing `ListSince` applies only `created_at >= since` and is not
  used: an unbounded historical scan would let a `/api/v1/digest?date=`
  request for an old date read every event from that date through the
  present before any in-memory day filtering, so endpoint cost would grow
  with database history. `ListBetween` bounds the scan on both sides for
  both the job and the dashboard route. The existing `event_logs` indexes
  are `(entity_type, entity_id, created_at)` and `(event_type,
  created_at)`; neither has a `created_at` leading column, and no
  `created_at`-leading index is added in this slice. The range predicate
  therefore scans rows from `since` forward and sorts in memory; the cost grows
  with total `event_logs` history rather than with the one-day window. This is
  an accepted, documented trade-off for a derived projection: the daily job
  scans once per day, and the dashboard route is on-demand (non-polled). A
  `created_at`-leading index remains a future optimization (see Risks).
- `PullRequestSnapshotsRepository.GetLatestByProjectAndHead` for current PR
  title, review state, and diff metadata, selected by `projectID`, `repo`,
  `prNumber`, **and the merge event's `HeadSHA`**. The existing
  `GetLatestByProject` returns the newest snapshot regardless of head and
  must not be used for merged-PR fields: a snapshot for a later head would
  attribute another commit's title/diff to the merged head. The new method
  filters `pull_request_snapshots` by `head_sha` and returns the latest
  matching snapshot, or `nil`. When no snapshot matches the merged head,
  PR title, review state, and diff metadata are rendered **unavailable**
  rather than falling back to a newer or older snapshot (see Merged and
  Risks).
- Gate reports: `pull_request.merge_gate.evaluated`
  (`gatekeeper.GateReportEventType`, payload `gatekeeper.Report`) and
  `pull_request.merge_gate.merge_attempted`
  (`gatekeeper.MergeOutcomeEventType`, payload `gatekeeper.MergeOutcome`).
- Reviewer verdicts: `pr.review.posted` events (payload extended with the
  structured reviewer `outcome`, see below). The snapshot's `ReviewState`
  is **not** a verdict source: it is GitHub's aggregate decision
  (`APPROVED` / `CHANGES_REQUESTED` / `REVIEW_REQUIRED`), not Looper's
  `clean` / `non_blocking` / `blocking` outcome, so clean and non-blocking
  comments can share the same aggregate state.

## Sources and assembly

### Merged

For each `pull_request.merge_gate.merge_attempted` event with
`MergeOutcome.Merged == true` in the window:

- PR identity (`Repo`, `PRNumber`) from the `MergeOutcome` event itself,
  which carries them authoritatively; PR title from the head-matched
  snapshot (`GetLatestByProjectAndHead` for the merge event's `HeadSHA`).
  The rendered pull-request URL is built from the event's `Repo` using
  `githubinfra.SplitRepoHostname`: an unqualified repo uses `github.com`,
  while a host-qualified value such as
  `github.example.com/acme/looper` preserves that enterprise hostname and
  links to `https://github.example.com/acme/looper/pull/<PRNumber>`. The
  renderer must never hard-code the cloud host.
  Every selected `MergeOutcome` already carries `Repo` and `PRNumber`, so
  the repository and PR number never depend on the snapshot: when no
  snapshot matches the merged head, only snapshot-owned fields (title,
  review state, diff metadata) are rendered unavailable — the digest can
  still identify and link the merged PR from the merge event. The title
  is never read from a newer snapshot for a different head.
  Validation includes a host-qualified repository fixture and asserts that the
  rendered link uses that repository's hostname rather than `github.com`.
- Originating issue: resolved via a project-scoped loop lookup using the
  merge event's `ProjectID`, `Repo`, and `PRNumber` — the loop's source
  issue is the originating issue. A source issue is identified by the
  **composite** `(IssueRepo, IssueNumber)` from the worker metadata, not by
  number alone: `owner/a#42` and `owner/b#42` are distinct issues. The
  collected candidate set and conflict check therefore use both fields, the
  digest renders both repo and number (using the persisted `IssueURL` when
  present for the link's host), and a missing `IssueRepo` or number omits the
  issue rather than guessing the merge repository. The merge event is appended by
  `persistMergeOutcome` with `ProjectID`, `EntityType`, and `EntityID`;
  it carries no `LoopID`, so a `merge event → LoopRecord` join on
  `LoopID` is not possible. The pre-existing unscoped
  `LoopsRepository.ListByRepoAndPR(repo, prNumber)`
  (`internal/storage/repositories.go`) has **no project predicate**. If
  the assembly used it, the repository's support for the same
  `repo`/`prNumber` under multiple projects would let the most recently
  updated loop belong to another project, causing the digest to attribute
  that other project's source issue to this merge. The assembly therefore
  uses a new
  `LoopsRepository.ListByProjectRepoAndPR(projectID, repo, prNumber)`
  that adds a `project_id` predicate to the same `updated_at DESC, seq
  DESC` ordering, so only loops belonging to the merge event's
  `ProjectID` are candidates. The assembly inspects **all** returned
  candidates for distinct source issues **before** selecting: it collects
  the set of distinct source issues among loops whose source issue is
  set, and only when that set has exactly one element picks the most
  recent such loop. When two or more candidates name **different** source
  issues (retries or manually created loops that disagree), the issue is
  omitted rather than attributed to the newest one — selecting the most
  recent loop first cannot establish whether a second project-scoped loop
  names a different source issue, so the conflict check precedes
  selection. If no candidate has a source issue, the issue is omitted
  rather than guessed. The collected identity key is the canonical
  `(IssueRepo, IssueNumber)` pair (including the host when present), and a
  fixture with `owner/a#42` plus `owner/b#42` must render two distinct issues,
  never collapse them by number alone.
- Reviewer verdict summary: the most recent `pr.review.posted` at or before
  the merge attempt for the same PR **whose `headSha` equals the merge
  event's `HeadSHA`** and **whose event record's `ProjectID` equals the
  `MergeOutcome.ProjectID`** (both payloads already carry the head; the
  project match prevents a review produced under project B from becoming
  project A's displayed merge evidence when the same `repo`/`prNumber` is
  configured under two projects), read from its persisted `outcome`
  (`clean` / `non_blocking` / `blocking`). Matching the head prevents
  attributing a verdict for an obsolete commit to the merged code: after
  Looper posts `outcome=clean` for head A, a push creates head B and
  gatekeeper may merge B, so a timestamp-only lookup would wrongly report
  A's clean verdict for B. This per-PR verdict is read from the PR's full
  `pr.review.posted` history via
  `EventsRepository.ListByEntity(ctx, "pull_request", entityID)` (ordered
  `created_at ASC, seq ASC`), **not** from the digest date's `ListBetween` result:
  a merge at 00:05 whose head-matched review was posted at 23:55 the
  previous day lies outside the digest's `ListBetween` window, so a
  window-bounded lookup would render the verdict unavailable despite a
  structured durable outcome existing one boundary away. The assembly
  scans the `ListByEntity` history backward from the merge attempt and
  takes the first event matching the merged head and project. If no
  `pr.review.posted` event matches the merged head and project — including
  a historical event whose payload predates the `outcome` extension and
  lacks it — the verdict is rendered **unavailable** rather than falling
  back to the snapshot `ReviewState` (see above): `ReviewState` is
  GitHub's aggregate decision, not Looper's outcome, so presenting it as
  the verdict would be a guess.
- Gatekeeper gate summary at merge time: the confirming `GateReport`
  evidence. The confirming pass carries a fresh
  `ConfirmationCorrelationID` through its `GateReport` event and the
  eventual `merge_attempted` event's existing event-log `CorrelationID`; the
  digest requires that exact correlation match in addition to project and
  head. Generate it once for the confirming pass and thread it through the
  existing `EvaluationInput`/event-log fields; no new table or persisted
  authority is introduced. The producer contract is explicit: `confirmAndMerge`
  allocates the correlation before the confirming evaluation, passes it into
  `EvaluationInput`, the confirming `GateReport` event and `merge_attempted`
  copy that exact value, and every unrelated webhook evaluation receives a
  distinct correlation. A producer-path integration test interleaves such a
  webhook evaluation between confirmation and outcome persistence and asserts
  that only the correlated report is selected. `MergeOutcome.AttemptedAt` is stamped **before**
  `confirmAndMerge` calls `EvaluatePullRequest` for the confirming pass
  (`internal/gatekeeper/merge.go`), and that confirming evaluation
  persists the `GateReport` that proves the merge was eligible, so the
  confirming report's `EvaluatedAt` is **after** `AttemptedAt`. Selecting
  the most recent report "at or before `AttemptedAt`" would therefore
  return the earlier first-pass report and exclude the actual confirming
  evidence. The assembly instead selects the most recent
  `GateReportEventType` event for the PR **whose head matches the merged
  `HeadSHA`**, **whose event record's `ProjectID` equals the
  `MergeOutcome.ProjectID`**, and **whose event-log `CorrelationID` equals
  the merge event's confirming correlation**; the report's `EvaluatedAt` is
  still required to be at or before the
  `MergeOutcomeEventType` event's own persisted `created_at` (the outcome
  event bounds the confirming report, which is persisted before the
  outcome is appended). This confirming report is read from the merge
  PR's full gate-report history via
  `EventsRepository.ListByEntity(ctx, "pull_request", entityID)` (ordered
  `created_at ASC, seq ASC`), **not**
  from the digest date's `ListBetween` result: when confirmation starts
  just before midnight and the forge merge finishes just after midnight,
  the successful `MergeOutcome` belongs to the new digest window while
  its confirming `GateReport` belongs to the prior one, so a
  `ListBetween`-bounded selection (Merged is otherwise bounded by
  `ListBetween`) cannot find the report it promises to summarize. The
  assembly fetches the merge PR's gate history through the outcome event
  and applies the head/project/time predicates there. `MergeOutcome.ConfirmingReasons` is **not** used:
  it is populated only when the confirming evaluation becomes blocked, in
  which case `Merged` remains `false`, so every event selected here with
  `Merged == true` has an empty `ConfirmingReasons`. A successful merge
  therefore has no blocking-reason summary by definition. A legacy merge
  event without the confirming correlation renders the gate evidence
  unavailable rather than falling back to timestamp/head matching. The digest
  reports the confirming `GateReport`'s `Status` and, for the actual gate
  detail, its `Evidence` (`gatekeeper.Report.Evidence`): the required
  checks and their per-check status/conclusion (`Evidence.Checks`,
  `Evidence.RequiredChecks`), the review decision
  (`Evidence.ReviewDecision`), the required approving review count, and
  the mergeability state. `Reasons` is empty for every successful merge
  (`Eligible` is true only when `Reasons` is empty), so `Status` +
  `Reasons` alone would show the same eligible summary for every merged
  PR; `Evidence` is what distinguishes them.
- Diff size: derived from the raw `diff` retained in the **head-matched**
  snapshot's `PayloadJSON` (`payloadMap["diff"]`) by counting `+`/`-`
  content lines in **hunk bodies only**. A unified diff's grammar is
  `diff --git`, `--- <old>`, `+++ <new>` file headers, `@@ ... @@` hunk
  headers, then `+`/`-`/` ` content lines; the counter walks the diff line
  by line, recognizes the complete file-header grammar (`diff --git` plus
  the immediately following `---`/`+++` pair) so those two header lines are
  excluded, and counts only lines that begin with `+` or `-` **after** the
  first `@@` hunk header of each file. Excluding every line with a `+++`
  or `---` prefix would also exclude valid hunk content: adding a source
  line whose text begins with `++` produces a diff line beginning with
  `+++`, and removing content beginning with `--` produces `---`, so a
  naive prefix filter silently undercounts those additions and deletions.
  Restricting counting to hunk bodies (lines after a `@@` header, with the
  file-header pair recognized and skipped) avoids misclassifying hunk
  content as a header. The raw diff belongs to the snapshot's `HeadSHA`,
  so the count is only valid for the merged head when the snapshot was
  selected by `GetLatestByProjectAndHead` for that head; counting the
  latest snapshot regardless of head would report another commit's
  additions/deletions as the merged head's diff size.
  `CapturePullRequestSnapshot` stores a `detail` object with no
  `additions`/`deletions` fields plus the raw `diff`, so the numeric
  counts are derived from the retained diff text, not from a field that
  does not exist. When no snapshot matches the merged head, diff size is
  rendered **unavailable**. When the matching snapshot recorded
  `diffTruncated: true` (the diff was oversized or local capture was
  truncated), the digest renders diff size as "unavailable (truncated)"
  rather than counting an incomplete diff. No GitHub read at digest time.

#### Merge authority coverage

`MergeOutcomeEventType` is emitted only by `gatekeeper.confirmAndMerge`
(`internal/gatekeeper/merge.go`). The reviewer auto-merge path calls
`github.EnableAutoMerge` (`internal/reviewer/runner.go`) without emitting
a merge event, and a maintainer may merge manually with no Looper event
at all. This design is a derived projection over existing durable records
and adds no new persisted state, so it can only observe what is already
durably recorded. The Merged section therefore covers **only PRs merged
through the gatekeeper `confirmAndMerge` path**; deployments that rely on
reviewer auto-merge or manual maintainer merges will produce a "nothing
merged" digest despite actual merges. This is an explicit, documented
scope narrowing rather than a silent gap: covering every supported merge
path would require a durable merge observation shared by all paths, which
is a separate, larger change and is out of scope for this derived
projection. The limitation is listed in Risks.

### Reviewer outcome on `pr.review.posted`

A `pr.review.posted` payload today contains only `repo`, `prNumber`,
`event` (the GitHub review event: `APPROVE` / `COMMENT` /
`REQUEST_CHANGES`), and `headSha`. The GitHub `event` does not reveal the
Looper reviewer outcome: under the default policy both a clean review and
a blocking review can publish as `COMMENT` (clean policy may be `COMMENT`,
blocking policy may be `COMMENT`), so a comment-only event is ambiguous
between `clean`, `non_blocking`, and `blocking`. Treating comment-only
events as non-blocking anomalies would therefore misreport ordinary clean
or blocking reviews.

The `pr.review.posted` event payload is extended to include the structured
reviewer `outcome` (`clean` / `non_blocking` / `blocking`) that the
reviewer runner already computes and stamps into the review body marker
(`<!-- looper:review outcome=... -->`). This is a small payload extension
to an existing event, not a new table: `recordPublishedReviewProgress`
threads the `outcome` it already receives (via
`pendingReviewCheckpoint.Outcome`, populated at every call site) into the
`appendEvent` payload map. The digest classifies reviews by this
`outcome`, never by the GitHub `event` type alone.

Because `outcome` is persisted state that must stay synchronized with the
review actually published, the validation includes a **producer-level**
persistence test that runs `recordPublishedReviewProgress` and asserts the
persisted `pr.review.posted` payload carries `outcome` equal to the
reviewer's computed outcome — not just an assembly fixture that seeds a
prebuilt event. Deleting the field breaks the Merged verdict summary and
both Anomaly signals: they render `unavailable` (no head-matched verdict)
or misclassify a clean/comment review as a non-blocking anomaly. What it
cannot capture is the review body marker itself, which remains the
authority for label transitions; the payload `outcome` is a derived
projection of that marker for digest-time classification.

**Persistence ordering and failure observability.** The current
`recordPublishedReviewProgress` first persists `lastPublishedHeadSha` and
then calls `appendEvent`, whose `eventlog.Append` error is explicitly
ignored. A transient append failure therefore permanently suppresses a
retry (the head-sha update already landed) while leaving no
`pr.review.posted` outcome for the digest, so the field does not in fact
stay synchronized under failure. The implementation must not claim the
field stays synchronized without making the update and the event
observable together: make the `lastPublishedHeadSha` update and the
`appendEvent` call atomic (a single SQLite transaction, or an equivalent
rollback that restores the prior head marker when append fails). A bare
"surface the append error" is insufficient: if the head marker remains
committed, the retry sees that SHA and skips publishing, so no later event
repairs the digest. The validation adds a **failure-path contract test** that
injects an `eventlog.Append` error into `recordPublishedReviewProgress`,
asserts the head-sha update is not left committed ahead of the missing event,
then retries and proves the whole publish/event path is attempted again.

### Closed-and-regenerated (#462/#464)

This section depends on the close-and-regenerate tracking that #462/#464
introduce. The digest does not own that concept; it reads a named
lifecycle event that #462/#464 emit at the transition. To prevent the
section from silently rendering empty when the producer's field names
diverge from what the digest expects, the event contract is fixed here and
locked by a fixture before the digest assembly is implemented.

**Event contract — `pull_request.closed_and_regenerated`:**

The close-and-regenerate transition and the retry that follows it are
**two distinct observations** with different terminal reachability, so
they are emitted as two event types sharing one payload schema. A single
append-only event emitted at the transition can only carry
`retryOutcome = pending` (the retry that follows the regeneration has
not completed yet), and a one-event-per-transition dedupe rule would
then prevent any later terminal observation, leaving dashboards and
digests permanently stale instead of reporting whether the retry
succeeded. The contract therefore defines a transition event and a
follow-up completion event:

- **Transition event** — type `pull_request.closed_and_regenerated` (a
  new `event_logs.event_type`, emitted by #462/#464 at the close-and-
  regenerate transition).
- **Completion event** — type
  `pull_request.closed_and_regenerated.retry_completed` (a second new
  `event_logs.event_type`, emitted by #462/#464 when the retry loop that
  followed the regeneration terminates).
- Entity (both): `entity_type = "pull_request"`,
  `entity_id = "<repo>#<prNumber>"`.
- Payload fields (both):
  - `repo` (string), `prNumber` (int), `headSha` (string) — PR identity,
    matching the identity the gatekeeper and reviewer events already carry.
  - `discoveryFingerprint` (string) — the fingerprint of the discovery
    that failed and triggered regeneration, observed at transition time.
  - `retryOutcome` (enum string): `succeeded` | `failed` | `pending` —
    whether the retry loop that followed the regeneration succeeded,
    failed, or had not completed when the event was emitted. The
    transition event is emitted with `pending`; the completion event is
    emitted with `succeeded` or `failed`.
  - `closedAt` (RFC3339 string) — the transition timestamp (carried by
    both events so the completion event joins back to its transition).
- Deduplication: one transition event per `(ProjectID, entity_id,
  discoveryFingerprint, closedAt)` transition and one completion event
  per `(ProjectID, entity_id, discoveryFingerprint, closedAt)` transition;
  #462/#464 are responsible for not emitting duplicates for the same
  transition (the digest does not dedupe producer output). The two event
  types dedupe independently, so the terminal completion event is not
  suppressed by the transition event that preceded it.

The assembly lists PRs in the window that were closed by the regenerate
flow and the retry loop that followed, with the failure fingerprint and
retry outcome. The fingerprint and retry outcome come from these events'
own payloads, not from a join against the mutable `LoopRecord`.
`autonomousRecovery.lastFailedDiscoveryFingerprint` on
`LoopRecord.MetadataJSON` is a mutable "last" value: a later failure can
overwrite it, recovery can clear it, and the loop's completion status can
change after another retry, so a dashboard request issued later could
attach a newer fingerprint or outcome to an older close event. The
lifecycle events carry both values observed at the transition and at
retry termination; the digest reads `discoveryFingerprint` directly and
selects `retryOutcome` as the **latest** outcome for the
`(ProjectID, entity_id, discoveryFingerprint, closedAt)` triple: if a
`retry_completed` event exists for that project-scoped triple, its terminal
`succeeded`/`failed` outcome is used; otherwise the transition event's
`pending` is used and the section renders the retry as "in progress
(pending)". This lets a `pending` observation reach a terminal state
once the completion event lands, and a triple whose retry never
terminates (or whose completion event has not yet been emitted) is
reported as `pending`, not silently promoted to a terminal outcome. If
#462/#464 have not yet persisted events of these types with these
fields, the section renders empty with a note naming the missing event
types and fills in once they do.

**Reading completion events across the window boundary.** The transition
events are discovered through the digest date's `ListBetween` window, but
a transition that occurs near midnight can have its retry complete after
`(D+1) 00:00` — the completion event then lies outside the
`ListBetween` source this section assigns to the transition, so the
digest for `D` would keep rendering `pending` even after completion, and
the next digest has no in-window transition to attach the late
completion to. After discovering transition events in the date window,
the assembly therefore queries each transition's matching completion
event through the **assembly instant**, not the window: for every
discovered transition triple `(ProjectID, entity_id, discoveryFingerprint,
  closedAt)`, it reads the project-scoped PR lifecycle via
`EventsRepository.ListByEntity(ctx, "pull_request", entityID)` (ordered
`created_at ASC, seq ASC`) and selects the latest
`pull_request.closed_and_regenerated.retry_completed` event whose
`(ProjectID, entity_id, discoveryFingerprint, closedAt)` matches the transition,
regardless of whether that completion event falls inside or outside the
digest window. The completion's terminal outcome then supersedes the
transition's `pending`. This mirrors the cross-boundary reads the Merged
and Awaiting-human sections already use (`ListByEntity` for per-PR
history that can straddle the window edge) and prevents a stale
`pending` from persisting once the durable completion event exists.

**Producer-consumer contract coverage.** Because the producer (#462/#464)
is separate work, an assembly fixture that seeds a prebuilt
`EventLogRecord` cannot detect producer/consumer drift — the section could
ship without a working producer. The validation therefore includes an
integration test that emits the real
`pull_request.closed_and_regenerated` transition event and its
`pull_request.closed_and_regenerated.retry_completed` follow-up through
the producer path #462/#464 define and then assembles the digest,
asserting the section renders the fingerprint and the terminal
`retryOutcome` from the emitted completion event. The fixture and the
integration test both conform to the payload schema above, so a producer
that emits different field names fails the test rather than silently
producing an empty section.

### Awaiting human

This section is a **current-state projection as of the assembly instant**,
not a windowed event set: it reports which PRs are awaiting a human *now*,
at the job's fire time (or the dashboard request time). Membership is
decided by each PR's latest gate report as of the assembly instant,
regardless of whether that report falls inside the digest window — a PR
first blocked at 08:00 on the digest date must appear in that day's 09:00
digest, and a PR whose hold was removed at 08:00 must not. Reading only
reports inside the window would snapshot status as of midnight rather than
at delivery. The digest date `D` still identifies the digest, but this
section reflects current state at assembly time, consistent with the
dashboard being a current projection (see Delivery).

A PR is listed when its latest gate report as of the assembly instant is
blocked with a human-actionable reason code — `ReasonHold`,
`ReasonReviewChangesRequested`, `ReasonUnresolvedReviewThread`,
`ReasonReviewRequired` — together with age. `ReasonHold` is always
human-actionable; the other three are included **only** when no active
reviewer or fixer loop is handling the PR (see below). This mirrors the
gatekeeper reason vocabulary already in `internal/gatekeeper/runner.go`; no
new reason codes are introduced. The triager's
`AwaitingConfirmationStatus` (`internal/triager/awaiting.go`) covers
*issue* triage awaiting confirmation and is a separate surface; this
section is PR-scoped and reads gate reports, not triage reports.

**Exclude PRs with any durable successful merge.**
Using the latest gate report as the current-state authority leaves a PR
listed forever when a maintainer closes or manually merges it after a
blocked report: those paths emit no durable merge event, the closed PR
disappears from gatekeeper discovery, and no later eligible report
replaces the blocked one. To keep the actionable section from presenting
work that no longer exists, membership additionally requires that **no
durable successful merge event for the project-scoped PR exists**:
the assembly checks the PR's lifecycle (read via `ListByProjectAndEntity`,
see below) for any `MergeOutcomeEventType` event with `Merged == true`; if
one exists, the PR was merged through the gatekeeper path and is excluded,
even when a racing stale blocked report was appended after the merge.
PRs closed or merged via paths that emit **no** durable event (manual
maintainer merge, reviewer auto-merge — see Merge authority coverage)
have no durable close evidence, so they remain listed until a subsequent
gate report or merge event supersedes the blocked report. This is an
explicit, documented stale-state limitation of a derived projection that
adds no new persisted state: the section renders the last blocked
report's `EvaluatedAt` so the human can see staleness, and the
limitation plus its reconciliation path (a later gate report or durable
merge event removes the PR) are listed in Risks. Requiring a durable
"still open" signal that no merge path produces today would be a larger
change and is out of scope.

**Age from the start of the continuous blocked interval.** Gatekeeper
re-evaluates an unchanged pull request every `maxSkipAge` (30 minutes in
`internal/gatekeeper/unchanged.go`), so a PR held for days will have a
latest report only minutes old. Using the latest report's `EvaluatedAt`
would measure time since the last re-evaluation, not how long the PR has
awaited a human. The assembly walks backward through consecutive
`GateReportEventType` events for the PR that carry a human-actionable
reason and uses the `EvaluatedAt` of the **first** report in that
continuous blocked interval (the boundary where the PR transitioned into
the blocked state). A gap in the blocked-reason sequence, or a report with
a non-human-actionable reason, ends the walk; the age is
`assemblyInstant − firstBlockedEvaluatedAt`.

**Candidate enumeration is unbounded, not windowed, but latest-only.**
`ListByEntity` requires an already-known `entityID`, so the assembly must
first enumerate the candidate PR entities. The only global event source
specified for the other sections is the date-bounded `ListBetween`
result, but a PR first blocked at 08:00 on the digest date — after the
previous day's window midnight `until` — has no event inside that
prior-day window, so it would never become a candidate if the windowed
result seeded the entity set. The assembly therefore enumerates
pull-request gate entities through an **unbounded current-state query**
that returns only the **latest** report per `(project_id, entity_id)`,
  `EventsRepository.ListLatestByProjectAndEntityTypeAndEventTypes(ctx,
  "pull_request", [GateReportEventType])`
  (`internal/storage/repositories.go`), partitioned by `(project_id,
  entity_id)`, and takes the distinct `(ProjectID, entity_id)` tuples whose
  latest report is blocked as the candidate set; it then reads each
  candidate's full lifecycle via a project-scoped `ListByProjectAndEntity`
  (or an equivalent `ListByEntity` result filtered by `ProjectID`) for the
membership check and backward walk. Materializing every gate report ever
written (the unbounded `ListByEntityTypeAndEventTypes` over all history)
and then performing another full-lifecycle query for each distinct PR
makes work and memory grow continuously with historical evaluations even
when the number of current PRs is stable, because unchanged blocked PRs
are re-evaluated every 30 minutes; enumerating only the latest report
per project/entity bounds the candidate set to the current project/PR count,
reserving the project-scoped lifecycle walk for the small set whose blocked
interval actually needs walking. This ensures a PR whose latest blocked
report falls after the window's `until` is still discovered.

The backward walk and the membership check both read the PR's full
lifecycle via `EventsRepository.ListByProjectAndEntity(ctx, "pull_request",
projectID, entityID)` (ordered `created_at ASC, seq ASC`), not the windowed
`ListBetween`
result: the latest report as of the assembly instant may lie after the
window's `until`, and the transition into the blocked state may lie before
the window's `since`. The assembly walks backward from the latest blocked
report as of the assembly instant through consecutive human-actionable
reports until it finds the transition — a preceding report with a
non-human-actionable reason, a gap, or the entity's first report — even
when that preceding report lies before `since`. The digest window bounds
the Merged, Closed-and-regenerated, and Anomaly sections; Awaiting-human
is a current-state section whose membership and age are bounded by one
PR's lifecycle, not by the window.

**Exclude work automation is still handling, matched to the responsible
role.** The active-automation correlation applies to every
automation-handled reason, not only `ReasonReviewRequired`, and each
reason is matched to the role that can actually address it.
`ReasonReviewRequired` just means GitHub currently lacks a required
approval; an active reviewer loop addresses it, and a fixer cannot supply
that approval. `ReasonReviewChangesRequested` and
`ReasonUnresolvedReviewThread` are addressed by an active fixer loop
(change-requested reviews and unresolved review threads are fixer work),
and a reviewer cannot resolve them. Listing any of these under "Awaiting
human" while the responsible role's loop is actively handling the PR
would report ordinary in-progress automation as human-actionable. Before
classifying a reason as human-actionable, the assembly correlates the
gate report with the **role that owns that reason** via a project-scoped
`LoopsRepository.ListByProjectRepoAndPR(projectID, repo, prNumber)`:
`ReasonReviewRequired` is suppressed only when a returned loop has an
active **reviewer** status (queued, waiting, or running) for the PR, and
`ReasonReviewChangesRequested` / `ReasonUnresolvedReviewThread` are
suppressed only when a returned loop has an active **fixer** status
(queued, waiting, or running) for the PR. The previous "any active reviewer or
fixer" condition omitted a PR when *either* role was active for *any* of
the three reasons, so an unrelated queued fixer would suppress a
missing-approval item even though a fixer cannot supply that approval;
matching each reason to its responsible role avoids that. Only
`ReasonHold` — an explicit human-applied hold label — is human-actionable
regardless of active automation. A cross-project fixture with the same
repository/PR and a waiting loop in the other project must not suppress this
project's item.

### Anomalies

Two signals, both derived from existing records and classified by the
persisted reviewer `outcome`, not the GitHub review event type:

- Merges where the most recent `pr.review.posted` before merge **for the
  merged `headSha`** and **whose event record's `ProjectID` equals the
  `MergeOutcome.ProjectID`** carried `outcome=non_blocking` (a
  non-blocking review with actionable feedback that did not block merge).
  The same head-match and project-match rules as the Merged verdict
  summary apply: a `non_blocking` review for head A, or a review produced
  under project B when the merge is under project A, must not flag the
  merge. This most recent pre-merge review is read from the PR's full
  `pr.review.posted` history via `EventsRepository.ListByEntity` (the
  same cross-boundary read the Merged verdict summary uses), not from the
  digest date's `ListBetween` result: a merge at 00:05 whose
  head-matched, project-matched `non_blocking` review was posted at 23:55
  the previous day lies outside the digest window, so a window-bounded
  lookup would miss the anomaly. A `COMMENT` event alone is not an
  anomaly: under the default policy a clean review may also publish as
  `COMMENT`, so only the structured `outcome` distinguishes them.
- Merges where the reviewer verdict flipped (e.g. `blocking` → `clean`,
  or `non_blocking` → `clean`) within a fixed window before the merge
  attempt. The window is `VerdictFlipWindowMinutes` (default 120, a field
  on `DigestConfig`, see Configuration) anchored to the merge event's
  `AttemptedAt`: only `pr.review.posted` events for the PR whose `headSha`
  equals the merged head, **whose event record's `ProjectID` equals the
  `MergeOutcome.ProjectID`**, and whose timestamp falls in
  `[AttemptedAt − VerdictFlipWindowMinutes, AttemptedAt]` are considered,
  ordered by time; a flip is two consecutive such events with different
  `outcome` values. This per-PR window is **not** read from the digest
  date's `ListBetween` result: the flip window can begin before the digest
  date's `since` (a merge at 00:30 with reviews at 23:30 the previous day
  and 00:10 has a valid 120-minute flip, but the 23:30 review lies in the
  previous day's window). The assembly reads the PR's `pr.review.posted`
  history via `EventsRepository.ListByEntity` over
  `[AttemptedAt − VerdictFlipWindowMinutes, AttemptedAt]` so the earlier
  review is present even when it falls outside the digest window. A fixed,
  configured threshold anchored to the merge attempt makes the
  classification stable across implementations and the fixture
  assertable, rather than leaving "short window" to mean anything from
  adjacent minutes to the whole digest window.

Both are "worth a second look" flags, not gates. The digest lists them; the
human decides.

## Scheduling

There is no cron infrastructure today; the runtime uses
`time.NewTicker`-based poll loops (`internal/runtime/runtime.go`). The
digest is not a discovery lane (it produces no queue items), so it is not
registered through `discovery_lanes.go`. Instead it is a dedicated periodic
job goroutine started in `Runtime.Start` and stopped through the existing
stop-channel lifecycle, next to the scheduler loop.

The job goroutine sleeps until the next configured fire time, then fires
once per day. A timezone-aware boundary is computed from config
(`time.Location` from the configured tz string; default UTC). On each fire
the job selects the digest date `D` = the most recent date whose window
`[D 00:00 in tz, (D+1) 00:00 in tz)` has closed before the fire instant
(under defaults, `D` = the previous calendar day). An in-memory
`lastDigestDate` string (the digest date `D`, YYYY-MM-DD in the configured
tz) prevents a same-process double-fire for the same `D`; it is not
persisted state and does not survive a restart.

#### DST-affected fire times

The fire time is a local `HH:MM` in the configured timezone, so a
`FireTime` can be nonexistent or repeated on a daylight-saving transition.
The scheduler's behavior is fixed and tested for both cases:

- **Spring-forward (nonexistent local time):** if `FireTime` does not
  exist on a given calendar day in the configured location (e.g. `02:30`
  on a spring-forward day that jumps 02:00 → 03:00), the scheduler fires
  **once on that same calendar day at a defined substitute instant**: the
  first valid local instant at or after the configured `FireTime` on that
  day, i.e. the gap-end instant the clock jumps to (`03:00` for a `02:30`
  `FireTime` on a 02:00 → 03:00 spring-forward). Firing on the skipped
  calendar day is required because that day's fire is responsible for
  digest date `D = F − 1`: shifting the fire to the next calendar day
  would select `D = F` and permanently lose `F − 1` (the next day's fire
  never selects `F − 1`, and relying on a later daemon restart to invoke
  startup catch-up is not guaranteed). The substitute instant stays on
  day `F`, so `D = F − 1` is still selected and delivered; only the fire
  instant shifts from `02:30` to `03:00` on the same day, which does not
  move the date-derived window. The substitute instant is computed
  **explicitly**, not by relying on `time.Date` normalization alone: Go's
  `time.Date` does not guarantee which zone a nonexistent local time
  resolves to, and for `America/New_York` a requested `2026-03-08 02:30`
  resolves to `01:30 EST` (the pre-gap instant), not the `03:00` gap end
  the substitute promises. The scheduler therefore constructs the
  candidate via `time.Date(F.year, F.month, F.day, hh, mm, 0, 0, loc)`,
  detects whether that instant is nonexistent by checking the
  normalization mismatch — the returned `time.Time`'s wall-clock
  `(hour, minute)` in the configured location does not equal the requested
  `(hh, mm)`, or its offset does not match either of the two transition
  offsets — and, when a mismatch is detected, searches for the first
  valid instant explicitly by advancing from the pre-gap boundary
  forward in small steps (e.g. one minute) until the wall clock reaches
  the gap-end instant (`03:00` for a `02:30` `FireTime` on a 02:00 →
  03:00 spring-forward). Only when the candidate is valid (no
  normalization mismatch) is it used directly. This avoids firing before
  the configured wall time or computing a past sleep target in zones
  where `time.Date` normalizes backward; the scheduler does not skip the
  calendar day.
- **Fall-back (repeated local time):** if `FireTime` occurs twice on a
  given calendar day (e.g. `01:30` on a fall-back day that repeats
  01:00–02:00), the scheduler fires **once**, at the first occurrence
  (the earlier UTC instant). The scheduler must find both valid instants
  whose local `(hour, minute)` equals `FireTime` (for example by checking
  both transition offsets), compare their UTC timestamps, and choose the
  earlier one explicitly; it must not rely on which zone `time.Date` happens
  to select. Firing twice would select the same `D` and rely on dedupe;
  firing once at the first occurrence is deterministic and matches the
  once-per-day contract.

Both cases are covered by tests asserting the selected digest date and
the fire count (one fire per calendar day: the spring-forward day fires
once at the gap-end substitute instant and still selects `D = F − 1`, so
no digest date is lost; the fall-back day fires once at the first
occurrence).

#### Catch-up from durable notification history

On startup the job does **not** simply fire the single most-recent closed
`D`. Selecting only the newest closed window loses missed dates: if day
`F−1`'s 09:00 run was missed and the daemon restarts after midnight on
`F+1`, the most recent closed date is `F`, so a newest-only catch-up
pushes `F` (early), the in-memory guard then suppresses another `F` at
09:00, and `F−1` is permanently skipped even though the outage lasted
under 24 hours. Instead the startup catch-up determines missed dates from
the **durable notification history**: it walks back from the most recent
closed `D` through a bounded catch-up horizon (a fixed 7 calendar days),
but considers a date `d` missed only when that date's scheduled fire instant
(`d+1` calendar day at the configured `FireTime`, resolved through the same
DST-aware helper) is at or before the current instant. A window can be closed
at local midnight while its fire is still in the future; that date is left for
the regular schedule. For each eligible date `d` in that range it queries
`NotificationsRepository.GetLatestByDedupe(ctx, "in_app",
"digest:<d>:<configFingerprint>")` (see Same-date delivery idempotence
for the key and why the check is a single date-wide invocation marker,
not a per-channel check). Any date with no invocation marker is processed in
chronological order before the regular schedule resumes: a non-empty digest
calls `Notify`, while a quiet empty-day digest writes its marker directly.
The horizon is
bounded so a long outage does not flood the channel on restart; dates
older than the horizon are accepted as lost (an audit surface, not a
retry queue). The in-memory `lastDigestDate` is then set to the newest date
whose invocation marker was successfully recorded (whether by `Notify` or
quiet mode), so the regular schedule does not re-invoke it.

**Activation boundary — first enablement is not a backfill.** Absence of
an invocation marker is not evidence that a digest was scheduled and
missed: on the first startup with this disabled-by-default feature
enabled, notification history contains no digest keys for any prior
date, so a naive "any date with no invocation marker is missed" rule
would classify all seven dates in the horizon as missed and push seven
historical digests, flooding configured channels on first enablement.
The catch-up therefore treats a date `d` as missed **only if there is
durable evidence the feature was already active on or before `d`**:
specifically, a delivered digest record **or a quiet invocation marker**
at some date `e <= d` proves the feature was active by `e`, so `d` is a
candidate only when `d` is **at or after the oldest invocation-marker date**
in the horizon. The boundary is the *oldest* marker, not the newest:
evidence that the
feature was active on `F−2` must make *later* closed dates within the
horizon (`F−1`, `F`) eligible for catch-up, not earlier dates. When the
daemon last recorded an invocation for `F−2`, then remains down for the
`F−1` and `F` fires and restarts on `F+1`, the oldest marker is `F−2`, so
`F−1` and `F` (at or after `F−2`, within the horizon, with no invocation
marker of their own) are pushed in chronological order; an "at or before the
newest marker" predicate would consider only dates at or before
`F−2` and never push the two trailing missed dates. If no invocation
marker exists anywhere in the horizon (first enablement, or an outage
longer than the horizon with no prior invocation), no `d` satisfies
the boundary, the catch-up pushes nothing, and the regular schedule
begins from its next configured fire. This uses already-persisted
delivered/quiet invocation records as the activation boundary — no new
"first enabled at" timestamp is introduced — so enabling the feature never
produces a historical backfill unless the operator explicitly requests one
(operator-triggered historical backfill is out of scope for this PR).

**Guard initialization reflects actual invocation, not window closure.**
The in-memory `lastDigestDate` is set to the **newest date whose invocation
marker was actually recorded** during catch-up (a date that was delivered or
quietly completed), not to the most recent closed `D`. Setting it to a closed
`D` that was never invoked
suppresses the legitimate first scheduled fire: if the feature is first
enabled at 08:00 with the default 09:00 fire time, there is correctly no
historical backfill, but the most recent closed `D` is `F−1` (yesterday)
and setting `lastDigestDate = F−1` would make the regular 09:00 run for
`D = F−1` look like a same-process duplicate, permanently skipping the
first scheduled digest. The guard is therefore initialized according to
whether an invocation actually completed, not merely whether the window is
closed: when catch-up recorded at least one new marker, `lastDigestDate` is
the newest such date; when catch-up recorded nothing (first enablement, or
every in-horizon date already delivered/quiet), `lastDigestDate` stays empty
so the regular schedule's first fire for its selected `D` is not
pre-suppressed. Whether today's fire has already passed is handled by
the schedule's next-fire computation (the schedule sleeps until the next
configured fire instant at or after startup), not by pre-seeding the
guard with a closed-but-undelivered date: a daemon that starts at 08:00
before the 09:00 fire lets the regular schedule fire 09:00 for `F−1`
(`lastDigestDate` empty, no duplicate), and a daemon that starts at
10:00 after the 09:00 fire has passed simply waits for tomorrow's 09:00
fire for `D = F` (the date function never re-selects `F−1` tomorrow).
The durable `in_app` notification-log check (see Same-date delivery
idempotence) remains the backstop that prevents a restart re-fire of a
date whose invocation marker was recorded.

### Same-date delivery idempotence across restarts

`lastDigestDate` is empty after a restart, so the catch-up path can re-fire
the same digest date `D`. The gateway's existing dedupe is **not** sufficient
to suppress that duplicate: `DedupeKey` delivery is deduped only while
`ThrottleWindowSeconds` has not elapsed (60 seconds by default for
osascript, webhook, and Feishu in `internal/infra/notify/gateway.go`), so
a restart more than a minute after the scheduled run would send a
duplicate push despite the claimed same-date idempotence.

The job therefore performs a **date-wide durable dedupe check** before
pushing. The digest's output depends on the effective configuration —
`Timezone` (window boundaries) and `VerdictFlipWindowMinutes` (anomaly
classification) — so the dedupe key must include that configuration, not
just the date: a config change after a date was pushed produces a
*different* projection for the same `D`, and a date-only key would
suppress it. The `DedupeKey` is therefore
`digest:<D>:<configFingerprint>` where `<D>` is the digest date and
`<configFingerprint>` is a short stable hash (e.g. the first 8 hex chars
of `sha256(Timezone + ":" + VerdictFlipWindowMinutes)`) over the
configuration that affects digest output.

**Config fingerprint — residual cases and why a hash over a versioned
key.** This content-hash concept is a dedupe identity, not a security
primitive, and two residual cases remain where it does not distinguish
effective configurations. First, truncating SHA-256 to eight hex
characters (32 bits) admits a collision probability that is negligible
for the small set of distinct configs a single daemon produces over its
lifetime but is not zero: two effective configurations can in principle
share a durable dedupe identity, in which case the second config's
projection for an already-pushed `D` is suppressed as a duplicate. The
collision is one-sided (a suppressed delivery is visible on the
dashboard, which re-derives under the current config) and recoverable by
a config tweak that changes the fingerprint; the truncation is retained
because a full 64-hex-char key bloats every `NotificationRecord.DedupeKey`
for a benefit (zero collisions) that does not change observable behavior
at this scale. Second, the hash input is manually assembled from the
fields known to affect output today (`Timezone`,
`VerdictFlipWindowMinutes`); a future output-affecting field added to
`DigestConfig` can be omitted from that input by mistake, producing the
same fingerprint for a genuinely different projection and silently
suppressing the new projection as a duplicate. This is guarded by a test
that fails when a new `DigestConfig` field that the assembly reads is
not included in the fingerprint input (see Validation), so the manual
assembly cannot silently drift. A simpler explicit versioned key (e.g.
`digest:<D>:v1`) was rejected because it bumps only on explicit code
changes: it cannot distinguish two *runtime* configurations (different
`Timezone`/`VerdictFlipWindowMinutes`) for the same `D`, so a config
change would be suppressed as a duplicate — the exact failure the
fingerprint exists to prevent. The hash is therefore preferable to a
versioned key for the runtime-config-distinction purpose, with the two
residual cases above accepted and guarded.

The idempotence unit is the **single digest invocation** (one
`Gateway.Notify` fan-out or one quiet marker write), not each channel: one
`Notify` call fans out to every configured channel, so
a per-channel durable check is incoherent with the all-channel operation
— skipping when any delivered record exists permanently abandons a
channel that failed, while requiring every channel to have succeeded
re-fires `Notify` and duplicates delivery on the successful channel
after its throttle expires. The Delivery section explicitly chooses
persistence over retries, so partial per-channel failure is accepted
rather than retried. The durable backstop is therefore a single
date-wide invocation marker: the job queries
`NotificationsRepository.GetLatestByDedupe(ctx, "in_app",
"digest:<D>:<configFingerprint>")` and skips the push when that record
exists, regardless of age. The gateway persists an `in_app`
`NotificationRecord` for every `Notify` call regardless of push-channel
outcome (`recordInApp` in `internal/infra/notify/gateway.go`), and quiet
mode writes the same channel/key directly, so the `in_app` record is the
durable proof that the invocation already happened; a push channel that
failed on the first call is not retried,
and the human pulls the digest from the dashboard (see Delivery). This
bounds restart re-fires using already-persisted state — no new "last
digest delivered at" timestamp is introduced.

**The marker write must succeed before channel fan-out.** The claimed
durable proof is absent when `recordInApp` fails to write: the existing
gateway drops that error and continues sending osascript/webhook/Feishu,
so a transient SQLite failure can deliver externally with no `in_app`
record, and a restart then re-delivers the same date because the
dedupe-check finds no marker. The implementation must therefore require
the `in_app` marker write to succeed **before** channel fan-out, but scope this
fail-closed behavior to a digest opt-in flag on the notification payload
(for example `RequireDurableMarker: true`). When that flag is set,
`recordInApp` is attempted first and a failure returns without fanning out
(no external delivery without durable proof). Ordinary worker-failure,
PR-ready, deployment, HITL, and recovery notifications keep the existing
best-effort fan-out semantics; changing the shared default would suppress
urgent alerts while storage is unhealthy. The validation adds a digest test
that injects an `in_app` persistence failure and asserts no push channel is
invoked, plus a non-digest regression proving the flag-off path still fans
out, so a transient marker-write failure cannot break restart idempotence or
silence unrelated alerts. The
gateway's short throttle still suppresses a same-process double-fire
within the throttle window; the date-wide `in_app` notification-log
check is the durable backstop that covers restarts beyond that window.
The dashboard route reconstructs the window from the daemon's current
config, so after a config change the date-only route re-derives under
the new config (a current projection, see Delivery); the
config-fingerprinted dedupe key ensures the new projection is delivered
rather than suppressed as a duplicate of the old one.

## Configuration

The digest schedule is daemon-wide — one goroutine, one payload, one date
key, one dashboard timezone — so its configuration lives in **global
config**, not under `CoordinatorRoleConfig`. `CoordinatorRoleConfig`
participates in `ProjectRoleConfigs`, so placing the schedule there would
let different projects inherit or override different fire times and
timezones with no rule for selecting among them; one project could
silently control every project's digest, or project overrides would be
silently ignored. A new top-level field on `Config`
(`internal/config/types.go`):

```go
type DigestConfig struct {
    Enabled                 bool   `json:"enabled"`
    FireTime                string `json:"fireTime"`                // "HH:MM" in configured tz, default "09:00"
    Timezone                string `json:"timezone"`                // IANA name, default "UTC"
    EmptyDayMode            string `json:"emptyDayMode"`            // "quiet" | "notice", default "quiet"
    VerdictFlipWindowMinutes int   `json:"verdictFlipWindowMinutes"` // default 120
}
```

Added to `Config` as `Digest DigestConfig` and to `PartialConfig` as
`Digest *PartialDigestConfig`. Default in `internal/config/defaults.go`:
`Enabled: false`, `FireTime: "09:00"`, `Timezone: "UTC"`, `EmptyDayMode:
"quiet"`, `VerdictFlipWindowMinutes: 120`. Config validation fails fast
at daemon boot on a malformed `FireTime`, an unknown
`Timezone`/`EmptyDayMode`, and a `VerdictFlipWindowMinutes` outside the
valid range. The lower bound is `> 0` (zero or negative makes the
verdict-flip anomaly ill-defined); the upper bound is
`math.MaxInt64 / int64(time.Minute)` (153,722,867 minutes), because the
implementation converts this value to a `time.Duration` to compute
`AttemptedAt − VerdictFlipWindowMinutes`, and values above that
threshold overflow a nanosecond `time.Duration`, turning the lookback
negative or moving the interval to the wrong side of the merge and
silently suppressing anomalies. Validation rejects any value exceeding
that threshold during fail-fast config validation, consistent with
looperd's existing fail-fast-on-config-validation behavior. There is no
`LookbackHours` field: the digest window is always the calendar day in
the configured timezone (see Digest window), so a window-length knob is
not exposed. TOML example:

```toml
[digest]
enabled = false
fireTime = "09:00"
timezone = "UTC"
emptyDayMode = "quiet"
verdictFlipWindowMinutes = 120
```

**Configuration-loader wiring contract.** Adding `Digest` to `Config` and
`PartialConfig` is not sufficient: the implementation must register the
top-level `digest` section in the decoder's `topLevelConfigSections`/section
map, include `Digest` in `mergeConfig` so file, environment, and CLI values
retain the repository's existing precedence, and include the section in
serialization/round-trip handling. A file-load test through both `Load` and
`LoadAt` must prove that `[digest]` values reach the daemon's effective
`Config` (and therefore the scheduler), while a malformed `[digest]` value
reaches the same fail-fast validation path rather than being silently ignored.
The test suite must cover TOML round-trip/serialization and file-load
precedence, plus rejection of malformed `FireTime`, `Timezone`,
`EmptyDayMode`, and `VerdictFlipWindowMinutes` values.

## Delivery

One `Gateway.Notify(ctx, payload)` call per non-quiet digest
(`internal/infra/notify/gateway.go:191`), with a `SystemNotificationPayload`
whose `Body` carries the rendered digest and whose `DedupeKey` is
`digest:<D>:<configFingerprint>` (the digest date plus the effective-config
fingerprint over `Timezone` and `VerdictFlipWindowMinutes`, see Scheduling)
so a same-date re-fire under the same config is deduped by the gateway's
existing throttle/dedupe path within the throttle window, while a config
change produces a new key and is delivered rather than suppressed; the
date-wide `in_app` notification-log check in Scheduling is the durable
backstop (one invocation marker, not a per-channel check). The digest sets
the payload-only `RequireDurableMarker: true` flag; this is an ephemeral
delivery policy, not new persisted state, and it scopes marker fail-closed
behavior to the digest without changing unrelated notification callers.
`Level` is set to a value the outbound channel allow-lists
accept and `Sound` is left empty so osascript does not play a sound for a
routine digest. Sound is controlled independently of level
(`payload.Sound` plus `osascript.soundForLevels` in
`internal/infra/notify/gateway.go`), so an empty `Sound` silences
osascript without affecting delivery. Under the default allow-list,
`webhookLevelAllowed` admits only `action_required` and `failure` when
`notifications.webhook.levels` is empty, and the Feishu path applies the
same check, so a hypothetical "routine"/"info" level would be persisted
as `level filtered` and never delivered to webhook/Feishu. The job
therefore uses `Level: action_required` (the least severe deliverable
level) with `Sound: ""`: webhook/Feishu deliver, osascript is silent, and
the in-app `NotificationRecord` is always persisted regardless of level
filtering. This labels the digest as `action_required`, which is the only
way to deliver it through the default allow-list without a config change;
a deployment that wants a less alarming level must extend the level
vocabulary and allow-list (out of scope for this derived projection). The
gateway persists an in-app `NotificationRecord` regardless of push-channel
success, so the digest content survives a webhook/osascript failure.

When `EmptyDayMode = "quiet"` and the assembled window has no qualifying
events, the job does **not** call `Gateway.Notify`: it writes the same
date/config-fingerprinted key directly through the existing
`NotificationsRepository` as an `in_app` invocation marker (for example,
`Status: "quiet"`, with the empty-day result in `PayloadJSON`). This marker
is durable evidence that the scheduled invocation completed even though no
push was requested, and it is consumed by the catch-up and activation-boundary
checks below exactly like the marker produced by a non-quiet `Notify` call.
The marker write is required to succeed before the job advances
`lastDigestDate`; a storage error leaves the date eligible for a later retry
and never fabricates activation evidence. This reuses the existing
notifications record rather than adding a digest table or delivery ledger.

The dashboard section is backed by a new HTTP route
`GET /api/v1/digest?date=YYYY-MM-DD` (default: the most recent digest date
`D` whose window has closed — yesterday under defaults) in
`internal/api/handler.go`, following the existing
`/api/v1/projects` list-route pattern. This route is part of the
repository's frozen HTTP compatibility boundary, so the implementation
must register it in
`internal/api/testdata/contracts/daemon-http.compat.json` (the
hand-maintained route/auth map), add deterministic response and error
captures under `internal/api/testdata/contracts/`, and register the
capture IDs in `contract_artifact_regen_test.go` so
`go generate ./internal/api/...` regenerates the artifacts; an ordinary
handler test does not add the endpoint automatically (see Validation).

**The route rejects unclosed windows.** The design requires every queried
window to be completed, but the explicit `date` parameter permits today
or a future date: at 09:00 a request for today's date would assemble only
the first nine hours of `[today 00:00, tomorrow 00:00)` and present that
incomplete slice as the day's digest. The handler therefore validates
that the requested window end `(D+1) 00:00 in tz` is at or before the
request instant (`now`): a `date` whose window has not closed (today or
any future date, and any date whose `(D+1) 00:00 in tz` is after `now`)
is rejected with a `422 Unprocessable Content` (or `400 Bad Request`,
consistent with the existing handler error convention) carrying a message
naming the earliest acceptable date, and no `ListBetween` query is run.
The default (no `date`) selects the most recent closed `D` and is always
acceptable. This keeps the dashboard from presenting a partial day as a
complete digest.

The handler re-assembles the same four sections on demand from the same
`ListBetween` storage query over the **same fixed date-derived window**
(`[D 00:00 in tz, (D+1) 00:00 in tz)`), so a failed push delivery never
loses the digest — the human can always pull it from the dashboard. The
dashboard is a **current projection**, not a byte-identical replay of the
pushed body: values that depend on `now`
(Awaiting-human age is `now − firstBlockedEvaluatedAt`) or on the latest
snapshot (PR title, diff size) reflect the request time and the latest
stored records, not the push time. So a request even a minute after
delivery renders a different age, and a snapshot updated after the push
can change the title/diff fields; the dashboard body is therefore **not**
byte-identical to what the job pushed. Exact push-time fidelity would
require persisting the rendered payload and an as-of timestamp at push
time — a new persisted artifact this design explicitly avoids; the
dashboard instead re-derives over the same window on demand. The dashboard
page (`web/dashboard/src`) adds a `/digest` route and a `DigestPage`
component following the existing page pattern (`useDashboardData`, table
render), and adds a `Digest` entry to the fixed `navItems` list in
`web/dashboard/src/components/layout/Shell.tsx` so the page is discoverable.
The route/navigation regression test must assert both that `/digest` renders
the page and that the navigation link is present/active. The dashboard fetches
the digest on-demand (not polled) since it is a daily artifact.

## Trade-off: derived projection, no persisted digest state

> Delete this six months from now — what breaks?

The digest disappears from push channels but not from the dashboard, which
re-derives it on demand. Nothing else depends on a persisted digest record.
Deleting the feature removes the scheduled push and the dashboard section;
the underlying events, snapshots, and gate reports remain the source of
truth.

> What does it still not catch?

A fixed calendar-day window can split a merge that happened at 08:59 from
the review that happened at 09:01 the previous day across two digests, and
a daemon down across a calendar day can skip that day's regular fire
(though the startup catch-up recovers it within the 7-day horizon). It
does not catch drift that compounds over weeks — that needs the human to
read consecutive digests, which is the intended audit cadence, not a
per-digest guarantee. It also does not catch merges that leave no durable
merge event (reviewer auto-merge, manual maintainer merges); see Merge
authority coverage.

Why the simpler alternative is insufficient: persisting a "last digest
delivered at" timestamp to bound the window would add a state field that
must stay in sync with the schedule and survive restarts, with a test that
fails when it goes stale. A fixed date-derived window needs none of that
and is correct for a daily audit: the worst case is a shifted boundary, not
a missed or duplicated digest. Same-date restart idempotence is bounded by
the already-persisted notification log, not by a new timestamp.

## Trade-off: delivery failure does not lose the digest

The issue requires that a delivery failure not lose the digest. The
solution is "persist," not "retry": the gateway already persists an in-app
`NotificationRecord` for every `Notify` call regardless of push-channel
outcome, and the dashboard re-assembles the digest from storage on demand.
A failed webhook or osascript delivery therefore leaves the digest visible
in two places (in-app notification log, dashboard) without a retry queue.
A retry queue would be a new durability layer over a derived projection —
exactly the layer this design avoids.

## Authority statement

The digest is a read-only projection. Its authority is the existing durable
event log and snapshots — the same records the gatekeeper and reviewer
already write as audit evidence. The digest adds no gate, no validation
step, and no "verify before acting" check on any agent-driven action; it
does not influence merge, dispatch, or routing. There is no new authority
to name because the digest does not act. The storage changes — a bounded
range query on `event_logs` and a payload extension on `pr.review.posted`
— read and record existing evidence; they do not create a new authority
surface. The additive `event_logs.seq` migration is storage ordering metadata
only; it does not become a digest authority or materialized state. The range
query uses the existing `event_logs` indexes (see Digest window). The migration
does add a backfill and append-allocation cost; the simpler implicit `rowid`
alternative was rejected because VACUUM/backup restore can renumber it and
change which equal-timestamp event a backward scan treats as latest.

## Risks

- **Merge authority coverage.** Only `gatekeeper.confirmAndMerge` emits a
  durable merge event. PRs merged via reviewer auto-merge
  (`EnableAutoMerge`) or by a maintainer manually are not in the Merged
  section. A deployment that does not use gatekeeper auto-merge will see a
  "nothing merged" digest despite actual merges. Covering those paths
  requires a durable merge observation shared by all merge paths, which is
  a separate, larger change and is intentionally out of scope for this
  derived projection.
- **#462/#464 dependency.** The closed-and-regenerated section reads the
  `pull_request.closed_and_regenerated` transition event and its
  `pull_request.closed_and_regenerated.retry_completed` follow-up defined
  in Closed-and-regenerated. The event contract (two types, identity
  fields, `discoveryFingerprint`, `retryOutcome` enum, dedup) is fixed in
  this spec and locked by a fixture and a producer-consumer integration
  test, so a producer that emits a different shape fails the test rather
  than silently rendering the section empty. If #462/#464 have not
  landed, the section renders empty with a note naming the missing event
  types.
- **Snapshot head match.** PR title, review state, and diff size come
  from the snapshot matching the merge event's `HeadSHA`
  (`GetLatestByProjectAndHead`). If no snapshot matches the merged head
  (e.g. gatekeeper merged head B before snapshot capture caught up),
  those snapshot-owned fields are rendered unavailable rather than read
  from a snapshot for a different head; the PR's `Repo` and `PRNumber`
  are still rendered from the `MergeOutcome` event, so the digest can
  identify and link the merged PR even without a snapshot. There is no
  recency fallback across heads.
- **Diff size truncation.** When the snapshot's diff was truncated
  (`diffTruncated: true`), diff size is rendered "unavailable (truncated)"
  rather than counted from an incomplete diff. This is an honest
  unavailable result, not a guess.
- **Window boundary shifts.** A late fire does not shift the window: the
  window is fixed to the digest date's calendar day. A daemon down across
  a calendar day skips that date's regular fire, but the startup catch-up
  (see Scheduling) recovers missed dates within the 7-day horizon from
  durable notification history; dates older than the horizon are accepted
  as lost.
- **Dashboard re-assembly cost.** Re-running the assembly on every
  `/api/v1/digest` request scans `EventsRepository.ListBetween` for the
  date's fixed calendar-day window. The scan is bounded on both sides by
  the date, but without a `created_at`-leading index the range predicate scans
  rows from `since` forward and sorts in memory, so cost grows with total
  `event_logs` history rather than with the one-day window. Accepted for an
  on-demand (non-polled) endpoint on small/medium histories; a
  `created_at`-leading index is a future optimization. No caching layer is
  added.
- **Routine digest labeled `action_required`.** The default webhook/Feishu
  allow-list admits only `action_required` and `failure`, so the digest
  uses `Level: action_required` with `Sound: ""` to deliver while staying
  silent. This labels a routine audit digest as action-required on
  webhook/Feishu, which may over-signal urgency. The trade-off is
  documented in Delivery; a less alarming level requires extending the
  level vocabulary and allow-list (out of scope).
- **Awaiting-human stale state for non-durable closes.** Membership
  excludes a PR when a durable `MergeOutcomeEventType` event with
  `Merged == true` follows its last blocked report (gatekeeper merge).
  PRs closed or merged via paths that emit no durable event (manual
  maintainer merge, reviewer auto-merge) have no durable close evidence,
  so they remain listed until a subsequent gate report or durable merge
  event supersedes the blocked report. The section renders the last
  blocked report's `EvaluatedAt` so the staleness is visible to the
  human; the reconciliation path is a later gate report or durable merge
  event. A durable "still open" signal shared by all merge paths would
  remove this limitation and is a separate, larger change out of scope
  for this derived projection.

## Validation

Regression coverage (Go tests, fixture-based, no network):

- Assembly from fixtures: seed `EventLogRecord`s and
  `PullRequestSnapshotRecord`s for a day with merges, holds, a verdict
  flip, and a closed-and-regenerated retry; assert the four sections match
  expected content (titles, reason codes, ages, anomaly flags). The
  closed-and-regenerated fixture conforms to the
  `pull_request.closed_and_regenerated` and
  `pull_request.closed_and_regenerated.retry_completed` payload schema
  (type, identity, `discoveryFingerprint`, `retryOutcome`, `closedAt`) so
  a producer emitting different field names is detected. Include a `pr.review.posted`
  with `event=COMMENT` and `outcome=clean` to assert it is **not** flagged
  as a non-blocking anomaly, and one with `outcome=non_blocking` to assert
  it is.
- Event ordering tie-breaker: append successive gate/review/lifecycle events
  with the same millisecond `created_at`; assert every latest/consecutive
  selection follows the durable storage sequence (`seq`) rather than random
  event IDs or implicit `rowid`, including `ListBetween`, `ListByEntity`, and latest-per-project/entity
  enumeration.
- Originating issue resolution: seed a merge event with no `LoopID` and a
  matching `LoopRecord` reachable by `ListByProjectRepoAndPR` for the
  merge event's `ProjectID`; assert the originating issue is resolved.
  Seed a merge with no matching loop; assert the issue is omitted, not
  guessed. Seed the same `repo`/`prNumber` under two projects with
  distinct source issues, where the other project's loop is more
  recently updated; assert the digest attributes the merge project's own
  source issue (the lookup is scoped to the merge event's `ProjectID`),
  not the other project's issue. Seed two loops under the same project
  for the same `repo`/`prNumber` naming **different** source issues
  (retries or manually created loops that disagree); assert the issue is
  omitted rather than attributed to the newest loop — the conflict check
  inspects all candidates for distinct source issues before selecting.
  Seed two otherwise matching loops whose source issue numbers are equal but
  whose `IssueRepo` values differ; assert they are treated as two distinct
  source issues and the digest omits the ambiguous issue. Seed a cross-repo
  source issue (`IssueRepo` different from the merged `Repo`) and assert both
  the source repo/number and enterprise-aware issue link are rendered.
- Gate summary (confirming report selection): seed a successful merge
  (`Merged == true`, `ConfirmingReasons` empty) where the confirming
  `GateReportEventType` has `EvaluatedAt` **after** the merge event's
  `AttemptedAt` (as `confirmAndMerge` produces) but at or before the
  `MergeOutcomeEventType` event's `created_at`, plus an earlier first-pass
  `GateReport` before `AttemptedAt`; assert the digest selects the
  confirming report (matched to the merged head and project, bounded by
  the outcome event's `created_at`, not `AttemptedAt`) and reads its
  `Evidence`, not the first-pass report or the empty `ConfirmingReasons`.
  Seed the same `repo`/`prNumber` under two projects and a confirming
  `GateReport` under the other project; assert it is not selected (the
  report's `ProjectID` must equal the `MergeOutcome.ProjectID`). Seed a
  confirming `GateReport` whose `EvaluatedAt` lies in the previous day's
  digest window (confirmation started before midnight, forge merge
  finished after midnight); assert the report is still selected via
  `ListByEntity` through the outcome event, not lost to the
  `ListBetween` window boundary. Seed an unrelated webhook-triggered
  evaluation for the same project, head, and PR after the confirming report
  but before `persistMergeOutcome`; assert its different event-log
  `CorrelationID` is ignored and the report carrying the merge's confirming
  correlation is selected. A merge event without that correlation must render
  gate evidence unavailable rather than use timestamp-only matching.
- Reviewer verdict head match: seed a merge for head B with a preceding
  `pr.review.posted` carrying `outcome=clean` for head A (older) and one
  for head B carrying `outcome=blocking`; assert the verdict summary uses
  B's `blocking`, not A's `clean`. Seed a merge with no `pr.review.posted`
  matching the merged head; assert the verdict renders "unavailable", not
  the snapshot `ReviewState`. Seed the same `repo`/`prNumber` under two
  projects and a head-matched `pr.review.posted` under the other project;
  assert it is not selected (the review event's `ProjectID` must equal
  the `MergeOutcome.ProjectID`). Seed a merge at 00:05 whose head-matched
  review was posted at 23:55 the previous day (outside the digest's
  `ListBetween` window); assert the verdict is still resolved via
  `ListByEntity`, not rendered unavailable.
- Diff size (head-matched snapshot): seed a snapshot matching the merged
  head with a complete retained diff; assert additions/deletions are
  counted from it. Seed a snapshot whose retained diff contains added
  source lines beginning with `++` (producing diff lines beginning with
  `+++`) and removed source lines beginning with `--` (producing `---`);
  assert those hunk-content lines are **counted**, not excluded as file
  headers (counting is restricted to hunk bodies after a `@@` header,
  with the file-header grammar recognized). Seed a snapshot with
  `diffTruncated: true` for the merged head; assert diff size renders
  "unavailable (truncated)". Seed a newer snapshot for a different head
  and no snapshot for the merged head; assert diff size renders
  "unavailable" rather than counting the other head's diff.
- Snapshot head match (title and repo preservation): seed a merge for
  head B with a newer snapshot for head A and a matching snapshot for
  head B; assert the title comes from head B's snapshot. Seed a merge
  with no snapshot matching the merged head; assert the title renders
  "unavailable" but the **repo and PR number are still rendered from the
  `MergeOutcome` event** (the digest can still identify and link the
  merged PR), so only snapshot-owned fields are unavailable.
  Seed a host-qualified repo (`github.example.com/acme/looper`) and assert
  the rendered pull-request URL uses `https://github.example.com/...` rather
  than hard-coded `github.com`.
- Awaiting-human current state: seed a PR blocked at 08:00 on the **fire
  date** `F = D + 1` (i.e. 08:00 on `F`, which is after the digest date
  `D`'s window end `(D+1) 00:00 = F 00:00`), with no earlier in-window
  blocked report; assert it is listed in the 09:00 digest on `F`
  (membership is as of the assembly instant, not window-bounded). Seeding
  the report at 08:00 on the digest date `D` itself would place it
  *inside* `[D 00:00, (D+1) 00:00)` and would not exercise the
  post-window discovery path; the report must be on `F = D + 1` so the
  test actually proves candidate discovery and membership work beyond the
  digest window. Seed a PR whose hold was removed at 08:00 on `F` with
  its latest report no longer blocked; assert it is not listed. Seed
  consecutive blocked gate reports 30 minutes apart spanning several
  hours, with the first report **before** the window's `since`; assert
  the age is measured from that first report in the continuous blocked
  interval (read via `ListByEntity`), not from the latest report.
- Awaiting-human exclusion (role-matched): seed a `ReasonReviewRequired`
  gate report with an active **reviewer** loop for the PR; assert the PR
  is omitted. Seed a `ReasonReviewRequired` gate report with an active
  **fixer** loop (but no active reviewer) for the PR; assert the PR is
  **still listed** (a fixer cannot supply the missing approval). Seed a
  `ReasonReviewChangesRequested` (and separately
  `ReasonUnresolvedReviewThread`) gate report with an active **fixer**
  loop; assert the PR is omitted. Seed the same two reasons with an
  active **reviewer** loop (but no active fixer); assert the PR is
  **still listed** (a reviewer cannot resolve change-requested reviews
  or unresolved threads). Seed each reason with no active
  reviewer/fixer loop; assert it is included. Seed a `ReasonHold` gate
  report with an active fixer loop; assert it is still included.
- Awaiting-human durable-close exclusion: seed a PR whose latest gate
  report is blocked, then a later `MergeOutcomeEventType` event with
  `Merged == true` for the same PR (gatekeeper merge after the blocked
  report); assert the PR is omitted. Seed a PR whose latest blocked
  report has no subsequent durable merge event (simulating a manual
  maintainer close); assert the PR is still listed and the section
  renders the last blocked report's `EvaluatedAt` so the staleness is
  visible.
- Awaiting-human candidate enumeration: seed a PR first blocked at 08:00
  on the fire date `F = D + 1` with no gate event inside the digest
  date's `ListBetween` window; assert it is still discovered as a
  candidate via `ListLatestByProjectAndEntityTypeAndEventTypes` (latest
  report per `(ProjectID, entity_id)`) and listed in the 09:00 digest. Seed
  the same PR under two projects with different latest gate states and assert
  each project's candidate is evaluated independently. Seed many historical gate
  reports for already-merged PRs outside the candidate set; assert the
  candidate enumeration does not materialize them (only the latest
  report per project/entity is read, and each full lifecycle walk is
  project-scoped.
- Verdict-flip anomaly threshold: seed two `pr.review.posted` events for
  the merged head with differing `outcome` 30 minutes apart, both within
  `VerdictFlipWindowMinutes` of `AttemptedAt`; assert a flip is flagged.
  Seed a flip whose earlier event is outside the window; assert it is not
  flagged. Seed a flip on a non-merged head; assert it is not considered.
- Verdict-flip across the digest boundary: seed a merge at 00:30 with
  reviews at 23:30 the previous day and 00:10 (a valid 120-minute flip
  whose earlier review lies in the previous day's window); assert the
  flip is flagged (the per-PR history is read via `ListByEntity` over
  `[AttemptedAt − VerdictFlipWindowMinutes, AttemptedAt]`, not the
  date-bounded `ListBetween`).
- Non-blocking anomaly head and project match: seed a merge for head B
  with a `pr.review.posted` carrying `outcome=non_blocking` for head A
  (newer than any head-B review); assert the merge is **not** flagged
  (the anomaly requires the pre-merge review to match the merged head).
  Seed a merge for head B with a head-B `outcome=non_blocking` review;
  assert it is flagged. Seed the same `repo`/`prNumber` under two
  projects and a head-matched `outcome=non_blocking` review under the
  other project; assert the merge is **not** flagged (the review's
  `ProjectID` must equal the `MergeOutcome.ProjectID`). Seed a merge at
  00:05 whose head-matched, project-matched `non_blocking` review was
  posted at 23:55 the previous day (outside the digest window); assert
  the anomaly is still flagged via `ListByEntity`, not missed to the
  window boundary.
- Producer review-outcome persistence: run
  `recordPublishedReviewProgress` with a computed `outcome` and assert
  the persisted `pr.review.posted` event payload carries that `outcome`
  (producer-level test, not an assembly fixture).
- Producer review-outcome persistence failure path: inject an
  `eventlog.Append` error into `recordPublishedReviewProgress` after the
  `lastPublishedHeadSha` update is staged; assert the head-sha update rolls
  back with the missing event, then retry and assert the publish/event path is
  attempted again. A surfaced error without rollback is not sufficient because
  the committed head marker would suppress that retry.
- Empty-day behavior: no qualifying events in the window → `quiet` mode
  produces no `Notify` call but persists one `in_app` invocation marker with
  the date/config key; a marker-write failure leaves the date retryable.
  `notice` mode produces one payload with the "nothing merged" body and its
  `Notify` call persists the equivalent marker before fan-out.
- Delivery level: assert the payload uses `Level: action_required` with
  `Sound: ""`; assert webhook/Feishu channels deliver (not `level
  filtered`) and osascript does not play a sound.
- Delivery failure does not lose the digest: inject a webhook `HTTPPost`
  that returns an error; assert an in-app `NotificationRecord` is still
  persisted and the `/api/v1/digest` handler still returns the assembled
  digest.
- Invocation marker fail-closed: for a digest payload with
  `RequireDurableMarker: true`, inject an `in_app` persistence failure into
  `Gateway.Notify` (the `recordInApp` write fails); assert no push channel
  (osascript/webhook/Feishu) is invoked and no external delivery occurs. Add
  a non-digest payload with the flag unset and assert the existing best-effort
  fan-out remains, so a transient marker-write failure cannot break digest
  restart idempotence or silence unrelated alerts.
- Same-date restart idempotence: persist an `in_app` delivered
  notification record with `DedupeKey digest:<D>:<configFingerprint>`
  older than `ThrottleWindowSeconds`; fire the job under the same config;
  assert the push is skipped by the date-wide `in_app` notification-log
  check and no duplicate `Notify` occurs. Then change `Timezone` (or
  `VerdictFlipWindowMinutes`) and fire again for the same `D`; assert the
  push is **not** skipped (the config-fingerprinted key differs) and the
  new projection is delivered. Assert that when the `in_app` record
  exists but a push channel failed, the failed channel is **not**
  retried (the `in_app` invocation marker is the idempotence unit, not
  per-channel delivery).
- Config fingerprint drift: add a new `DigestConfig` field that the
  assembly reads and assert the test fails when that field is not
  included in the `configFingerprint` input, so a future output-affecting
  field cannot be silently omitted from the dedupe key.
- DST-affected fire times: in a timezone with a spring-forward transition
  where `time.Date` normalizes a nonexistent `FireTime` backward (e.g.
  `America/New_York` `2026-03-08 02:30` resolves to `01:30 EST`, not the
  `03:00` gap end), configure that `FireTime`; assert the scheduler
  detects the normalization mismatch, searches for the first valid
  instant explicitly, and fires once on that date at the gap-end
  substitute instant (`03:00`) and selects `D = F − 1`, so digest date
  `F − 1` is delivered and not lost and the fire does not occur before
  the configured wall time. In a timezone with a fall-back transition,
  configure a `FireTime` that occurs twice; assert the scheduler fires
  once (the explicitly selected first occurrence) and selects the correct
  `D`. Add a timezone whose transition skips local midnight (for example
  `America/Santiago` on 2026-09-06); assert the digest window starts at the
  first valid instant on the requested local date rather than a normalized
  instant on the prior date, and that the adjacent date windows do not
  overlap.
- Catch-up from durable history: with no invocation marker for `F−1` and an
  invocation marker for `F` (delivered or quiet), restart the daemon after
  midnight on `F+1`; assert the startup catch-up pushes `F−1` (missed) in
  chronological order before resuming the regular schedule, and does not
  re-push `F` (already invoked). Also restart at 00:01 on `F`, before the
  configured 09:00 fire,
  and assert `F−1` is not pushed early; the regular schedule later delivers
  it at 09:00. Assert dates older than the 7-day horizon are not pushed.
- Catch-up trailing missed dates: with an invocation marker for `F−2` only
  and no marker for `F−1` or `F`, restart the daemon after
  midnight on `F+1`; assert the startup catch-up pushes both `F−1` and
  `F` (the trailing missed dates at or after the oldest invocation marker
  `F−2`), in chronological order — a predicate that considers only dates
  at or before the newest marker would never push them.
- Catch-up activation boundary: on first enablement with no invocation
  marker anywhere in the 7-day horizon, restart/start the daemon
  and assert the catch-up pushes **nothing** (no historical backfill) and
-  `lastDigestDate` stays empty. Then seed an invocation marker for `F−1`
  only and restart after `F`'s scheduled fire has passed, with no marker for
  `F`; assert `F` is pushed (the `F−1` marker is the activation boundary
  proving the feature was active) but dates before the oldest marker are not
  treated as missed. A marker for `F` alone must
  not make `F−1` eligible, because it cannot prove activation for that
  earlier date.
- First scheduled fire not suppressed: on first enablement at 08:00 with
  the default 09:00 fire time and no invocation markers, start the daemon
  and assert the regular schedule fires 09:00 for `D = F − 1` (the guard
  is not pre-seeded with the closed-but-undelivered `F − 1`), so the
  first scheduled digest is delivered rather than permanently skipped.
  Start a second daemon at 10:00 (after the 09:00 fire has passed) with
  no invocation markers and assert it waits for tomorrow's 09:00 fire for
  `D = F` without pre-seeding the guard.
- Completed-window selection: under defaults (`FireTime = "09:00"`), fire
  the job on day `F` and assert it selects `D = F − 1` and queries
  `[F−1 00:00, F 00:00)`; assert an event at `F 00:00` belongs to the
  next day's digest, not this one.
- Window reproducibility: request `/api/v1/digest?date=<D>` and assert the
  handler uses the same fixed `[D 00:00, (D+1) 00:00 in tz)` window the
  job would use, with an exclusive end bound (an event at exactly
  `(D+1) 00:00` is excluded). Assert `end(D)` is `(D+1) 00:00` constructed
  via the configured location (assert correct behavior on a DST
  transition date).
- Unclosed-window rejection: at 09:00 on day `F`, request
  `/api/v1/digest?date=<F>` (today) and `/api/v1/digest?date=<F+1>` (a
  future date); assert both are rejected with a 4xx error and no
  `ListBetween` query is run. Request `/api/v1/digest` with no `date`
  (default) and `/api/v1/digest?date=<F−1>` (a closed window); assert
  both succeed.
- Dashboard is a current projection: push a digest, then advance `now` by
  one minute and request `/api/v1/digest?date=<D>`; assert the
  Awaiting-human age differs from the pushed body (the dashboard is not
  byte-identical), while the merged/closed sections over the fixed window
  match.
- Lifecycle producer-consumer integration: emit a real
  `pull_request.closed_and_regenerated` transition event and its
  `pull_request.closed_and_regenerated.retry_completed` follow-up through
  the producer path #462/#464 define and assemble the digest; assert the
  section renders `discoveryFingerprint` and the terminal `retryOutcome`
  from the emitted completion event. Emit events with mismatched field
  names and assert the test fails rather than the section silently
  rendering empty.
- Closed-and-regenerated pending reaches terminal state: seed a
  transition event with `retryOutcome = pending` and no completion event;
  assert the section renders the retry as "in progress (pending)". Then
  emit the matching `retry_completed` event with `retryOutcome =
  succeeded` (and separately `failed`); assert the section now renders
  the terminal outcome, not `pending` — this fails if `pending` remains
  stale after the completion event lands.
- Closed-and-regenerated cross-boundary stale state: seed a transition
  event near midnight on digest date `D` (inside the `ListBetween`
  window) with `retryOutcome = pending`, and its matching
  `retry_completed` event (terminal `succeeded`) at 00:30 on `D + 1`
  (outside the digest date's `ListBetween` window); assert the digest for
  `D` renders the terminal `succeeded` outcome, not `pending` — the
  completion event is read via `ListByEntity` through the assembly
  instant, not the windowed `ListBetween` result, so a completion that
  lands after `(D+1) 00:00` is not lost. This fails if the section reads
  completion events only from the digest window.
- HTTP contract registration: assert the `/api/v1/digest` route is
  present in `internal/api/testdata/contracts/daemon-http.compat.json`,
  has deterministic response and error captures, and that
  `go generate ./internal/api/...` regenerates them from the capture IDs
  registered in `contract_artifact_regen_test.go`.
- Config validation: malformed `FireTime`, unknown `Timezone`, unknown
  `EmptyDayMode`, `VerdictFlipWindowMinutes <= 0`, and
  `VerdictFlipWindowMinutes` exceeding `math.MaxInt64 / int64(time.Minute)`
  (153,722,867) all fail fast at boot; a value just under the threshold is
  accepted.

Contract coverage for the lifecycle: the digest goroutine starts with
`Runtime.Start` and stops cleanly on the stop channel (no leak across
restarts), following the existing scheduler-loop lifecycle pattern.

Repository validation is `gofmt -l .`, `go vet ./...`, production-only
staticcheck (`go run honnef.co/go/tools/cmd/staticcheck@v0.6.1
-tests=false -checks='U1000,SA1006,SA4004,SA4006' ./...`, matching the
pinned version and check set the documented PR verification pipeline runs
between `go vet` and `go test`), `go test ./...`, and `go build ./...`.
Dashboard changes additionally run `pnpm install`, `pnpm test`, and
`pnpm build` per CI.
