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
  - **Awaiting human** — PRs holding `needs-human-review` and why (reason
    codes), with age.
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
- New persisted tables, migrations, ledgers, or status columns. The only
  storage change is a new bounded range query on the existing `event_logs`
  table and a small payload extension to the existing `pr.review.posted`
  event (see Sources and assembly).
- Cron expressions; a fixed daily schedule plus timezone is sufficient.
- Per-PR re-reads from GitHub at digest time.
- Merges that no durable record observes. Only the gatekeeper
  `confirmAndMerge` path emits a durable merge event today; reviewer
  auto-merge and manual maintainer merges are not covered (see Merged).

## Digest window

The digest is a **derived projection** over existing durable records. It
introduces no new persisted state. The window is a **fixed boundary derived
from the digest date**, not the instant the job fires: for digest date `D`
(a calendar day in the configured timezone), the window is
`[D 00:00 in tz, D 00:00 in tz + LookbackHours)`. With the default
`LookbackHours = 24` this is exactly the calendar day `D`; no "last digest
run" timestamp is kept in sync, and a missed run simply re-runs the date
that was missed.

A fixed date-derived boundary is required so the date-only dashboard route
can reconstruct the exact window the job pushed: the window is a pure
function of the digest date, the configured timezone, and `LookbackHours`,
all of which the handler reads from daemon config. Ending the window at the
actual fire instant would make a late catch-up unreproducible from the
date-only route and would overlap consecutive daily output under different
dedupe keys; the fixed boundary avoids both.

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
  both the job and the dashboard route.
- `PullRequestSnapshotsRepository.GetLatestByProject` for current PR title,
  review state, and diff metadata when an event payload does not carry them.
- Gate reports: `pull_request.merge_gate.evaluated`
  (`gatekeeper.GateReportEventType`, payload `gatekeeper.Report`) and
  `pull_request.merge_gate.merge_attempted`
  (`gatekeeper.MergeOutcomeEventType`, payload `gatekeeper.MergeOutcome`).
- Reviewer verdicts: `pr.review.posted` events (payload extended with the
  structured reviewer `outcome`, see below) and the snapshot's
  `ReviewState`.

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
- Reviewer verdict summary: the most recent `pr.review.posted` before the
  merge attempt for the same PR, read from its persisted `outcome`
  (`clean` / `non_blocking` / `blocking`), falling back to the snapshot
  `ReviewState` when no `pr.review.posted` event exists.
- Gatekeeper gate summary at merge time: the most recent
  `GateReportEventType` event for the PR at or before the merge attempt
  (the confirming `GateReport` evidence). `MergeOutcome.ConfirmingReasons`
  is **not** used: it is populated only when the confirming evaluation
  becomes blocked, in which case `Merged` remains `false`, so every event
  selected here with `Merged == true` has an empty `ConfirmingReasons`.
  A successful merge therefore has no blocking-reason summary by
  definition; the digest reports the confirming `GateReport`'s `Status`
  and any non-blocking `Reasons` it recorded.
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
failure fingerprint and retry outcome. If #462/#464 have not yet persisted
a distinguishable event type, this section renders empty with a note, and
fills in once those events exist — the digest reads events, it does not
own the regeneration concept. The fingerprint is the worker discovery
fingerprint already stored on `LoopRecord.MetadataJSON`
(`autonomousRecovery.lastFailedDiscoveryFingerprint`); retry success is the
presence of a later successful merge or a completed loop for the same
source.

### Awaiting human

PRs whose latest gate report in the window is blocked with a
human-actionable reason code — `ReasonHold`,
`ReasonReviewChangesRequested`, `ReasonUnresolvedReviewThread` — together
with age. `ReasonReviewRequired` is included **only** when no active
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

