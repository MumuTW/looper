// Package postmergedigest assembles the daily coordinator audit from local,
// durable records. It deliberately has no forge client: EventLog, snapshots,
// and loops are the authority for what the digest reports.
package postmergedigest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/infra/notify"
	"github.com/MumuTW/looper/internal/storage"
)

const (
	defaultMaxItems  = 50
	digestEntityType = "post_merge_digest"
)

// Config is the effective runtime projection of roles.coordinator.postMergeDigest.
type Config struct {
	Enabled      bool
	Schedule     string
	Timezone     string
	IncludeEmpty bool
	MaxItems     int
}

// ConfigFromRole returns the disabled projection for a nil (legacy/default)
// config. Keeping nil disabled avoids changing existing config artifacts.
func ConfigFromRole(value *config.CoordinatorPostMergeDigestConfig) Config {
	if value == nil {
		return Config{}
	}
	return Config{Enabled: value.Enabled, Schedule: value.Schedule, Timezone: value.Timezone, IncludeEmpty: value.IncludeEmpty, MaxItems: value.MaxItems}
}

type DiffSummary struct {
	Files     int  `json:"files"`
	Additions int  `json:"additions"`
	Deletions int  `json:"deletions"`
	Available bool `json:"available"`
}

type MergedItem struct {
	Repo            string      `json:"repo"`
	PRNumber        int64       `json:"prNumber"`
	IssueNumber     int64       `json:"issueNumber,omitempty"`
	Title           string      `json:"title,omitempty"`
	HeadSHA         string      `json:"headSha,omitempty"`
	MergedAt        string      `json:"mergedAt"`
	ReviewerVerdict string      `json:"reviewerVerdict,omitempty"`
	GateReasons     []string    `json:"gateReasons,omitempty"`
	Diff            DiffSummary `json:"diff"`
}

type RegeneratedItem struct {
	Repo         string `json:"repo,omitempty"`
	PRNumber     int64  `json:"prNumber,omitempty"`
	IssueNumber  int64  `json:"issueNumber,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	RetrySuccess bool   `json:"retrySuccess"`
	Summary      string `json:"summary,omitempty"`
	At           string `json:"at"`
}

type AwaitingHumanItem struct {
	LoopID     string `json:"loopId"`
	ProjectID  string `json:"projectId"`
	Repo       string `json:"repo,omitempty"`
	PRNumber   int64  `json:"prNumber,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	AgeSeconds int64  `json:"ageSeconds"`
	UpdatedAt  string `json:"updatedAt"`
}

type Anomaly struct {
	Repo       string   `json:"repo,omitempty"`
	PRNumber   int64    `json:"prNumber,omitempty"`
	Kind       string   `json:"kind"`
	Reasons    []string `json:"reasons,omitempty"`
	ObservedAt string   `json:"observedAt"`
}

// Digest is the stable four-section dashboard/notification projection.
type Digest struct {
	Date                 string              `json:"date"`
	GeneratedAt          string              `json:"generatedAt"`
	Empty                bool                `json:"empty"`
	Merged               []MergedItem        `json:"merged"`
	ClosedAndRegenerated []RegeneratedItem   `json:"closedAndRegenerated"`
	AwaitingHuman        []AwaitingHumanItem `json:"awaitingHuman"`
	Anomalies            []Anomaly           `json:"anomalies"`
}

// Options wires storage, clock, and the existing notification gateway.
type Options struct {
	Repos   *storage.Repositories
	Config  Config
	Now     func() time.Time
	Deliver func(context.Context, notify.SystemNotificationPayload) []storage.NotificationRecord
}

type Service struct {
	repos   *storage.Repositories
	config  Config
	now     func() time.Time
	deliver func(context.Context, notify.SystemNotificationPayload) []storage.NotificationRecord
}

func New(options Options) *Service {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repos: options.Repos, config: options.Config, now: now, deliver: options.Deliver}
}

