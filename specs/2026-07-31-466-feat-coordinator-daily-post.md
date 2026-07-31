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
- New persisted tables, ledgers, status columns, or migrations. The only
  storage changes are a new bounded range query on the existing `event_logs`
  table (see Digest window) and a small payload extension to the existing
  `pr.review.posted` event (see Sources and assembly). No new index or other
  migration is added: the PR objective prohibits migrations, so the range
  query uses the existing `event_logs` indexes and documents its cost (see
  Digest window and Risks) rather than adding a `created_at`-leading index.
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
  `SELECT * FROM event_logs WHERE created_at >= ? AND created_at < ? ORDER BY created_at ASC, id ASC`.
  The existing `ListSince` applies only `created_at >= since` and is not
  used: an unbounded historical scan would let a `/api/v1/digest?date=`
  request for an old date read every event from that date through the
  present before any in-memory day filtering, so endpoint cost would grow
  with database history. `ListBetween` bounds the scan on both sides for
  both the job and the dashboard route. The existing `event_logs` indexes
  are `(entity_type, entity_id, created_at)` and `(event_type,
  created_at)`; neither has a `created_at` leading column, and the PR
  objective prohibits migrations, so no `created_at`-leading index is
  added. The range predicate therefore scans rows from `since` forward and
  sorts in memory; the cost grows with total `event_logs` history rather
  than with the one-day window. This is an accepted, documented trade-off
  for a derived projection that adds no storage: the daily job scans once
  per day, and the dashboard route is on-demand (non-polled). A
  `created_at`-leading index is a future optimization outside this PR's
  no-migration scope (see Risks).
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

- PR title and repo from the head-matched snapshot
  (`GetLatestByProjectAndHead` for the merge event's `HeadSHA`); when no
  snapshot matches the merged head, the title is rendered unavailable
  rather than read from a newer snapshot for a different head.
- Originating issue: resolved via `LoopsRepository.ListByRepoAndPR(repo,
  prNumber)` (`internal/storage/repositories.go`) using the merge event's
  own `Repo`/`PRNumber` identity — the loop's source issue is the
  originating issue. The merge event is appended by
  `persistMergeOutcome` with only `ProjectID`, `EntityType`, and
  `EntityID`; it carries no `LoopID`, so a `merge event → LoopRecord` join
  on `LoopID` is not possible. `ListByRepoAndPR` returns loops ordered by
  `updated_at DESC, seq DESC`; the assembly picks the most recent loop
  whose source issue is set. If multiple loops conflict or none is
  recoverable, the issue is omitted rather than guessed.
- Reviewer verdict summary: the most recent `pr.review.posted` at or before
  the merge attempt for the same PR **whose `headSha` equals the merge
  event's `HeadSHA`** (both payloads already carry the head), read from its
  persisted `outcome` (`clean` / `non_blocking` / `blocking`). Matching the
  head prevents attributing a verdict for an obsolete commit to the merged
  code: after Looper posts `outcome=clean` for head A, a push creates head
  B and gatekeeper may merge B, so a timestamp-only lookup would wrongly
  report A's clean verdict for B. If no `pr.review.posted` event matches
  the merged head — including a historical event whose payload predates the
  `outcome` extension and lacks it, or a review event outside the queried
  window — the verdict is rendered **unavailable** rather than falling back
  to the snapshot `ReviewState` (see above): `ReviewState` is GitHub's
  aggregate decision, not Looper's outcome, so presenting it as the verdict
  would be a guess.
- Gatekeeper gate summary at merge time: the confirming `GateReport`
  evidence. `MergeOutcome.AttemptedAt` is stamped **before**
  `confirmAndMerge` calls `EvaluatePullRequest` for the confirming pass
  (`internal/gatekeeper/merge.go`), and that confirming evaluation
  persists the `GateReport` that proves the merge was eligible, so the
  confirming report's `EvaluatedAt` is **after** `AttemptedAt`. Selecting
  the most recent report "at or before `AttemptedAt`" would therefore
  return the earlier first-pass report and exclude the actual confirming
  evidence. The assembly instead selects the most recent
  `GateReportEventType` event for the PR **whose head matches the merged
  `HeadSHA`** and whose `EvaluatedAt` is at or before the
  `MergeOutcomeEventType` event's own persisted `created_at` (the outcome
  event bounds the confirming report, which is persisted before the
  outcome is appended). `MergeOutcome.ConfirmingReasons` is **not** used:
  it is populated only when the confirming evaluation becomes blocked, in
  which case `Merged` remains `false`, so every event selected here with
  `Merged == true` has an empty `ConfirmingReasons`. A successful merge
  therefore has no blocking-reason summary by definition. The digest
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
  content lines (excluding `+++`/`---` file headers). The raw diff belongs
  to the snapshot's `HeadSHA`, so the count is only valid for the merged
  head when the snapshot was selected by `GetLatestByProjectAndHead` for
  that head; counting the latest snapshot regardless of head would report
  another commit's additions/deletions as the merged head's diff size.
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