**Exclude reviews automation is still handling.** `ReasonReviewRequired`
only means GitHub currently lacks a required approval; it does not mean a
human must act. While Looper's reviewer loop is queued or running,
gatekeeper emits this reason too, so listing it under "Awaiting human"
would show ordinary in-progress automated reviews as human-actionable.
Before classifying `ReasonReviewRequired` as human-actionable, the
assembly correlates the gate report with active reviewer/fixer work via
`LoopsRepository.ListByRepoAndPR(repo, prNumber)`: if any returned loop
has an active reviewer or fixer status (queued or running), the PR is
omitted from this section. `ReasonHold`, `ReasonReviewChangesRequested`,
and `ReasonUnresolvedReviewThread` are human-actionable regardless of
active automation.

### Anomalies

Two signals, both derived from existing records and classified by the
persisted reviewer `outcome`, not the GitHub review event type:

- Merges where the most recent `pr.review.posted` before merge carried
  `outcome=non_blocking` (a non-blocking review with actionable feedback
  that did not block merge). A `COMMENT` event alone is not an anomaly:
  under the default policy a clean review may also publish as `COMMENT`,
  so only the structured `outcome` distinguishes them.
- Merges where the reviewer verdict flipped (e.g. `blocking` → `clean`,
  or `non_blocking` → `clean`) within a short window before the merge
  attempt — detected by ordering `pr.review.posted` events for the PR up
  to the merge time and comparing consecutive `outcome` values.

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
(`time.Location` from the configured tz string; default UTC). On startup,
if the configured fire time has already passed today and the job has not
yet fired today, it fires once — tracked with an in-memory `lastFireDate`
string (YYYY-MM-DD in the configured tz), not persisted state. The
in-memory guard only prevents a same-process double-fire; it does not
survive a restart.

### Same-day delivery idempotence across restarts

`lastFireDate` is empty after a restart, so the catch-up path can re-fire
the same day's digest. The gateway's existing dedupe is **not** sufficient
to suppress that duplicate: `DedupeKey` delivery is deduped only while
`ThrottleWindowSeconds` has not elapsed (60 seconds by default for
osascript, webhook, and Feishu in `internal/infra/notify/gateway.go`), so
a restart more than a minute after the scheduled run would send a
duplicate push despite the claimed same-day idempotence.