// Run evaluates the configured local schedule and sends at most one digest per
// local calendar day. A failed delivery leaves no sent marker, so the next
// scheduler tick retries without relying on provider state.
func (s *Service) Run(ctx context.Context) error {
	if s == nil || !s.config.Enabled {
		return nil
	}
	if s.repos == nil || s.repos.Events == nil {
		return errors.New("post-merge digest storage is not configured")
	}
	now := s.now()
	location, scheduled, err := scheduleState(s.config, now)
	if err != nil {
		return err
	}
	if !scheduled {
		return nil
	}
	localNow := now.In(location)
	// A scheduled run closes the previous local calendar day. Using a complete
	// day avoids silently dropping events that occur after the delivery time;
	// delayed observations still retain their forge merge timestamp below.
	reportDay := localNow.AddDate(0, 0, -1)
	dayKey := reportDay.Format("2006-01-02")
	sent, err := s.repos.Events.ListByEntity(ctx, digestEntityType, dayKey)
	if err != nil {
		return fmt.Errorf("check post-merge digest marker: %w", err)
	}
	for _, event := range sent {
		if event.EventType == eventlog.PostMergeDigestSentEventType {
			return nil
		}
	}
	dedupeKey := "post-merge-digest:" + dayKey
	if s.repos.Notifications != nil {
		records, listErr := s.repos.Notifications.ListByDedupe(ctx, dedupeKey)
		if listErr != nil {
			return fmt.Errorf("check post-merge digest delivery history: %w", listErr)
		}
		if notificationSucceeded(records) {
			// A provider can succeed while the following event-log append fails.
			// Recover the durable marker instead of delivering the same digest twice.
			return s.markSent(ctx, dayKey, Digest{Date: dayKey}, "recovered")
		}
	}
	digest, err := assembleForDate(ctx, s.repos, s.config, now, reportDay)
	if err != nil {
		return fmt.Errorf("assemble post-merge digest: %w", err)
	}
	if digest.Empty && !s.config.IncludeEmpty {
		return s.markSent(ctx, dayKey, digest, "empty")
	}
	if s.deliver == nil {
		return errors.New("post-merge digest notification delivery is not configured")
	}
	records := s.deliver(ctx, notify.SystemNotificationPayload{
		ID:         "post-merge-digest-" + dayKey,
		Level:      digestLevel(digest),
		Title:      "Daily post-merge digest",
		Subtitle:   dayKey,
		Body:       Format(digest),
		EntityType: digestEntityType,
		EntityID:   dayKey,
		DedupeKey:  dedupeKey,
	})
	if !notificationSucceeded(records) {
		return errors.New("post-merge digest notification delivery failed")
	}
	return s.markSent(ctx, dayKey, digest, "delivered")
}

func (s *Service) markSent(ctx context.Context, dayKey string, digest Digest, status string) error {
	entityType := digestEntityType
	entityID := dayKey
	payload := map[string]any{
		"date": digest.Date, "status": status,
		"sections": map[string]int{
			"merged":               len(digest.Merged),
			"closedAndRegenerated": len(digest.ClosedAndRegenerated),
			"awaitingHuman":        len(digest.AwaitingHuman),
			"anomalies":            len(digest.Anomalies),
		},
	}
	if err := eventlog.Append(ctx, s.repos, eventlog.AppendInput{
		EventType:  eventlog.PostMergeDigestSentEventType,
		EntityType: &entityType, EntityID: &entityID, Payload: payload, CreatedAt: s.now(),
	}); err != nil {
		return fmt.Errorf("record post-merge digest marker: %w", err)
	}
	return nil
}

func notificationSucceeded(records []storage.NotificationRecord) bool {
	for _, record := range records {
		if strings.EqualFold(strings.TrimSpace(record.Status), "success") {
			return true
		}
	}
	return false
}

func digestLevel(digest Digest) string {
	if len(digest.AwaitingHuman) > 0 || len(digest.Anomalies) > 0 {
		return "action_required"
	}
	return "info"
}

