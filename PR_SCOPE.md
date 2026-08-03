## Scope: thin discovery GraphQL queries (rate-limit resilience)

**Closes:** (rate-limit incident 2026-08-04)

### Problem

looperd's per-tick discovery burned the full GitHub GraphQL quota
(5000 points/hour) in ~20 minutes, taking down every `gh` call on the
same token (including unrelated Hermes sessions) via the shared quota +
REST secondary burst limit.

Root cause in code (not config):

1. **`DiscoverySnapshot.viewPullRequest` always fetches the heaviest
   view.** `discovery_snapshot.go:205` calls `ViewPullRequestForFixer`
   (includes `statusCheckRollup` + `reviews` + comments) for **every**
   cached PR detail, even when the caller only needs
   `IsDraft`/`Author`/`ReviewRequests`/`Labels` (e.g. `reviewerWorkPending`,
   `fixerWorkPending`, review-assignment routing in coordinator).
   `statusCheckRollup` is the single most expensive GraphQL field — one
   PR's check rollup costs ~10-50 points (each check run is a node).

2. **`ViewPullRequest` (lightweight metadata fields) is only used when
   no snapshot is in context.** With a snapshot present (the default
   during scheduler ticks), `Gateway.ViewPullRequest` routes to
   `snapshot.viewPullRequest` → heavy `ViewPullRequestForFixer`.
   So discovery's "cheap path" silently becomes the expensive path.

3. Every scheduler tick (30s before this incident; 600s after the
   config fix) does: open-PR list (already thin via
   `prDiscoveryListJSONFields`) + **one heavy view per candidate PR**,
   × 3 repos × 4 discovery lanes. The list was already thin; the
   per-PR views were the quota burner.

### Change

Split snapshot PR-detail caching into two tiers:

- **Metadata tier** (`ViewPullRequest` semantics): the fields
  `prViewMetadataJSONFields` already define (no `statusCheckRollup`,
  no `reviews`). Used by discovery/pending/assignment paths.
- **Full tier** (`ViewPullRequestForFixer`/`ForReviewer` semantics):
  only for callers that actually consume `detail.Checks`
  (fixer normalize/regeneration, coordinator `hasPendingWorkForPR`,
  gatekeeper evidence) or `detail.Reviews` (reviewer).

Concretely:

1. `DiscoverySnapshot` gains a metadata cache (`prMetadata` map) keyed
   separately from the existing full-detail cache.
2. `snapshot.viewPullRequest` (the metadata path) fetches via
   `prViewMetadataJSONFields`-equivalent fields, not
   `ViewPullRequestForFixer`.
3. Callers that need Checks/Reviews explicitly use
   `ViewPullRequestForFixer`/`ViewPullRequestForReviewer` (full tier,
   still snapshot-cached but only pulled once per tick per PR).
4. Gatekeeper's `ViewPullRequestForGatekeeper` stays fresh/bypass
   (it must bind reports to current head — unchanged).

### Expected effect

- Discovery per-tick GraphQL cost drops from ~10.4 points/PR (rollup
  included) to ~2 points/PR (metadata only).
- Full views (rollup) happen only for PRs that reach
  merge/processing paths, not for every open PR in every tick.
- With the 600s scheduler interval already applied, per-hour cost
  lands ~300-500 points (≈10% of quota), leaving headroom for Hermes
  sessions and other tools on the same token.

### Out of scope (separate PRs)

- Query batching (single POST, multiple named operations).
- Global token bucket / rate-limit circuit breaker in looperd.
- Incremental `updated:>last_poll` filtering.
- Retry backoff changes for 403/429.

### Verification

- Unit tests: metadata vs full tier return correct field subsets;
  snapshot caches per tier without cross-contamination.
- Existing gateway/discovery tests stay green.
- `go vet ./...` + `go test ./...` (coordinator, reviewer, fixer,
  infra/github packages).