### Closed-and-regenerated (#462/#464)

This section depends on the close-and-regenerate tracking that #462/#464
introduce. The digest does not own that concept; it reads a named
lifecycle event that #462/#464 emit at the transition. To prevent the
section from silently rendering empty when the producer's field names
diverge from what the digest expects, the event contract is fixed here and
locked by a fixture before the digest assembly is implemented.

**Event contract — `pull_request.closed_and_regenerated`:**

- Event type: `pull_request.closed_and_regenerated` (a new
  `event_logs.event_type`, emitted by #462/#464 at the close-and-regenerate
  transition).
- Entity: `entity_type = "pull_request"`,
  `entity_id = "<repo>#<prNumber>"`.
- Payload fields:
  - `repo` (string), `prNumber` (int), `headSha` (string) — PR identity,
    matching the identity the gatekeeper and reviewer events already carry.
  - `discoveryFingerprint` (string) — the fingerprint of the discovery
    that failed and triggered regeneration, observed at transition time.
  - `retryOutcome` (enum string): `succeeded` | `failed` | `pending` —
    whether the retry loop that followed the regeneration succeeded,
    failed, or had not completed when the event was emitted.
  - `closedAt` (RFC3339 string) — the transition timestamp.
- Deduplication: one event per `(entity_id, discoveryFingerprint,
  closedAt)` transition; #462/#464 are responsible for not emitting
  duplicates for the same transition (the digest does not dedupe producer
  output).

The assembly lists PRs in the window that were closed by the regenerate
flow and the retry loop that followed, with the failure fingerprint and
retry outcome. The fingerprint and retry outcome come from this event's
own payload, not from a join against the mutable `LoopRecord`.
`autonomousRecovery.lastFailedDiscoveryFingerprint` on
`LoopRecord.MetadataJSON` is a mutable "last" value: a later failure can
overwrite it, recovery can clear it, and the loop's completion status can
change after another retry, so a dashboard request issued later could
attach a newer fingerprint or outcome to an older close event. The
lifecycle event carries both values observed at transition time; the
digest reads `discoveryFingerprint` and `retryOutcome` directly. If
#462/#464 have not yet persisted an event of this type with these fields,
the section renders empty with a note naming the missing event type and
fills in once they do.

**Producer-consumer contract coverage.** Because the producer (#462/#464)
is separate work, an assembly fixture that seeds a prebuilt
`EventLogRecord` cannot detect producer/consumer drift — the section could
ship without a working producer. The validation therefore includes an
integration test that emits the real `pull_request.closed_and_regenerated`
transition event through the producer path #462/#464 define and then
assembles the digest, asserting the section renders the fingerprint and
`retryOutcome` from the emitted event. The fixture and the integration
test both conform to the payload schema above, so a producer that emits
different field names fails the test rather than silently producing an
empty section.

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

The backward walk and the membership check both read the PR's full
lifecycle via `EventsRepository.ListByEntity(ctx, "pull_request",
entityID)` (ordered `created_at ASC`), not the windowed `ListBetween`
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

**Exclude work automation is still handling.** The active-automation
correlation applies to every automation-handled reason, not only
`ReasonReviewRequired`. `ReasonReviewRequired` just means GitHub currently
lacks a required approval; an active reviewer loop addresses it.
`ReasonReviewChangesRequested` and `ReasonUnresolvedReviewThread` are
addressed by an active fixer loop (change-requested reviews and unresolved
review threads are fixer work). Listing any of these under "Awaiting human"
while a queued or running loop is actively handling the PR would report
ordinary in-progress automation as human-actionable. Before classifying
any of `ReasonReviewRequired`, `ReasonReviewChangesRequested`, or
`ReasonUnresolvedReviewThread` as human-actionable, the assembly correlates
the gate report with active reviewer/fixer work via
`LoopsRepository.ListByRepoAndPR(repo, prNumber)`: if any returned loop
has an active reviewer or fixer status (queued or running) for the PR, the
PR is omitted from this section. Only `ReasonHold` — an explicit
human-applied hold label — is human-actionable regardless of active
automation.

### Anomalies

Two signals, both derived from existing records and classified by the
persisted reviewer `outcome`, not the GitHub review event type:

- Merges where the most recent `pr.review.posted` before merge **for the
  merged `headSha`** carried `outcome=non_blocking` (a non-blocking review
  with actionable feedback that did not block merge). The same head-match
  rule as the Merged verdict summary applies: a `non_blocking` review for
  head A must not flag a merge of head B. A `COMMENT` event alone is not
  an anomaly: under the default policy a clean review may also publish as
  `COMMENT`, so only the structured `outcome` distinguishes them.
- Merges where the reviewer verdict flipped (e.g. `blocking` → `clean`,
  or `non_blocking` → `clean`) within a fixed window before the merge
  attempt. The window is `VerdictFlipWindowMinutes` (default 120, a field
  on `DigestConfig`, see Configuration) anchored to the merge event's
  `AttemptedAt`: only `pr.review.posted` events for the PR whose `headSha`
  equals the merged head and whose timestamp falls in
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
  on a spring-forward day that jumps 02:00 → 03:00), the scheduler does
  not fire on that day. It computes the next fire instant as the next
  calendar day's `FireTime` and fires once there. The skipped day still
  produces a digest on the following fire: that fire selects `D` = the
  skipped day's date (its window has closed) and pushes it, so no digest
  date is lost — only the fire instant is shifted by one day.
