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
- New persisted tables, ledgers, or status columns. The only storage
  changes are a new bounded range query on the existing `event_logs`
  table, a new `created_at`-leading index on `event_logs` to support that
  query (see Digest window), and a small payload extension to the existing
  `pr.review.posted` event (see Sources and assembly).
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

The digest date `D` identifies the window `[D 00:00 in tz, end(D))`. When
the job fires at `FireTime` on calendar day `F`, it selects `D` = the most
recent date whose window has fully closed before the fire instant
(`end(D) <= fireInstant`). With the default `LookbackHours = 24` and
`FireTime = "09:00"`, firing on day `F` selects `D = F − 1` and summarizes
the completed previous calendar day; the 09:00 run therefore reports all
of yesterday's events, not the 00:00–09:00 slice of today. Tomorrow's run
selects `D = F` and covers the day that has just closed. No "last digest
run" timestamp is kept in sync, and a missed run simply re-runs the date
that was missed, since `D` is a pure function of the fire date and config.

The window end `end(D)` is constructed by **calendar arithmetic in the
configured location**, not by adding a fixed duration to `D 00:00`:
adding `24h` to local midnight is not exactly the next calendar day on a
daylight-saving transition — a 23-hour spring-forward day extends the
window into the next date, and a 25-hour fall-back day omits the final
hour, causing overlap or missing merges. For the calendar-day default
`LookbackHours = 24`, `end(D) = (D+1) 00:00 in tz` built via
`time.Date(D.year, D.month, D.day+1, 0, 0, 0, 0, loc)`, which lets the
`time.Location` resolve the DST transition. For a non-24 `LookbackHours`
the window is an **elapsed-duration** window, `end(D) = D 00:00 in tz +
LookbackHours·h`, which is intentionally not a calendar day and is
documented as such in Configuration; the selected `D` is still the most
recent date whose `end(D)` has closed before the fire instant, so a
`LookbackHours > 24` paired with an early `FireTime` may require waiting
until a later fire date whose window has closed (the default 09:00
satisfies `LookbackHours <= 24`).