func scheduleState(cfg Config, now time.Time) (*time.Location, bool, error) {
	if strings.TrimSpace(cfg.Schedule) == "" {
		return nil, false, errors.New("post-merge digest schedule is empty")
	}
	location, err := time.LoadLocation(strings.TrimSpace(cfg.Timezone))
	if err != nil {
		return nil, false, fmt.Errorf("load post-merge digest timezone: %w", err)
	}
	schedule, err := time.ParseInLocation("15:04", strings.TrimSpace(cfg.Schedule), location)
	if err != nil {
		return nil, false, fmt.Errorf("parse post-merge digest schedule: %w", err)
	}
	localNow := now.In(location)
	scheduledAt := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), schedule.Hour(), schedule.Minute(), 0, 0, location)
	return location, !localNow.Before(scheduledAt), nil
}

// Assemble is pure with respect to external systems: it only reads the
// repositories supplied by the caller and never invokes GitHub or an agent.
func Assemble(ctx context.Context, repos *storage.Repositories, cfg Config, now time.Time) (Digest, error) {
	if repos == nil || repos.Events == nil || repos.Loops == nil {
		return Digest{}, errors.New("post-merge digest repositories are not configured")
	}
	location, err := time.LoadLocation(strings.TrimSpace(cfg.Timezone))
	if err != nil {
		return Digest{}, fmt.Errorf("load post-merge digest timezone: %w", err)
	}
	return assembleForDate(ctx, repos, cfg, now, now.In(location))
}

