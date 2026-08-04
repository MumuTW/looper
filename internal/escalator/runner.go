package escalator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	notifyinfra "github.com/MumuTW/looper/internal/infra/notify"
	"github.com/MumuTW/looper/internal/storage"
)

const (
	DigestEventType  = "escalator.digest.generated"
	digestEntityType = "escalator_digest"
	digestEntityID   = "global"
)

type SnapshotCollector interface {
	Collect(context.Context) (Snapshot, error)
}

type Notifier interface {
	Notify(context.Context, notifyinfra.SystemNotificationPayload) []storage.NotificationRecord
}

type RunnerOptions struct {
	Now      func() time.Time
	MaxItems int
}

type Runner struct {
	collector    SnapshotCollector
	notifier     Notifier
	repositories *storage.Repositories
	now          func() time.Time
	maxItems     int
}

type RunResult struct {
	Snapshot   Snapshot `json:"snapshot"`
	Delta      Delta    `json:"delta"`
	Notified   bool     `json:"notified"`
	Suppressed bool     `json:"suppressed"`
}

func NewRunner(collector SnapshotCollector, notifier Notifier, repositories *storage.Repositories, options RunnerOptions) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	maxItems := options.MaxItems
	if maxItems <= 0 {
		maxItems = 500
	}
	return &Runner{collector: collector, notifier: notifier, repositories: repositories, now: now, maxItems: maxItems}
}

// Run performs one read-only census. The only writes are the existing
// notification delivery records and, after a successful delivery (or an empty
// digest), a bounded delta baseline event. A partial census never advances the
// baseline and therefore cannot manufacture resolutions.
func (r *Runner) Run(ctx context.Context) (RunResult, error) {
	if r.collector == nil {
		return RunResult{}, fmt.Errorf("escalator collector is not configured")
	}
	if r.repositories == nil || r.repositories.Events == nil {
		return RunResult{}, fmt.Errorf("escalator events repository is not configured")
	}
	snapshot, err := r.collector.Collect(ctx)
	if err != nil {
		return RunResult{}, fmt.Errorf("collect escalator digest: %w", err)
	}
	if len(snapshot.Items) > r.maxItems {
		return RunResult{}, fmt.Errorf("escalator digest has %d items, exceeds bounded baseline limit %d", len(snapshot.Items), r.maxItems)
	}
	if snapshot.Partial {
		// A rotating source window is a useful read projection, but omitted rows
		// are not resolution evidence. Wait for the collector to complete its
		// cycle before comparing or persisting the digest baseline. The result
		// retains Partial so the scheduler can retry without consuming cadence.
		return RunResult{Snapshot: snapshot, Suppressed: true}, nil
	}
	previous, err := r.loadPrevious(ctx)
	if err != nil {
		return RunResult{}, err
	}
	delta := Compare(previous, snapshot)
	result := RunResult{Snapshot: snapshot, Delta: delta}
	if previous != nil && delta.Empty() {
		result.Suppressed = true
		return result, nil
	}

	// Empty current state is deliberately collapsed: recording the complete
	// empty baseline prevents already-resolved items from reappearing in the
	// next delta without sending a content-free notification.
	if len(snapshot.Items) == 0 {
		if err := r.persistBaseline(ctx, snapshot); err != nil {
			return RunResult{}, err
		}
		result.Suppressed = true
		return result, nil
	}
	if r.notifier == nil {
		return RunResult{}, fmt.Errorf("escalator notifier is not configured")
	}
	records := r.notifier.Notify(ctx, buildNotification(snapshot, delta))
	delivered := false
	for _, record := range records {
		if record.Status == "success" {
			delivered = true
			break
		}
	}
	if !delivered {
		return RunResult{}, fmt.Errorf("deliver escalator digest: no notification channel succeeded")
	}
	if err := r.persistBaseline(ctx, snapshot); err != nil {
		return RunResult{}, err
	}
	result.Notified = true
	return result, nil
}

func (r *Runner) loadPrevious(ctx context.Context) (*Snapshot, error) {
	events, err := r.repositories.Events.ListByEntityTypeAndEventTypes(ctx, digestEntityType, []string{DigestEventType})
	if err != nil {
		return nil, fmt.Errorf("load previous escalator digest: %w", err)
	}
	if len(events) == 0 {
		return nil, nil
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(events[len(events)-1].PayloadJSON), &snapshot); err != nil {
		return nil, fmt.Errorf("decode previous escalator digest: %w", err)
	}
	if len(snapshot.Items) > r.maxItems {
		return nil, fmt.Errorf("previous escalator digest has %d items, exceeds bounded baseline limit %d", len(snapshot.Items), r.maxItems)
	}
	return &snapshot, nil
}

func (r *Runner) persistBaseline(ctx context.Context, snapshot Snapshot) error {
	entityType, entityID := digestEntityType, digestEntityID
	actorType, actorID := "system", "escalator"
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode escalator digest baseline: %w", err)
	}
	createdAt := eventlog.FormatJavaScriptISOString(r.now().UTC())
	if err := r.repositories.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("escalator-digest"), EventType: DigestEventType,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(encoded), CreatedAt: createdAt,
	}); err != nil {
		return fmt.Errorf("persist escalator digest baseline: %w", err)
	}
	return nil
}

func buildNotification(snapshot Snapshot, delta Delta) notifyinfra.SystemNotificationPayload {
	lines := []string{fmt.Sprintf("Since previous digest: %d new, %d changed, %d resolved", len(delta.Added), len(delta.Changed), len(delta.Resolved))}
	lastReason := Reason("")
	for _, item := range snapshot.Items {
		if item.Reason != lastReason {
			lines = append(lines, "", "### "+string(item.Reason))
			lastReason = item.Reason
		}
		lines = append(lines, fmt.Sprintf("- [%s] %s (%s) — %s", item.Reason, item.Title, formatAge(item.AgeSeconds), item.Link))
	}
	if len(delta.Resolved) > 0 {
		lines = append(lines, "", "### resolved")
		for _, item := range delta.Resolved {
			lines = append(lines, "- "+item.Title)
		}
	}
	if len(snapshot.Backlog) > 0 {
		lines = append(lines, "", "### stage backlog")
		for _, backlog := range snapshot.Backlog {
			lines = append(lines, fmt.Sprintf("- %s/%s: %d item(s), oldest %s", backlog.ProjectID, backlog.Stage, backlog.Depth, formatAge(backlog.OldestAgeSeconds)))
		}
	}
	return notifyinfra.SystemNotificationPayload{
		Level: "warning", Title: fmt.Sprintf("Looper needs attention on %d item(s)", len(snapshot.Items)),
		Subtitle: fmt.Sprintf("+%d changed %d resolved %d", len(delta.Added), len(delta.Changed), len(delta.Resolved)),
		Body:     strings.Join(lines, "\n"), Group: "escalator", EntityType: digestEntityType, EntityID: digestEntityID,
		DedupeKey: "escalator:" + snapshot.GeneratedAt,
	}
}

func formatAge(seconds int64) string {
	duration := time.Duration(seconds) * time.Second
	if duration >= 24*time.Hour {
		return fmt.Sprintf("%dd", int64(duration/(24*time.Hour)))
	}
	if duration >= time.Hour {
		return fmt.Sprintf("%dh", int64(duration/time.Hour))
	}
	return fmt.Sprintf("%dm", int64(duration/time.Minute))
}
