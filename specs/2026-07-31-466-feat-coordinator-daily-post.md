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

A scheduled coordinator job (daily, configurable, disabled by default)
that assembles a post-merge digest from durable storage only and delivers
it through the existing notify gateway plus a dashboard section rendering
the same data. No new GitHub reads at digest time.

## Scope

In scope:

- A periodic digest job owned by the coordinator, started and stopped with
  the daemon lifecycle, config-gated and disabled by default.
- Assembly of four sections from existing durable records:
  - **Merged** — PR title, originating issue, reviewer verdict summary,
    gatekeeper reasons at merge time, diff size.
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
- New persisted tables, migrations, ledgers, or status columns.
- Cron expressions; a fixed daily schedule plus timezone is sufficient.
- Per-PR re-reads from GitHub at digest time.

## Sources and assembly

The digest is a **derived projection** over existing durable records. It
introduces no new persisted state. The window is a rolling 24 hours ending
at the moment the job fires, so a missed run simply covers the last 24h
whenever it next fires — no "last digest run" timestamp to keep in sync.

Assembly reads:

- `EventsRepository.ListSince(ctx, sinceISO)` for the time window
  (`internal/storage/repositories.go`), filtered to the event types below.
- `PullRequestSnapshotsRepository.GetLatestByProject` for current PR title,
  review state, and diff metadata when an event payload does not carry them.
- Gate reports: `pull_request.merge_gate.evaluated`
  (`gatekeeper.GateReportEventType`, payload `gatekeeper.Report`) and
  `pull_request.merge_gate.merge_attempted`
  (`gatekeeper.MergeOutcomeEventType`, payload `gatekeeper.MergeOutcome`).
- Reviewer verdicts: `pr.review.posted` events and the snapshot's
  `ReviewState`.

### Merged

For each `pull_request.merge_gate.merge_attempted` event with
`MergeOutcome.Merged == true` in the window:

- PR title and repo from the latest snapshot (`GetLatestByProject`).
- Originating issue: the loop's source issue, resolved via the
  `LoopID` on the merge event → `LoopRecord` source reference. If
  unrecoverable, omitted rather than guessed.
- Reviewer verdict summary: the most recent `pr.review.posted` before the
  merge attempt for the same PR, or the snapshot `ReviewState`.
- Gatekeeper reasons at merge time: `MergeOutcome.ConfirmingReasons` (the
  gates that passed the confirming pass; empty on a clean merge).
- Diff size: from the snapshot's `PayloadJSON` (additions/deletions already
  captured at snapshot time). No GitHub read.

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
human-actionable reason code — `ReasonHold`, `ReasonReviewRequired`,
`ReasonReviewChangesRequested`, `ReasonUnresolvedReviewThread` — together
with age derived from the report's `EvaluatedAt`. This mirrors the
gatekeeper reason vocabulary already in `internal/gatekeeper/runner.go`; no
new reason codes are introduced. The triager's
`AwaitingConfirmationStatus` (`internal/triager/awaiting.go`) covers
*issue* triage awaiting confirmation and is a separate surface; this
section is PR-scoped and reads gate reports, not triage reports.

### Anomalies

Two signals, both derived from existing records:

- Merges where the most recent `pr.review.posted` before merge carried
  non-blocking review comments (review event type comment-only, no
  approval/changes-requested flip).
- Merges where the reviewer verdict flipped (e.g. changes-requested →
  approved) within a short window before the merge attempt — detected by
  ordering `pr.review.posted` events for the PR up to the merge time.

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
string (YYYY-MM-DD in the configured tz), not persisted state. A restart
therefore at most re-runs today's digest, which is idempotent because the
window is rolling and delivery is deduped by the gateway's existing
`DedupeKey` (see Delivery).

## Configuration

A new sub-config on `CoordinatorRoleConfig`
(`internal/config/types.go`):

```go
type CoordinatorDigestConfig struct {
    Enabled       bool   `json:"enabled"`
    FireTime      string `json:"fireTime"`      // "HH:MM" in configured tz, default "09:00"
    Timezone      string `json:"timezone"`      // IANA name, default "UTC"
    EmptyDayMode  string `json:"emptyDayMode"`  // "quiet" | "notice", default "quiet"
    LookbackHours int    `json:"lookbackHours"` // default 24
}
```