The job therefore performs a **day-wide durable dedupe check** before
pushing: for each configured channel it queries
`NotificationsRepository.GetLatestByDedupe(ctx, channel,
"digest:<YYYY-MM-DD>")` (the notification log the gateway already
persists) and skips the push when a delivered record exists for that date,
regardless of age. This bounds restart re-fires using already-persisted
state — no new "last digest delivered at" timestamp is introduced. The
`DedupeKey` remains `digest:<YYYY-MM-DD>` so the gateway's short throttle
still suppresses a same-process double-fire within the throttle window;
the day-wide notification-log check is the durable backstop that covers
restarts beyond that window.

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
    Enabled       bool   `json:"enabled"`
    FireTime      string `json:"fireTime"`      // "HH:MM" in configured tz, default "09:00"
    Timezone      string `json:"timezone"`      // IANA name, default "UTC"
    EmptyDayMode  string `json:"emptyDayMode"`  // "quiet" | "notice", default "quiet"
    LookbackHours int    `json:"lookbackHours"` // default 24
}
```

Added to `Config` as `Digest DigestConfig` and to `PartialConfig` as
`Digest *PartialDigestConfig`. Default in `internal/config/defaults.go`:
`Enabled: false`, `FireTime: "09:00"`, `Timezone: "UTC"`, `EmptyDayMode:
"quiet"`, `LookbackHours: 24`. Config validation fails fast at daemon boot
on a malformed `FireTime`, an unknown `Timezone`/`EmptyDayMode`, and a
non-positive or unbounded `LookbackHours` (required: `LookbackHours > 0`
and `LookbackHours <= 168`; zero produces an empty instant-sized window
and a negative value moves `since` into the future, both silently yielding
a misleading empty digest), consistent with looperd's existing
fail-fast-on-config-validation behavior. TOML example:

```toml
[digest]
enabled = false
fireTime = "09:00"
timezone = "UTC"
emptyDayMode = "quiet"
lookbackHours = 24
```

## Delivery

One `Gateway.Notify(ctx, payload)` call per digest
(`internal/infra/notify/gateway.go:191`), with a `SystemNotificationPayload`
whose `Body` carries the rendered digest and whose `DedupeKey` is
`digest:<YYYY-MM-DD>` so a same-day re-fire is deduped by the gateway's
existing throttle/dedupe path within the throttle window, with the
day-wide notification-log check in Scheduling as the durable backstop.
`Level` is a non-sound level so osascript does not fire a sound for a
routine digest; the in-app and webhook/Feishu channels still
record/deliver. The gateway already persists an in-app
`NotificationRecord` regardless of push-channel success, so the digest
content survives a webhook/osascript failure.

The dashboard section is backed by a new HTTP route
`GET /api/v1/digest?date=YYYY-MM-DD` (default: today in configured tz) in
`internal/api/handler.go`, following the existing
`/api/v1/projects` list-route pattern. The handler re-assembles the same
four sections on demand from the same `ListBetween` storage query over the
**same fixed date-derived window** (`[D 00:00 in tz, D 00:00 in tz +
LookbackHours)`), so a failed push delivery never loses the digest — the
human can always pull it from the dashboard, and the dashboard body is
byte-identical to what the job pushed because both derive the window from
the date. The dashboard page (`web/dashboard/src`) adds a `/digest` route
and a `DigestPage` component following the existing page pattern
(`useDashboardData`, table render). The dashboard fetches the digest
on-demand (not polled) since it is a daily artifact.

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
a missed or duplicated digest. Same-day restart idempotence is bounded by
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
to name because the digest does not act. The one storage change — a bounded
range query on `event_logs` and a payload extension on `pr.review.posted` —
reads and records existing evidence; it does not create a new authority
surface.

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
  date's fixed window. Bounded on both sides by the date-derived window;
  acceptable for an on-demand (non-polled) endpoint. No caching layer is
  added.

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
  `ConfirmingReasons` empty) with a preceding `GateReportEventType`; assert
  the digest reads the `GateReport` evidence, not the empty
  `ConfirmingReasons`.
- Diff size: seed a snapshot with a complete retained diff; assert
  additions/deletions are counted from the diff. Seed a snapshot with
  `diffTruncated: true`; assert diff size renders "unavailable (truncated)".
- Awaiting-human age: seed consecutive blocked gate reports 30 minutes
  apart spanning several hours; assert the age is measured from the first
  report in the continuous blocked interval, not the latest.
- Awaiting-human exclusion: seed a `ReasonReviewRequired` gate report with
  an active reviewer loop for the PR; assert the PR is omitted. Seed the
  same with no active reviewer/fixer loop; assert it is included.
- Empty-day behavior: no qualifying events in the window → `quiet` mode
  produces no `Notify` call; `notice` mode produces one payload with the
  "nothing merged" body.
- Delivery failure does not lose the digest: inject a webhook `HTTPPost`
  that returns an error; assert an in-app `NotificationRecord` is still
  persisted and the `/api/v1/digest` handler still returns the assembled
  digest.
- Same-day restart idempotence: persist a delivered notification record
  with `DedupeKey digest:<date>` older than `ThrottleWindowSeconds`; fire
  the job; assert the push is skipped by the day-wide notification-log
  check and no duplicate `Notify` occurs.
- Window reproducibility: request `/api/v1/digest?date=<D>` and assert the
  handler uses the same fixed `[D 00:00, D 00:00 + LookbackHours)` window
  the job would use, with an exclusive end bound (an event at exactly
  `D+1 00:00` is excluded).
- Config validation: malformed `FireTime`, unknown `Timezone`, unknown
  `EmptyDayMode`, `LookbackHours = 0`, `LookbackHours = -1`, and
  `LookbackHours > 168` all fail fast at boot.

Contract coverage for the lifecycle: the digest goroutine starts with
`Runtime.Start` and stops cleanly on the stop channel (no leak across
restarts), following the existing scheduler-loop lifecycle pattern.

Repository validation is `gofmt -l .`, `go vet ./...`, `go test ./...`,
and `go build ./...`. Dashboard changes additionally run `pnpm install`,
`pnpm test`, and `pnpm build` per CI.