func assembleForDate(ctx context.Context, repos *storage.Repositories, cfg Config, now, reportDay time.Time) (Digest, error) {
	if repos == nil || repos.Events == nil || repos.Loops == nil {
		return Digest{}, errors.New("post-merge digest repositories are not configured")
	}
	location, err := time.LoadLocation(strings.TrimSpace(cfg.Timezone))
	if err != nil {
		return Digest{}, fmt.Errorf("load post-merge digest timezone: %w", err)
	}
	localDay := reportDay.In(location)
	dayStart := time.Date(localDay.Year(), localDay.Month(), localDay.Day(), 0, 0, 0, 0, location)
	dayEnd := dayStart.AddDate(0, 0, 1)
	allEvents, err := repos.Events.ListAll(ctx)
	if err != nil {
		return Digest{}, err
	}
	maxItems := cfg.MaxItems
	if maxItems <= 0 {
		maxItems = defaultMaxItems
	}
	digest := Digest{Date: localDay.Format("2006-01-02"), GeneratedAt: now.UTC().Format(time.RFC3339Nano), Merged: []MergedItem{}, ClosedAndRegenerated: []RegeneratedItem{}, AwaitingHuman: []AwaitingHumanItem{}, Anomalies: []Anomaly{}}
	gateReports := latestGateReports(allEvents)
	reviews := reviewObservations(allEvents)
	mergedByEntity := map[string]MergedItem{}
	for _, event := range allEvents {
		if isMergedEvent(event) {
			item, merged, err := parseMerged(event)
			if err != nil {
				return Digest{}, err
			}
			if !merged || item.PRNumber <= 0 {
				continue
			}
			mergeAt := item.MergedAt
			if _, ok := parseEventTime(mergeAt); !ok {
				mergeAt = event.CreatedAt
			}
			mergedTime, ok := parseEventTime(mergeAt)
			if !ok || mergedTime.Before(dayStart.UTC()) || !mergedTime.Before(dayEnd.UTC()) {
				continue
			}
			item.MergedAt = mergeAt
			key := mergeKey(item.Repo, item.PRNumber)
			if prior, exists := mergedByEntity[key]; exists {
				if prior.MergedAt > item.MergedAt || (prior.MergedAt == item.MergedAt && prior.HeadSHA != "" && item.HeadSHA == "") {
					continue
				}
			}
			mergedByEntity[key] = item
		} else if isRegenerationEvent(event.EventType) {
			created, ok := parseEventTime(event.CreatedAt)
			if !ok || created.Before(dayStart.UTC()) || !created.Before(dayEnd.UTC()) {
				continue
			}
			if item, ok := parseRegenerated(event); ok {
				digest.ClosedAndRegenerated = append(digest.ClosedAndRegenerated, item)
			}
		}
	}
	for _, item := range mergedByEntity {
		entity := mergeKey(item.Repo, item.PRNumber)
		if report, ok := gateReportForHead(gateReports[entity], item.HeadSHA, item.MergedAt); ok {
			item.ReviewerVerdict = firstString(report, "status", "verdict")
			item.GateReasons = reasonCodes(report)
		} else {
			kind, reason := "missing-gate-report", "no durable gate report for merged head"
			if len(gateReports[entity]) > 0 {
				kind, reason = "stale-gate-report", "durable gate reports do not match merged head"
			}
			digest.Anomalies = append(digest.Anomalies, Anomaly{Repo: item.Repo, PRNumber: item.PRNumber, Kind: kind, Reasons: []string{reason}, ObservedAt: item.MergedAt})
		}
		if review, ok := latestReviewAtOrBefore(reviews[entity], item.MergedAt, item.HeadSHA); ok {
			item.ReviewerVerdict = review.Verdict
			if strings.EqualFold(review.Verdict, "comment") || strings.EqualFold(review.Verdict, "request_changes") {
				digest.Anomalies = append(digest.Anomalies, Anomaly{Repo: item.Repo, PRNumber: item.PRNumber, Kind: "reviewer-non-blocking-issue", Reasons: []string{review.Verdict}, ObservedAt: review.At})
			}
		}
		if flipped, ok := reviewFlippedNearMerge(reviews[entity], item.MergedAt); ok {
			digest.Anomalies = append(digest.Anomalies, Anomaly{Repo: item.Repo, PRNumber: item.PRNumber, Kind: "review-verdict-flipped-near-merge", Reasons: []string{flipped}, ObservedAt: item.MergedAt})
		}
		if repos.PullRequestSnapshots != nil {
			var snapshot *storage.PullRequestSnapshotRecord
			if projectID := projectIDForMergedEvent(allEvents, item.Repo, item.PRNumber); projectID != "" {
				snapshot, err = repos.PullRequestSnapshots.GetLatestByProject(ctx, projectID, item.Repo, item.PRNumber)
			} else {
				var snapshots []storage.PullRequestSnapshotRecord
				snapshots, err = repos.PullRequestSnapshots.ListLatestByRepoAndPR(ctx, item.Repo, item.PRNumber)
				if err == nil && len(snapshots) > 0 {
					snapshot = &snapshots[0]
				}
			}
			if err != nil {
				return Digest{}, err
			}
			if snapshot != nil {
				snapshotHead := strings.TrimSpace(snapshot.HeadSHA)
				if item.HeadSHA != "" && snapshotHead != "" && !strings.EqualFold(item.HeadSHA, snapshotHead) {
					digest.Anomalies = append(digest.Anomalies, Anomaly{Repo: item.Repo, PRNumber: item.PRNumber, Kind: "snapshot-head-mismatch", Reasons: []string{"latest snapshot is for a different head"}, ObservedAt: item.MergedAt})
				} else {
					item.Title = stringValue(snapshot.Title)
					if item.HeadSHA == "" {
						item.HeadSHA = snapshotHead
					}
					item.Diff = diffSummary(snapshot.PayloadJSON)
				}
			}
		}
		digest.Merged = append(digest.Merged, item)
	}
	awaiting, err := repos.Loops.ListByStatuses(ctx, []string{"awaiting_human", "human_takeover", "paused"})
	if err != nil {
		return Digest{}, err
	}
	for _, loop := range awaiting {
		item := AwaitingHumanItem{LoopID: loop.ID, ProjectID: loop.ProjectID, Status: loop.Status, Repo: stringValue(loop.Repo), PRNumber: intValue(loop.PRNumber), UpdatedAt: loop.UpdatedAt}
		item.Reason, item.ReasonCode = loopReason(loop.MetadataJSON)
		if updated, ok := parseEventTime(loop.UpdatedAt); ok {
			age := now.UTC().Sub(updated)
			if age > 0 {
				item.AgeSeconds = int64(age / time.Second)
			}
		}
		digest.AwaitingHuman = append(digest.AwaitingHuman, item)
	}
	sort.Slice(digest.Merged, func(i, j int) bool { return digest.Merged[i].MergedAt < digest.Merged[j].MergedAt })
	sort.Slice(digest.ClosedAndRegenerated, func(i, j int) bool { return digest.ClosedAndRegenerated[i].At < digest.ClosedAndRegenerated[j].At })
	sort.Slice(digest.AwaitingHuman, func(i, j int) bool { return digest.AwaitingHuman[i].AgeSeconds > digest.AwaitingHuman[j].AgeSeconds })
	sort.Slice(digest.Anomalies, func(i, j int) bool { return digest.Anomalies[i].ObservedAt < digest.Anomalies[j].ObservedAt })
	digest.Merged = limitMerged(digest.Merged, maxItems)
	digest.ClosedAndRegenerated = limitRegenerated(digest.ClosedAndRegenerated, maxItems)
	digest.AwaitingHuman = limitAwaiting(digest.AwaitingHuman, maxItems)
	digest.Anomalies = limitAnomalies(digest.Anomalies, maxItems)
	digest.Empty = len(digest.Merged) == 0 && len(digest.ClosedAndRegenerated) == 0 && len(digest.AwaitingHuman) == 0 && len(digest.Anomalies) == 0
	return digest, nil
}