- **Fall-back (repeated local time):** if `FireTime` occurs twice on a
  given calendar day (e.g. `01:30` on a fall-back day that repeats
  01:00–02:00), the scheduler fires **once**, at the first occurrence
  (the earlier UTC instant). Firing twice would select the same `D` and
  rely on dedupe; firing once at the first occurrence is deterministic
  and matches the once-per-day contract.

Both cases are covered by tests asserting the selected digest date and
the fire count (one fire per calendar day that has a valid fire instant;
the spring-forward day fires zero times and its digest is produced by the
next day's fire).

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
and for each date `d` in that range queries
`NotificationsRepository.GetLatestByDedupe(ctx, channel,
"digest:<d>:<configFingerprint>")` (see Same-date delivery idempotence
for the key). Any date with no delivered record is pushed, in
chronological order, before the regular schedule resumes. The horizon is
bounded so a long outage does not flood the channel on restart; dates
older than the horizon are accepted as lost (an audit surface, not a
retry queue). The in-memory `lastDigestDate` is then set to the newest
pushed date so the regular schedule does not re-fire it.

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
configuration that affects digest output. For each configured channel the
job queries `NotificationsRepository.GetLatestByDedupe(ctx, channel,
"digest:<D>:<configFingerprint>")` and skips the push when a delivered
record exists for that key, regardless of age. This bounds restart
re-fires using already-persisted state — no new "last digest delivered
at" timestamp is introduced. The gateway's short throttle still
suppresses a same-process double-fire within the throttle window; the
date-wide notification-log check is the durable backstop that covers
restarts beyond that window. The dashboard route reconstructs the window
from the daemon's current config, so after a config change the
date-only route re-derives under the new config (a current projection,
see Delivery); the config-fingerprinted dedupe key ensures the new
projection is delivered rather than suppressed as a duplicate of the old
one.

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
`Timezone`/`EmptyDayMode`, and a non-positive
`VerdictFlipWindowMinutes` (required: `> 0`; zero or negative makes the
verdict-flip anomaly ill-defined), consistent with looperd's existing
fail-fast-on-config-validation behavior. There is no `LookbackHours`
field: the digest window is always the calendar day in the configured
timezone (see Digest window), so a window-length knob is not exposed.
TOML example:

```toml
[digest]
enabled = false
fireTime = "09:00"
timezone = "UTC"
emptyDayMode = "quiet"
verdictFlipWindowMinutes = 120
```

## Delivery

One `Gateway.Notify(ctx, payload)` call per digest
(`internal/infra/notify/gateway.go:191`), with a `SystemNotificationPayload`
whose `Body` carries the rendered digest and whose `DedupeKey` is
`digest:<D>:<configFingerprint>` (the digest date plus the effective-config
fingerprint over `Timezone` and `VerdictFlipWindowMinutes`, see Scheduling)
so a same-date re-fire under the same config is deduped by the gateway's
existing throttle/dedupe path within the throttle window, while a config
change produces a new key and is delivered rather than suppressed; the
date-wide notification-log check in Scheduling is the durable backstop.
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
handler test does not add the endpoint automatically (see Validation). The
handler re-assembles the same four sections on demand from the same
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
render). The dashboard fetches the digest on-demand (not polled) since it
is a daily artifact.

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
surface. No migration is added (the PR objective prohibits migrations);
the range query uses the existing `event_logs` indexes (see Digest
window).

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
  `pull_request.closed_and_regenerated` event defined in Closed-and-
  regenerated. The event contract (type, identity fields,
  `discoveryFingerprint`, `retryOutcome` enum, dedup) is fixed in this
  spec and locked by a fixture and a producer-consumer integration test,
  so a producer that emits a different shape fails the test rather than
  silently rendering the section empty. If #462/#464 have not landed, the
  section renders empty with a note naming the missing event type.
