package webui

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

// Loader reads one render's worth of durable state.
type Loader func(context.Context) Input

// LoadCache holds one render's input for a short window so that N open tabs
// polling the same page cost one read of durable state rather than N. The page
// re-polls every RefreshInterval, so a hit is at most one refresh behind the
// board a single tab would have shown anyway.
//
// It is deliberately not single-flight: two concurrent misses both load, and
// the later load generation wins publication. Loading twice is the cost this
// exists to bound, not to eliminate, and a coalescing barrier would make a
// slow collect block every other request behind it.
type LoadCache struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	input    Input
	loadedAt time.Time
	loaded   bool
	// generation is incremented when a miss starts. A slower older miss must
	// not overwrite the result of a newer miss merely because it finished last.
	generation uint64
}

// NewLoadCache returns a cache that serves a stored input for ttl. A ttl of
// zero or less disables caching, which is what a caller that wants every
// request to read live state passes.
func NewLoadCache(ttl time.Duration, now func() time.Time) *LoadCache {
	if now == nil {
		now = time.Now
	}
	return &LoadCache{ttl: ttl, now: now}
}

// Wrap returns a Loader that answers from the cache while the stored input is
// younger than the TTL and otherwise delegates to inner. The caller rebuilds
// inner per request — the cache is what survives across them, so it must be
// held by something longer-lived than one handler.
func (c *LoadCache) Wrap(inner Loader) Loader {
	if inner == nil {
		return nil
	}
	if c == nil || c.ttl <= 0 {
		return inner
	}
	return func(ctx context.Context) Input {
		now := c.now().UTC()
		c.mu.Lock()
		if c.loaded && now.Sub(c.loadedAt) < c.ttl {
			cached := c.input
			c.mu.Unlock()
			// The state is the state that was read; advance relative to the
			// collector's durable generation timestamp so a slow Collect call
			// cannot make the digest look younger than the data it contains.
			elapsed := now.Sub(c.loadedAt)
			if generatedAt := parseTimestamp(cached.Escalator.GeneratedAt); !generatedAt.IsZero() {
				elapsed = now.Sub(generatedAt)
			}
			cached = advanceCachedAges(cached, elapsed)
			cached.Now = now
			return cached
		}
		c.generation++
		generation := c.generation
		c.mu.Unlock()

		input := inner(ctx)
		// A request-owned cancellation must not poison the shared cache with the
		// partial/noticed input produced by that canceled read. The next healthy
		// request should retry the durable sources instead of serving the failed
		// projection for the whole TTL.
		if ctx != nil && ctx.Err() != nil {
			return input
		}
		publishedAt := c.now().UTC()
		c.mu.Lock()
		if generation != c.generation {
			c.mu.Unlock()
			return input
		}
		c.input, c.loadedAt, c.loaded = input, publishedAt, true
		c.mu.Unlock()
		return input
	}
}

func advanceCachedAges(input Input, elapsed time.Duration) Input {
	seconds := int64(elapsed / time.Second)
	if seconds <= 0 {
		return input
	}
	if len(input.Escalator.Items) > 0 {
		items := append([]escalator.Item(nil), input.Escalator.Items...)
		for index := range items {
			items[index].AgeSeconds += seconds
		}
		input.Escalator.Items = items
	}
	if len(input.Escalator.Backlog) > 0 {
		backlog := append([]escalator.StageBacklog(nil), input.Escalator.Backlog...)
		for index := range backlog {
			backlog[index].OldestAgeSeconds += seconds
		}
		input.Escalator.Backlog = backlog
	}
	return input
}