func isMergedEvent(event storage.EventLogRecord) bool {
	if event.EventType == eventlog.CoordinatorPullRequestMergedEventType || event.EventType == gatekeeper.MergeOutcomeEventType {
		return true
	}
	// Preserve compatibility with webhook/older coordinator event names while
	// requiring a durable merged=true payload so merge attempts are not counted.
	t := strings.ToLower(event.EventType)
	return strings.Contains(t, "merged") || strings.Contains(t, "merge.completed")
}

type gateReportObservation struct {
	payload   map[string]any
	createdAt string
}

func latestGateReports(events []storage.EventLogRecord) map[string][]gateReportObservation {
	reports := make(map[string][]gateReportObservation)
	for _, event := range events {
		if event.EventType != gatekeeper.GateReportEventType || event.EntityID == nil {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil {
			continue
		}
		key := strings.TrimSpace(*event.EntityID)
		reports[key] = append(reports[key], gateReportObservation{payload: payload, createdAt: event.CreatedAt})
	}
	for key := range reports {
		sort.SliceStable(reports[key], func(i, j int) bool { return reports[key][i].createdAt < reports[key][j].createdAt })
	}
	return reports
}

// gateReportForHead selects the latest durable gate report for the merged head
// that was recorded before the merge. A same-head report written after the merge
// (e.g. a `pull_request_not_open` blocked outcome from the closed webhook) is
// not evidence for the revision that was actually evaluated, so it is skipped
// along with reports for other heads. When no head-matching pre-merge report
// exists the caller emits the missing/stale-gate-report anomaly.
func gateReportForHead(observations []gateReportObservation, headSHA, mergedAt string) (map[string]any, bool) {
	if len(observations) == 0 {
		return nil, false
	}
	mergeTime, haveMergeTime := parseEventTime(mergedAt)
	for i := len(observations) - 1; i >= 0; i-- {
		if head := strings.TrimSpace(headSHA); head != "" && !strings.EqualFold(head, gateReportHead(observations[i].payload)) {
			continue
		}
		if haveMergeTime {
			if reportAt, ok := parseEventTime(observations[i].createdAt); ok && !reportAt.Before(mergeTime) {
				continue
			}
		}
		return observations[i].payload, true
	}
	return nil, false
}

func gateReportHead(payload map[string]any) string {
	if evidence, ok := payload["evidence"].(map[string]any); ok {
		if head := firstString(evidence, "finalObservedHeadSha", "finalObservedHeadSHA", "observedHeadSha", "observedHeadSHA", "headSha", "headSHA"); head != "" {
			return head
		}
	}
	return firstString(payload, "observedHeadSha", "observedHeadSHA", "finalObservedHeadSha", "finalObservedHeadSHA", "headSha", "headSHA", "head")
}

type reviewObservation struct {
	Verdict string
	HeadSHA string
	At      string
}

func reviewObservations(events []storage.EventLogRecord) map[string][]reviewObservation {
	observations := make(map[string][]reviewObservation)
	for _, event := range events {
		if event.EventType != "pr.review.posted" || event.EntityID == nil {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil {
			continue
		}
		verdict := firstString(payload, "event", "verdict", "reviewEvent")
		if verdict == "" {
			continue
		}
		key := strings.TrimSpace(*event.EntityID)
		observations[key] = append(observations[key], reviewObservation{Verdict: verdict, HeadSHA: firstString(payload, "headSha", "headSHA"), At: event.CreatedAt})
	}
	for key := range observations {
		sort.Slice(observations[key], func(i, j int) bool { return observations[key][i].At < observations[key][j].At })
	}
	return observations
}

func latestReviewAtOrBefore(observations []reviewObservation, mergedAt, headSHA string) (reviewObservation, bool) {
	mergeTime, ok := parseEventTime(mergedAt)
	if !ok {
		return reviewObservation{}, false
	}
	var result reviewObservation
	found := false
	for _, observation := range observations {
		at, parsed := parseEventTime(observation.At)
		if !parsed || at.After(mergeTime) || (headSHA != "" && observation.HeadSHA != "" && observation.HeadSHA != headSHA) {
			continue
		}
		if !found || at.After(mustParseEventTime(result.At)) {
			result, found = observation, true
		}
	}
	return result, found
}

func reviewFlippedNearMerge(observations []reviewObservation, mergedAt string) (string, bool) {
	mergeTime, ok := parseEventTime(mergedAt)
	if !ok || len(observations) < 2 {
		return "", false
	}
	for i := 1; i < len(observations); i++ {
		previous, previousOK := parseEventTime(observations[i-1].At)
		current, currentOK := parseEventTime(observations[i].At)
		if !previousOK || !currentOK || current.After(mergeTime) || mergeTime.Sub(previous) > 30*time.Minute || observations[i-1].Verdict == observations[i].Verdict {
			continue
		}
		return observations[i-1].Verdict + " -> " + observations[i].Verdict, true
	}
	return "", false
}

func mustParseEventTime(value string) time.Time {
	parsed, _ := parseEventTime(value)
	return parsed
}

func parseMerged(event storage.EventLogRecord) (MergedItem, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
		return MergedItem{}, false, fmt.Errorf("decode merge event %s: %w", event.ID, err)
	}
	merged, _ := payload["merged"].(bool)
	if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
		merged = true
	}
	repo := firstString(payload, "repo", "repository")
	pr := firstInt(payload, "prNumber", "pullRequestNumber", "number")
	if repo == "" && event.EntityID != nil {
		repo, pr = parseEntity(*event.EntityID)
	}
	return MergedItem{Repo: repo, PRNumber: pr, IssueNumber: firstInt(payload, "issueNumber"), HeadSHA: firstString(payload, "headSha", "headSHA", "head"), MergedAt: firstString(payload, "mergedAt", "attemptedAt")}, merged, nil
}