A fixed date-derived boundary is required so the date-only dashboard route
can reconstruct the exact window the job pushed: the window is a pure
function of the digest date, the configured timezone, and `LookbackHours`,
all of which the handler reads from daemon config. Ending the window at the
actual fire instant would make a late catch-up unreproducible from the
date-only route and would overlap consecutive daily output under different
dedupe keys; the fixed, completed boundary avoids both.

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
  created_at)`; neither has a `created_at` leading column, so a
  `created_at`-only range predicate would scan and sort the full history
  for every job tick and dashboard request. A new migration therefore adds
  `CREATE INDEX idx_event_logs_created_at_id ON event_logs (created_at, id)`
  so the bounded range uses the leading column and the `id` tiebreaker
  satisfies the `ORDER BY created_at ASC, id ASC` without a separate sort.
  This index is the second storage change noted in Scope.
- `PullRequestSnapshotsRepository.GetLatestByProject` for current PR title,
  review state, and diff metadata when an event payload does not carry them.
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

- PR title and repo from the latest snapshot (`GetLatestByProject`).
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
- Gatekeeper gate summary at merge time: the most recent
  `GateReportEventType` event for the PR at or before the merge attempt
  (the confirming `GateReport` evidence). `MergeOutcome.ConfirmingReasons`
  is **not** used: it is populated only when the confirming evaluation
  becomes blocked, in which case `Merged` remains `false`, so every event
  selected here with `Merged == true` has an empty `ConfirmingReasons`.
  A successful merge therefore has no blocking-reason summary by
  definition. The digest reports the confirming `GateReport`'s `Status`
  and, for the actual gate detail, its `Evidence`
  (`gatekeeper.Report.Evidence`): the required checks and their per-check
  status/conclusion (`Evidence.Checks`, `Evidence.RequiredChecks`), the
  review decision (`Evidence.ReviewDecision`), the required approving
  review count, and the mergeability state. `Reasons` is empty for every
  successful merge (`Eligible` is true only when `Reasons` is empty), so
  `Status` + `Reasons` alone would show the same eligible summary for every
  merged PR; `Evidence` is what distinguishes them.
- Diff size: derived from the raw `diff` retained in the snapshot's
  `PayloadJSON` (`payloadMap["diff"]`) by counting `+`/`-` content lines
  (excluding `+++`/`---` file headers). `CapturePullRequestSnapshot`
  stores a `detail` object with no `additions`/`deletions` fields plus the
  raw `diff`, so the numeric counts are derived from the retained diff
  text, not from a field that does not exist. When the snapshot recorded
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
threads the `outcome` it already receives at its call sites into the
`appendEvent` payload map. The digest classifies reviews by this
`outcome`, never by the GitHub `event` type alone.

### Closed-and-regenerated (#462/#464)

This section depends on the close-and-regenerate tracking that #462/#464
introduce. Until those land, the digest reads whatever lifecycle events
they emit. The assembly contract is: list PRs in the window that were
closed by the regenerate flow and the retry loop that followed, with the
failure fingerprint and retry outcome. The fingerprint and retry outcome
must come from the **lifecycle event that #462/#464 emit at the
transition**, carried in that event's own payload — not from a join against
the mutable `LoopRecord`. `autonomousRecovery.lastFailedDiscoveryFingerprint`
on `LoopRecord.MetadataJSON` is a mutable "last" value: a later failure can
overwrite it, recovery can clear it, and the loop's completion status can
change after another retry, so a dashboard request issued later could
attach a newer fingerprint or outcome to an older close event. The
close-and-regenerate lifecycle event therefore carries, in its own
payload, the fingerprint of the discovery that failed and whether the
retry that followed succeeded, both observed at transition time; the
digest reads those payload fields directly. If #462/#464 have not yet
persisted such an event with those fields, this section renders empty with
a note and fills in once they do — the digest reads events, it does not
own the regeneration concept or join it to mutable loop state.

### Awaiting human

PRs whose latest gate report in the window is blocked with a
human-actionable reason code — `ReasonHold`,
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
a non-human-actionable reason, ends the walk; the age is `now −
firstBlockedEvaluatedAt`.

The backward walk is **not** limited to the digest window's
`ListBetween` result. If a PR entered a human-blocked state before the
window's `since` and remained blocked, the windowed query would stop at
the first report after the boundary and under-report the age by days or
weeks. For each awaiting-human PR the assembly therefore reads the PR's
full lifecycle via `EventsRepository.ListByEntity(ctx, "pull_request",
entityID)` (ordered `created_at ASC`) and walks backward from the latest
in-window blocked report through consecutive human-actionable reports
until it finds the transition — a preceding report with a non-human-
actionable reason, a gap, or the entity's first report — even when that
preceding report lies before `since`. The window bounds only *which PRs
are listed* (latest in-window report is blocked), not *how far back the
age walk reaches`; the per-PR lifecycle is bounded by one PR's history.

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

- Merges where the most recent `pr.review.posted` before merge carried
  `outcome=non_blocking` (a non-blocking review with actionable feedback
  that did not block merge). A `COMMENT` event alone is not an anomaly:
  under the default policy a clean review may also publish as `COMMENT`,
  so only the structured `outcome` distinguishes them.
- Merges where the reviewer verdict flipped (e.g. `blocking` → `clean`,
  or `non_blocking` → `clean`) within a fixed window before the merge
  attempt. The window is `VerdictFlipWindowMinutes` (default 120, a field
  on `DigestConfig`, see Configuration) anchored to the merge event's
  `AttemptedAt`: only `pr.review.posted` events for the PR whose `headSha`
  equals the merged head and whose timestamp falls in
  `[AttemptedAt − VerdictFlipWindowMinutes, AttemptedAt]` are considered,
  ordered by time; a flip is two consecutive such events with different
  `outcome` values. A fixed, configured threshold anchored to the merge
  attempt makes the classification stable across implementations and the
  fixture assertable, rather than leaving "short window" to mean anything
  from adjacent minutes to the whole digest window.

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
`[D 00:00 in tz, end(D))` has closed before the fire instant (under
defaults, `D` = the previous calendar day). On startup, if the selected
`D` has not yet been pushed the job fires once for it — tracked with an
in-memory `lastDigestDate` string (the digest date `D`, YYYY-MM-DD in the
configured tz), not persisted state. The in-memory guard only prevents a
same-process double-fire for the same `D`; it does not survive a restart.

### Same-date delivery idempotence across restarts

`lastDigestDate` is empty after a restart, so the catch-up path can re-fire
the same digest date `D`. The gateway's existing dedupe is **not** sufficient
to suppress that duplicate: `DedupeKey` delivery is deduped only while
`ThrottleWindowSeconds` has not elapsed (60 seconds by default for
osascript, webhook, and Feishu in `internal/infra/notify/gateway.go`), so
a restart more than a minute after the scheduled run would send a
duplicate push despite the claimed same-date idempotence.