- **Snapshot head match.** PR title, review state, and diff size come
  from the snapshot matching the merge event's `HeadSHA`
  (`GetLatestByProjectAndHead`). If no snapshot matches the merged head
  (e.g. gatekeeper merged head B before snapshot capture caught up), those
  fields are rendered unavailable rather than read from a snapshot for a
  different head. There is no recency fallback across heads.
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
  the date, but without a `created_at`-leading index (no migration is
  added) the range predicate scans rows from `since` forward and sorts in
  memory, so cost grows with total `event_logs` history rather than with
  the one-day window. Accepted for an on-demand (non-polled) endpoint on
  small/medium histories; a `created_at`-leading index is a future
  optimization outside this PR's no-migration scope. No caching layer is
  added.
- **Routine digest labeled `action_required`.** The default webhook/Feishu
  allow-list admits only `action_required` and `failure`, so the digest
  uses `Level: action_required` with `Sound: ""` to deliver while staying
  silent. This labels a routine audit digest as action-required on
  webhook/Feishu, which may over-signal urgency. The trade-off is
  documented in Delivery; a less alarming level requires extending the
  level vocabulary and allow-list (out of scope).

## Validation

Regression coverage (Go tests, fixture-based, no network):

- Assembly from fixtures: seed `EventLogRecord`s and
  `PullRequestSnapshotRecord`s for a day with merges, holds, a verdict
  flip, and a closed-and-regenerated retry; assert the four sections match
  expected content (titles, reason codes, ages, anomaly flags). The
  closed-and-regenerated fixture conforms to the
  `pull_request.closed_and_regenerated` payload schema (type, identity,
  `discoveryFingerprint`, `retryOutcome`, `closedAt`) so a producer
  emitting different field names is detected. Include a `pr.review.posted`
  with `event=COMMENT` and `outcome=clean` to assert it is **not** flagged
  as a non-blocking anomaly, and one with `outcome=non_blocking` to assert
  it is.
- Originating issue resolution: seed a merge event with no `LoopID` and a
  matching `LoopRecord` reachable by `ListByRepoAndPR`; assert the
  originating issue is resolved. Seed a merge with no matching loop; assert
  the issue is omitted, not guessed.