func parseRegenerated(event storage.EventLogRecord) (RegeneratedItem, bool) {
	var payload map[string]any
	if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil {
		return RegeneratedItem{}, false
	}
	item := RegeneratedItem{Repo: firstString(payload, "repo", "repository"), PRNumber: firstInt(payload, "prNumber", "pullRequestNumber", "number"), IssueNumber: firstInt(payload, "issueNumber"), Fingerprint: firstString(payload, "failureFingerprint", "fingerprint"), RetrySuccess: boolValue(payload, "retrySuccess", "regenerated", "success"), Summary: firstString(payload, "summary", "reason"), At: event.CreatedAt}
	if item.Repo == "" && event.EntityID != nil {
		item.Repo, item.PRNumber = parseEntity(*event.EntityID)
	}
	return item, item.Repo != "" || item.PRNumber > 0
}

func isRegenerationEvent(eventType string) bool {
	if eventType == eventlog.CoordinatorCloseAndRegenerateEventType || eventType == eventlog.FixerCloseAndRegenerateEventType {
		return true
	}
	t := strings.ToLower(eventType)
	return (strings.Contains(t, "regenerat") || strings.Contains(t, "close_and")) && (strings.HasPrefix(t, "coordinator.") || strings.HasPrefix(t, "fixer.") || strings.HasPrefix(t, "pull_request."))
}