The job therefore performs a **date-wide durable dedupe check** before
pushing: for each configured channel it queries
`NotificationsRepository.GetLatestByDedupe(ctx, channel,
"digest:<D>")` where `<D>` is the digest date (the notification log the
gateway already persists) and skips the push when a delivered record exists
for that date, regardless of age. This bounds restart re-fires using
already-persisted state — no new "last digest delivered at" timestamp is
introduced. The `DedupeKey` remains `digest:<D>` so the gateway's short
throttle still suppresses a same-process double-fire within the throttle
window; the date-wide notification-log check is the durable backstop that
covers restarts beyond that window.

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
    LookbackHours           int    `json:"lookbackHours"`           // default 24
    VerdictFlipWindowMinutes int   `json:"verdictFlipWindowMinutes"` // default 120
}
```

Added to `Config` as `Digest DigestConfig` and to `PartialConfig` as
`Digest *PartialDigestConfig`. Default in `internal/config/defaults.go`:
`Enabled: false`, `FireTime: "09:00"`, `Timezone: "UTC"`, `EmptyDayMode:
"quiet"`, `LookbackHours: 24`, `VerdictFlipWindowMinutes: 120`. Config
validation fails fast at daemon boot on a malformed `FireTime`, an unknown
`Timezone`/`EmptyDayMode`, a non-positive or unbounded `LookbackHours`
(required: `LookbackHours > 0` and `LookbackHours <= 168`; zero produces
an empty instant-sized window and a negative value moves `since` into the
future, both silently yielding a misleading empty digest), and a
non-positive `VerdictFlipWindowMinutes` (required: `> 0`; zero or
negative makes the verdict-flip anomaly ill-defined), consistent with
looperd's existing fail-fast-on-config-validation behavior. TOML example:

```toml
[digest]
enabled = false
fireTime = "09:00"
timezone = "UTC"
emptyDayMode = "quiet"
lookbackHours = 24
verdictFlipWindowMinutes = 120
```

## Delivery

One `Gateway.Notify(ctx, payload)` call per digest
(`internal/infra/notify/gateway.go:191`), with a `SystemNotificationPayload`
whose `Body` carries the rendered digest and whose `DedupeKey` is
`digest:<D>` (the digest date) so a same-date re-fire is deduped by the
gateway's existing throttle/dedupe path within the throttle window, with
the date-wide notification-log check in Scheduling as the durable
backstop. `Level` is set to a value the outbound channel allow-lists
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
`/api/v1/projects` list-route pattern. The handler re-assembles the same
four sections on demand from the same `ListBetween` storage query over the
**same fixed date-derived window** (`[D 00:00 in tz, end(D))`), so a failed
push delivery never loses the digest — the human can always pull it from
the dashboard. The dashboard is a **current projection**, not a
byte-identical replay of the pushed body: values that depend on `now`
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
a daemon down for more than `LookbackHours` can skip a day's window
entirely. It does not catch drift that compounds over weeks — that needs
the human to read consecutive digests, which is the intended audit cadence,
not a per-digest guarantee. It also does not catch merges that leave no
durable merge event (reviewer auto-merge, manual maintainer merges); see
Merge authority coverage.

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
range query and a `created_at`-leading index on `event_logs`, plus a
payload extension on `pr.review.posted` — read and record existing
evidence; they do not create a new authority surface.

## Risks

- **Merge authority coverage.** Only `gatekeeper.confirmAndMerge` emits a
  durable merge event. PRs merged via reviewer auto-merge
  (`EnableAutoMerge`) or by a maintainer manually are not in the Merged
  section. A deployment that does not use gatekeeper auto-merge will see a
  "nothing merged" digest despite actual merges. Covering those paths
  requires a durable merge observation shared by all merge paths, which is
  a separate, larger change and is intentionally out of scope for this
  derived projection.
- **#462/#464 dependency.** The closed-and-regenerated section reads events
  those issues produce. If they ship a different event shape than assumed,
  the section renders empty until the assembly filter is updated. The
  digest does not own that concept, so the risk is bounded to one section
  rendering empty, not a wrong value.
- **Snapshot recency.** PR title and diff size come from the latest
  snapshot, which may be stale if no discovery tick ran after the merge.
  Mitigated: merge events carry `HeadSHA`; the assembly prefers snapshot
  data captured at or after the merge and falls back to the latest
  available, never guessing.
- **Diff size truncation.** When the snapshot's diff was truncated
  (`diffTruncated: true`), diff size is rendered "unavailable (truncated)"
  rather than counted from an incomplete diff. This is an honest
  unavailable result, not a guess.
- **Window boundary shifts.** A late fire does not shift the window: the
  window is fixed to the digest date. A daemon down for more than
  `LookbackHours` skips that date's window entirely; accepted for an audit
  surface.
- **Dashboard re-assembly cost.** Re-running the assembly on every
  `/api/v1/digest` request scans `EventsRepository.ListBetween` for the
  date's fixed window. Bounded on both sides by the date-derived window and
  served by the new `created_at`-leading `event_logs` index, so the scan
  uses the leading column rather than the full history; acceptable for an
  on-demand (non-polled) endpoint. No caching layer is added.
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
  expected content (titles, reason codes, ages, anomaly flags). Include a
  `pr.review.posted` with `event=COMMENT` and `outcome=clean` to assert it
  is **not** flagged as a non-blocking anomaly, and one with
  `outcome=non_blocking` to assert it is.
- Originating issue resolution: seed a merge event with no `LoopID` and a
  matching `LoopRecord` reachable by `ListByRepoAndPR`; assert the
  originating issue is resolved. Seed a merge with no matching loop; assert
  the issue is omitted, not guessed.
- Gate summary: seed a successful merge (`Merged == true`,
  `ConfirmingReasons` empty) with a preceding `GateReportEventType` whose
  `Reasons` is empty and `Evidence` is populated (required checks, review
  decision, mergeability); assert the digest reads the `GateReport`
  `Evidence`, not the empty `ConfirmingReasons` or the bare eligible
  `Status`.
- Reviewer verdict head match: seed a merge for head B with a preceding
  `pr.review.posted` carrying `outcome=clean` for head A (older) and one
  for head B carrying `outcome=blocking`; assert the verdict summary uses
  B's `blocking`, not A's `clean`. Seed a merge with no `pr.review.posted`
  matching the merged head; assert the verdict renders "unavailable", not
  the snapshot `ReviewState`.
- Diff size: seed a snapshot with a complete retained diff; assert
  additions/deletions are counted from the diff. Seed a snapshot with
  `diffTruncated: true`; assert diff size renders "unavailable (truncated)".
- Awaiting-human age: seed consecutive blocked gate reports 30 minutes
  apart spanning several hours, with the first report **before** the
  window's `since`; assert the age is measured from that first report in
  the continuous blocked interval (read via `ListByEntity` beyond the
  window), not from the latest in-window report.
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
  with `DedupeKey digest:<D>` older than `ThrottleWindowSeconds`; fire
  the job; assert the push is skipped by the date-wide notification-log
  check and no duplicate `Notify` occurs.
- Completed-window selection: under defaults (`LookbackHours = 24`,
  `FireTime = "09:00"`), fire the job on day `F` and assert it selects
  `D = F − 1` and queries `[F−1 00:00, F 00:00)`; assert an event at
  `F 00:00` belongs to the next day's digest, not this one.
- Window reproducibility: request `/api/v1/digest?date=<D>` and assert the
  handler uses the same fixed `[D 00:00, end(D))` window the job would use,
  with an exclusive end bound (an event at exactly `end(D)` is excluded).
  For `LookbackHours = 24` assert `end(D)` is `(D+1) 00:00` constructed via
  the configured location (assert correct behavior on a DST transition
  date).
- Dashboard is a current projection: push a digest, then advance `now` by
  one minute and request `/api/v1/digest?date=<D>`; assert the
  Awaiting-human age differs from the pushed body (the dashboard is not
  byte-identical), while the merged/closed sections over the fixed window
  match.
- Config validation: malformed `FireTime`, unknown `Timezone`, unknown
  `EmptyDayMode`, `LookbackHours = 0`, `LookbackHours = -1`,
  `LookbackHours > 168`, and `VerdictFlipWindowMinutes <= 0` all fail fast
  at boot.

Contract coverage for the lifecycle: the digest goroutine starts with
`Runtime.Start` and stops cleanly on the stop channel (no leak across
restarts), following the existing scheduler-loop lifecycle pattern.

Repository validation is `gofmt -l .`, `go vet ./...`, `go test ./...`,
and `go build ./...`. Dashboard changes additionally run `pnpm install`,
`pnpm test`, and `pnpm build` per CI.