Added to `CoordinatorRoleConfig` as `Digest CoordinatorDigestConfig`.
Default in `internal/config/defaults.go`: `Enabled: false`, `FireTime:
"09:00"`, `Timezone: "UTC"`, `EmptyDayMode: "quiet"`, `LookbackHours: 24`.
Config validation fails fast at daemon boot on a malformed `FireTime` or
unknown `Timezone`/`EmptyDayMode`, consistent with looperd's existing
fail-fast-on-config-validation behavior. TOML example:

```toml
[coordinator.digest]
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
existing throttle/dedupe path. `Level` is a non-sound level so osascript
does not fire a sound for a routine digest; the in-app and webhook/Feishu
channels still record/deliver. The gateway already persists an in-app
`NotificationRecord` regardless of push-channel success, so the digest
content survives a webhook/osascript failure.

The dashboard section is backed by a new HTTP route
`GET /api/v1/digest?date=YYYY-MM-DD` (default: today in configured tz) in
`internal/api/handler.go`, following the existing
`/api/v1/projects` list-route pattern. The handler re-assembles the same
four sections on demand from the same storage queries, so a failed push
delivery never loses the digest — the human can always pull it from the
dashboard. The dashboard page (`web/dashboard/src`) adds a `/digest` route
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

A rolling 24h window can split a merge that happened at 08:59 from the
review that happened at 09:01 the previous day across two digests, and a
daemon down for >24h can skip a day's window entirely. It does not catch
drift that compounds over weeks — that needs the human to read consecutive
digests, which is the intended audit cadence, not a per-digest guarantee.

Why the simpler alternative is insufficient: persisting a "last digest
delivered at" timestamp to bound the window would add a state field that
must stay in sync with the schedule and survive restarts, with a test that
fails when it goes stale. A rolling window needs none of that and is
correct for a daily audit: the worst case is a shifted boundary, not a
missed or duplicated digest. The gateway's `DedupeKey` already prevents
duplicate delivery within a day.

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
to name because the digest does not act.

## Risks

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
- **Window boundary shifts.** A late fire shifts the 24h boundary. Accepted
  for an audit surface; documented above.
- **Dashboard re-assembly cost.** Re-running the assembly on every
  `/api/v1/digest` request scans `EventsRepository.ListSince` for the day.
  Bounded by one day of events; acceptable for an on-demand (non-polled)
  endpoint. No caching layer is added.

## Validation

Regression coverage (Go tests, fixture-based, no network):

- Assembly from fixtures: seed `EventLogRecord`s and
  `PullRequestSnapshotRecord`s for a day with merges, holds, a verdict
  flip, and a closed-and-regenerated retry; assert the four sections match
  expected content (titles, reason codes, ages, anomaly flags).
- Empty-day behavior: no qualifying events in the window → `quiet` mode
  produces no `Notify` call; `notice` mode produces one payload with the
  "nothing merged" body.
- Delivery failure does not lose the digest: inject a webhook `HTTPPost`
  that returns an error; assert an in-app `NotificationRecord` is still
  persisted and the `/api/v1/digest` handler still returns the assembled
  digest.
- Dedupe: fire the job twice for the same date; assert the second `Notify`
  is deduped by `DedupeKey` and no duplicate push occurs.
- Timezone boundary: configured tz shifts the fire time and window
  boundary; assert events near midnight are attributed to the correct
  digest date.
- Config validation: malformed `FireTime`, unknown `Timezone`, and unknown
  `EmptyDayMode` fail fast at boot.

Contract coverage for the lifecycle: the digest goroutine starts with
`Runtime.Start` and stops cleanly on the stop channel (no leak across
restarts), following the existing scheduler-loop lifecycle pattern.

Repository validation is `gofmt -l .`, `go vet ./...`, `go test ./...`,
and `go build ./...`. Dashboard changes additionally run `pnpm install`,
`pnpm test`, and `pnpm build` per CI.