func projectIDForMergedEvent(events []storage.EventLogRecord, repo string, pr int64) string {
	key := mergeKey(repo, pr)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.EventType != eventlog.CoordinatorPullRequestMergedEventType && event.EventType != gatekeeper.MergeOutcomeEventType || event.EntityID == nil || !strings.EqualFold(strings.TrimSpace(*event.EntityID), key) || event.ProjectID == nil {
			continue
		}
		return strings.TrimSpace(*event.ProjectID)
	}
	return ""
}

func mergeKey(repo string, pr int64) string {
	return strings.TrimSpace(repo) + "#" + strconv.FormatInt(pr, 10)
}

func parseEntity(entity string) (string, int64) {
	parts := strings.Split(strings.TrimSpace(entity), "#")
	if len(parts) != 2 {
		return strings.TrimSpace(entity), 0
	}
	pr, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	return strings.TrimSpace(parts[0]), pr
}

func parseEventTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return parsed, err == nil
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstInt(payload map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case float64:
			return int64(value)
		case json.Number:
			parsed, _ := value.Int64()
			return parsed
		case string:
			parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if parsed != 0 {
				return parsed
			}
		}
	}
	return 0
}

func boolValue(payload map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := payload[key].(bool); ok {
			return value
		}
	}
	return false
}

func reasonCodes(payload map[string]any) []string {
	values, _ := payload["reasons"].([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if reason, ok := value.(map[string]any); ok {
			if code := firstString(reason, "code", "reason"); code != "" {
				result = append(result, code)
			}
		} else if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			result = append(result, strings.TrimSpace(text))
		}
	}
	return result
}

func loopReason(metadata *string) (string, string) {
	if metadata == nil {
		return "", ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(*metadata), &payload) != nil {
		return "", ""
	}
	reason := firstString(payload, "reason", "failureReason", "humanReason", "message", "pauseReason")
	code := firstString(payload, "reasonCode", "failureKind", "kind", "pauseReasonCode")
	if human, ok := payload["human"].(map[string]any); ok {
		if reason == "" {
			reason = firstString(human, "reason", "message")
		}
		if code == "" {
			code = firstString(human, "reasonCode", "kind")
		}
	}
	if hitl, ok := payload["hitl"].(map[string]any); ok {
		if reason == "" {
			reason = firstString(hitl, "question", "reason", "message")
		}
		if code == "" {
			code = firstString(hitl, "reasonCode", "kind", "status")
		}
	}
	return reason, code
}

func diffSummary(payload *string) DiffSummary {
	if payload == nil || strings.TrimSpace(*payload) == "" {
		return DiffSummary{}
	}
	var value map[string]any
	if json.Unmarshal([]byte(*payload), &value) != nil {
		return DiffSummary{}
	}
	result := DiffSummary{Files: int(firstInt(value, "changedFiles", "filesChanged", "files")), Additions: int(firstInt(value, "additions", "addedLines")), Deletions: int(firstInt(value, "deletions", "deletedLines"))}
	if diff, ok := value["diff"].(string); ok {
		result.Available = true
		for _, line := range strings.Split(diff, "\n") {
			if strings.HasPrefix(line, "diff --git ") {
				result.Files++
			} else if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				result.Additions++
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				result.Deletions++
			}
		}
	}
	if result.Files > 0 || result.Additions > 0 || result.Deletions > 0 {
		result.Available = true
	}
	return result
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func intValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
func limitMerged(value []MergedItem, n int) []MergedItem {
	if len(value) > n {
		return value[:n]
	}
	return value
}
func limitRegenerated(value []RegeneratedItem, n int) []RegeneratedItem {
	if len(value) > n {
		return value[:n]
	}
	return value
}
func limitAwaiting(value []AwaitingHumanItem, n int) []AwaitingHumanItem {
	if len(value) > n {
		return value[:n]
	}
	return value
}
func limitAnomalies(value []Anomaly, n int) []Anomaly {
	if len(value) > n {
		return value[:n]
	}
	return value
}