// NewRepositoryLoader reads triage input straight from durable state. Every
// source degrades on its own: a source that fails narrows the board and leaves
// a notice on the page rather than failing the request, because an operator
// looking at a half-broken daemon still needs the half that works.
func NewRepositoryLoader(repos *storage.Repositories, collector *escalator.Collector, links escalator.Linker, now func() time.Time) Loader {
	if now == nil {
		now = time.Now
	}
	var escalatorStateMu sync.Mutex
	var lastCompleteEscalator escalator.Snapshot
	haveCompleteEscalator := false
	return func(ctx context.Context) Input {
		input := Input{Now: now().UTC(), Links: links}
		if repos == nil {
			input.Notices = append(input.Notices, "Storage is not available.")
			return input
		}
		if repos.Projects != nil {
			projects, err := repos.Projects.List(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Projects could not be read.")
			} else {
				input.ActiveProjects = make(map[string]bool, len(projects))
				input.ActiveProjectRepositories = make(map[string]string, len(projects))
				for _, project := range projects {
					input.ActiveProjects[project.ID] = !project.Archived
					if repo := projectRepository(project.MetadataJSON); repo != "" {
						input.ActiveProjectRepositories[project.ID] = repo
					}
				}
			}
		}

		if repos.PullRequestSnapshots != nil {
			snapshots, err := repos.PullRequestSnapshots.ListLatest(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Pull request snapshots could not be read.")
			} else {
				input.Snapshots = snapshots
			}
		}
		if repos.Events != nil {
			reports, err := loadGateReports(ctx, repos.Events)
			if err != nil {
				input.Notices = append(input.Notices, "Merge gate reports could not be read.")
			} else {
				input.Reports = reports
			}
		}
		if repos.Loops != nil {
			loops, err := repos.Loops.List(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Loops could not be read.")
			} else {
				input.Loops = loops
			}
		}
		if repos.Queue != nil {
			queue, err := repos.Queue.ListForTriage(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Queue items could not be read.")
			} else {
				input.Queue = queue
			}
		}
		if collector != nil {
			snapshot, err := collector.Collect(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Escalator digest could not be collected.")
			} else {
				escalatorStateMu.Lock()
				if snapshot.Partial {
					if haveCompleteEscalator {
						// A rotating census is not resolution evidence. Keep the last
						// complete projection and age it from its durable generation
						// timestamp while the collector finishes its cycle.
						elapsed := input.Now.Sub(parseTimestamp(lastCompleteEscalator.GeneratedAt))
						snapshot = advanceCachedAges(Input{Escalator: lastCompleteEscalator}, elapsed).Escalator
						snapshot.Partial = false
						input.Notices = append(input.Notices, "Escalator census is incomplete; showing the last complete projection.")
					} else {
						input.Notices = append(input.Notices, "Escalator census is incomplete; board counts may be provisional.")
					}
				} else {
					lastCompleteEscalator = cloneEscalatorSnapshot(snapshot)
					haveCompleteEscalator = true
				}
				escalatorStateMu.Unlock()
				input.Escalator = snapshot
			}
		}
		return input
	}
}

func cloneEscalatorSnapshot(snapshot escalator.Snapshot) escalator.Snapshot {
	snapshot.Items = append([]escalator.Item(nil), snapshot.Items...)
	snapshot.Backlog = append([]escalator.StageBacklog(nil), snapshot.Backlog...)
	return snapshot
}

// projectRepository is the durable Project binding, not a value copied from a
// stale snapshot. A missing or malformed metadata payload leaves the repo
// unknown so the read surface remains backwards-compatible with legacy rows.
func projectRepository(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var metadata struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(metadata.Repo))
}

// loadGateReports projects the durable merge-gate events. Gatekeeper appends one
// report per evaluation, so the newest per pull request is the current verdict:
// the query returns only that row per entity, because every earlier evaluation
// would be discarded here and the log only grows. Classify still reduces what
// arrives — one pull request can hold more than one entity id across renames —
// because it also has to reconcile the verdict with the snapshot.
func loadGateReports(ctx context.Context, events *storage.EventsRepository) ([]gatekeeper.Report, error) {
	records, err := events.ListLatestByEntityTypeAndEventTypes(ctx, "", "pull_request", []string{gatekeeper.GateReportEventType})
	if err != nil {
		return nil, err
	}
	reports := make([]gatekeeper.Report, 0, len(records))
	for _, record := range records {
		var report gatekeeper.Report
		if json.Unmarshal([]byte(record.PayloadJSON), &report) != nil {
			// The event row is still durable evidence that Gatekeeper evaluated
			// this PR. Preserve its identity as a blocked provider-state report;
			// silently dropping it would make an unreadable verdict look ready.
			if blocked, ok := unreadableGateReport(record); ok {
				reports = append(reports, blocked)
			}
			continue
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func unreadableGateReport(record storage.EventLogRecord) (gatekeeper.Report, bool) {
	if record.ProjectID == nil || record.EntityID == nil {
		return gatekeeper.Report{}, false
	}
	entity := strings.TrimSpace(*record.EntityID)
	separator := strings.LastIndexByte(entity, '#')
	if separator <= 0 || separator == len(entity)-1 {
		return gatekeeper.Report{}, false
	}
	number, err := strconv.ParseInt(entity[separator+1:], 10, 64)
	if err != nil || number <= 0 {
		return gatekeeper.Report{}, false
	}
	repo := strings.TrimSpace(entity[:separator])
	if repo == "" {
		return gatekeeper.Report{}, false
	}
	return gatekeeper.Report{
		Version:     2,
		Status:      gatekeeper.StatusBlocked,
		ProjectID:   strings.TrimSpace(*record.ProjectID),
		Repo:        repo,
		PRNumber:    number,
		Reasons:     []gatekeeper.Reason{{Code: gatekeeper.ReasonProviderStateUnavailable, Subject: "gate_report"}},
		EvaluatedAt: strings.TrimSpace(record.CreatedAt),
	}, true
}