- Gate summary (confirming report selection): seed a successful merge
  (`Merged == true`, `ConfirmingReasons` empty) where the confirming
  `GateReportEventType` has `EvaluatedAt` **after** the merge event's
  `AttemptedAt` (as `confirmAndMerge` produces) but at or before the
  `MergeOutcomeEventType` event's `created_at`, plus an earlier first-pass
  `GateReport` before `AttemptedAt`; assert the digest selects the
  confirming report (matched to the merged head and bounded by the
  outcome event's `created_at`, not `AttemptedAt`) and reads its
  `Evidence`, not the first-pass report or the empty `ConfirmingReasons`.
- Reviewer verdict head match: seed a merge for head B with a preceding
  `pr.review.posted` carrying `outcome=clean` for head A (older) and one
  for head B carrying `outcome=blocking`; assert the verdict summary uses
  B's `blocking`, not A's `clean`. Seed a merge with no `pr.review.posted`
  matching the merged head; assert the verdict renders "unavailable", not
  the snapshot `ReviewState`.
- Diff size (head-matched snapshot): seed a snapshot matching the merged
  head with a complete retained diff; assert additions/deletions are
  counted from it. Seed a snapshot with `diffTruncated: true` for the
  merged head; assert diff size renders "unavailable (truncated)". Seed a
  newer snapshot for a different head and no snapshot for the merged head;
  assert diff size renders "unavailable" rather than counting the other
  head's diff.
- Snapshot head match (title): seed a merge for head B with a newer
  snapshot for head A and a matching snapshot for head B; assert the
  title comes from head B's snapshot. Seed a merge with no snapshot
  matching the merged head; assert the title renders "unavailable".
- Awaiting-human current state: seed a PR blocked at 08:00 on the digest
  date (after the window's midnight `until`) with no earlier in-window
  blocked report; assert it is listed in the 09:00 digest (membership is
  as of the assembly instant, not window-bounded). Seed a PR whose hold
  was removed at 08:00 with its latest report no longer blocked; assert
  it is not listed. Seed consecutive blocked gate reports 30 minutes
  apart spanning several hours, with the first report **before** the
  window's `since`; assert the age is measured from that first report in
  the continuous blocked interval (read via `ListByEntity`), not from the
  latest report.
- Awaiting-human exclusion: seed a `ReasonReviewRequired` gate report with
  an active reviewer loop for the PR; assert the PR is omitted. Seed a
  `ReasonReviewChangesRequested` (and separately `ReasonUnresolvedReviewThread`)
  gate report with an active fixer loop; assert the PR is omitted. Seed
  each with no active reviewer/fixer loop; assert it is included. Seed a
  `ReasonHold` gate report with an active fixer loop; assert it is still
  included.
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
- Non-blocking anomaly head match: seed a merge for head B with a
  `pr.review.posted` carrying `outcome=non_blocking` for head A (newer
  than any head-B review); assert the merge is **not** flagged (the
  anomaly requires the pre-merge review to match the merged head). Seed a
  merge for head B with a head-B `outcome=non_blocking` review; assert it
  is flagged.
- Producer review-outcome persistence: run
  `recordPublishedReviewProgress` with a computed `outcome` and assert
  the persisted `pr.review.posted` event payload carries that `outcome`
  (producer-level test, not an assembly fixture).
- Empty-day behavior: no qualifying events in the window → `quiet` mode
  produces no `Notify` call; `notice` mode produces one payload with the
  "nothing merged" body.
- Delivery level: assert the payload uses `Level: action_required` with
  `Sound: ""`; assert webhook/Feishu channels deliver (not `level
  filtered`) and osascript does not play a sound.
- Delivery failure does not lose the digest: inject a webhook `HTTPPost`
  that returns an error; assert an in-app `NotificationRecord` is still
  persisted and the `/api/v1/digest` handler still returns the assembled
  digest.
- Same-date restart idempotence: persist a delivered notification record
  with `DedupeKey digest:<D>:<configFingerprint>` older than
  `ThrottleWindowSeconds`; fire the job under the same config; assert the
  push is skipped by the date-wide notification-log check and no
  duplicate `Notify` occurs. Then change `Timezone` (or
  `VerdictFlipWindowMinutes`) and fire again for the same `D`; assert the
  push is **not** skipped (the config-fingerprinted key differs) and the
  new projection is delivered.
- DST-affected fire times: in a timezone with a spring-forward transition,
  configure a `FireTime` that is nonexistent on the transition date;
  assert the scheduler fires zero times on that date and fires once on the
  next day, selecting `D` = the skipped date. In a timezone with a
  fall-back transition, configure a `FireTime` that occurs twice; assert
  the scheduler fires once (the first occurrence) and selects the correct
  `D`.
- Catch-up from durable history: with no delivered digest for `F−1` and a
  delivered record for `F`, restart the daemon after midnight on `F+1`;
  assert the startup catch-up pushes `F−1` (missed) in chronological order
  before resuming the regular schedule, and does not re-push `F` (already
  delivered). Assert dates older than the 7-day horizon are not pushed.
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
- Dashboard is a current projection: push a digest, then advance `now` by
  one minute and request `/api/v1/digest?date=<D>`; assert the
  Awaiting-human age differs from the pushed body (the dashboard is not
  byte-identical), while the merged/closed sections over the fixed window
  match.
- Lifecycle producer-consumer integration: emit a real
  `pull_request.closed_and_regenerated` transition event through the
  producer path #462/#464 define and assemble the digest; assert the
  section renders `discoveryFingerprint` and `retryOutcome` from the
  emitted event. Emit an event with mismatched field names and assert the
  test fails rather than the section silently rendering empty.
- HTTP contract registration: assert the `/api/v1/digest` route is
  present in `internal/api/testdata/contracts/daemon-http.compat.json`,
  has deterministic response and error captures, and that
  `go generate ./internal/api/...` regenerates them from the capture IDs
  registered in `contract_artifact_regen_test.go`.
- Config validation: malformed `FireTime`, unknown `Timezone`, unknown
  `EmptyDayMode`, and `VerdictFlipWindowMinutes <= 0` all fail fast at
  boot.

Contract coverage for the lifecycle: the digest goroutine starts with
`Runtime.Start` and stops cleanly on the stop channel (no leak across
restarts), following the existing scheduler-loop lifecycle pattern.

Repository validation is `gofmt -l .`, `go vet ./...`, `go test ./...`,
and `go build ./...`. Dashboard changes additionally run `pnpm install`,
`pnpm test`, and `pnpm build` per CI.