// Format is shared by notifications and CLI/API consumers so section names and
// empty-day behavior stay consistent across surfaces.
func Format(digest Digest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Post-merge digest · %s\n\n", digest.Date)
	formatMerged(&b, digest.Merged)
	formatRegenerated(&b, digest.ClosedAndRegenerated)
	formatAwaiting(&b, digest.AwaitingHuman)
	formatAnomalies(&b, digest.Anomalies)
	return strings.TrimSpace(b.String())
}

func formatMerged(b *strings.Builder, items []MergedItem) {
	fmt.Fprintf(b, "### Merged (%d)\n", len(items))
	if len(items) == 0 {
		b.WriteString("- None\n")
	}
	for _, item := range items {
		line := fmt.Sprintf("- %s#%d", item.Repo, item.PRNumber)
		if item.Title != "" {
			line += " — " + item.Title
		}
		if item.ReviewerVerdict != "" {
			line += " · reviewer " + item.ReviewerVerdict
		}
		if len(item.GateReasons) > 0 {
			line += " · gates " + strings.Join(item.GateReasons, ", ")
		}
		if item.Diff.Available {
			line += fmt.Sprintf(" · diff %d files +%d/-%d", item.Diff.Files, item.Diff.Additions, item.Diff.Deletions)
		}
		b.WriteString(line + "\n")
	}
}

func formatRegenerated(b *strings.Builder, items []RegeneratedItem) {
	fmt.Fprintf(b, "### Closed-and-regenerated (%d)\n", len(items))
	if len(items) == 0 {
		b.WriteString("- None\n")
	}
	for _, item := range items {
		line := fmt.Sprintf("- %s#%d", item.Repo, item.PRNumber)
		if item.Fingerprint != "" {
			line += " · " + item.Fingerprint
		}
		line += " · retry success: " + strconv.FormatBool(item.RetrySuccess)
		if item.Summary != "" {
			line += " · " + item.Summary
		}
		b.WriteString(line + "\n")
	}
}

func formatAwaiting(b *strings.Builder, items []AwaitingHumanItem) {
	fmt.Fprintf(b, "### Awaiting human (%d)\n", len(items))
	if len(items) == 0 {
		b.WriteString("- None\n")
	}
	for _, item := range items {
		line := "- " + item.LoopID
		if item.Repo != "" {
			line += " · " + item.Repo
		}
		if item.PRNumber > 0 {
			line += fmt.Sprintf("#%d", item.PRNumber)
		}
		if item.ReasonCode != "" {
			line += " · " + item.ReasonCode
		}
		if item.Reason != "" {
			line += " · " + item.Reason
		}
		line += fmt.Sprintf(" · age %ds", item.AgeSeconds)
		b.WriteString(line + "\n")
	}
}

func formatAnomalies(b *strings.Builder, items []Anomaly) {
	fmt.Fprintf(b, "### Anomalies (%d)\n", len(items))
	if len(items) == 0 {
		b.WriteString("- None\n")
	}
	for _, item := range items {
		line := fmt.Sprintf("- %s#%d · %s", item.Repo, item.PRNumber, item.Kind)
		if len(item.Reasons) > 0 {
			line += " · " + strings.Join(item.Reasons, ", ")
		}
		b.WriteString(line + "\n")
	}
}
