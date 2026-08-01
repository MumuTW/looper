package escalator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/triager"
)

type Linker interface {
	Issue(projectID, repo string, number int64) string
	PullRequest(projectID, repo string, number int64) string
	Loop(projectID string, seq int64) string
}

type CollectorOptions struct {
	Now                   func() time.Time
	RetryAttemptThreshold int64
	UnroutedAfter         time.Duration
	StaleHeadAfter        time.Duration
}

type Collector struct {
	repositories  *storage.Repositories
	links         Linker
	now           func() time.Time
	retryAfter    int64
	unroutedAfter time.Duration
	staleAfter    time.Duration
}

func NewCollector(repositories *storage.Repositories, links Linker, options CollectorOptions) *Collector {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	retryAfter := options.RetryAttemptThreshold
	if retryAfter <= 0 {
		retryAfter = 2
	}
	unroutedAfter := options.UnroutedAfter
	if unroutedAfter <= 0 {
		unroutedAfter = time.Hour
	}
	staleAfter := options.StaleHeadAfter
	if staleAfter <= 0 {
		staleAfter = 24 * time.Hour
	}
	return &Collector{repositories: repositories, links: links, now: now, retryAfter: retryAfter, unroutedAfter: unroutedAfter, staleAfter: staleAfter}
}

func (c *Collector) Collect(ctx context.Context) (Snapshot, error) {
	if c.repositories == nil || c.repositories.Projects == nil || c.repositories.Loops == nil || c.repositories.Queue == nil || c.repositories.Events == nil || c.repositories.PullRequestSnapshots == nil {
		return Snapshot{}, fmt.Errorf("escalator repositories are not configured")
	}
	if c.links == nil {
		return Snapshot{}, fmt.Errorf("escalator links are not configured")
	}
	now := c.now().UTC()
	projects, err := c.repositories.Projects.List(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list escalator projects: %w", err)
	}
	activeProjects := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		if !project.Archived {
			activeProjects[project.ID] = struct{}{}
		}
	}
	loopRecords, err := c.repositories.Loops.List(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list escalator loops: %w", err)
	}
	queueRecords, err := c.repositories.Queue.List(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list escalator queue: %w", err)
	}

	allLoops := make(map[string]storage.LoopRecord, len(loopRecords))
	activeLoops := make(map[string]storage.LoopRecord)
	blockedByLoop := map[string]int{}
	for _, item := range queueRecords {
		if item.LoopID == nil || queueTerminal(item.Status) {
			continue
		}
		blockedByLoop[*item.LoopID]++
	}
	for _, loop := range loopRecords {
		allLoops[loop.ID] = loop
		if _, active := activeProjects[loop.ProjectID]; !active || !domain.IsActiveLoopStatus(domain.LoopStatus(loop.Status)) {
			continue
		}
		activeLoops[loop.ID] = loop
	}

	backlog, err := buildBacklog(now, activeLoops)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{GeneratedAt: eventlog.FormatJavaScriptISOString(now), Items: []Item{}, Backlog: backlog}
	if err := c.collectTriageConfirmations(ctx, now, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := c.collectLoops(now, activeLoops, blockedByLoop, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := c.collectQueue(now, queueRecords, allLoops, activeProjects, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := c.collectUnroutedTriage(ctx, now, activeProjects, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := c.collectEligibleAdvise(ctx, now, activeProjects, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := c.collectStaleHeads(ctx, now, activeLoops, &snapshot); err != nil {
		return Snapshot{}, err
	}
	snapshot.Normalize()
	return snapshot, nil
}

func (c *Collector) collectTriageConfirmations(ctx context.Context, now time.Time, snapshot *Snapshot) error {
	summary, err := triager.AwaitingConfirmationStatus(ctx, c.repositories, now)
	if err != nil {
		return fmt.Errorf("collect triage confirmations: %w", err)
	}
	for _, source := range summary.Sources {
		link, err := requiredLink(c.links.Issue(source.ProjectID, source.Repo, source.IssueNumber))
		if err != nil {
			return fmt.Errorf("triage confirmation %s#%d: %w", source.Repo, source.IssueNumber, err)
		}
		id := itemID(ReasonTriageConfirmation, source.ProjectID, source.Repo, source.IssueNumber)
		snapshot.Items = append(snapshot.Items, newItem(id, KindWaiting, ReasonTriageConfirmation, source.ProjectID, "triager", fmt.Sprintf("Confirm triage for %s#%d", source.Repo, source.IssueNumber), source.Command, link, source.AgeSeconds, 1, source.Command))
	}
	return nil
}

func (c *Collector) collectLoops(now time.Time, active map[string]storage.LoopRecord, blocked map[string]int, snapshot *Snapshot) error {
	for _, loop := range active {
		age, err := ageSeconds(now, loop.UpdatedAt)
		if err != nil {
			return fmt.Errorf("parse loop %s updatedAt: %w", loop.ID, err)
		}
		link, err := requiredLink(c.links.Loop(loop.ProjectID, loop.Seq))
		if err != nil {
			return fmt.Errorf("loop %s: %w", loop.ID, err)
		}
		blockedWork := 1 + blocked[loop.ID]
		if loop.Status == string(domain.LoopStatusAwaitingHuman) {
			reason, detail := ReasonPlannerEscalation, "Planner needs an operator decision"
			if ask, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok && ask.Status == "awaiting" {
				reason, detail = ReasonHITLQuestion, ask.Question
				if ask.AskedAt != "" {
					age, err = ageSeconds(now, ask.AskedAt)
					if err != nil {
						return fmt.Errorf("parse loop %s HITL askedAt: %w", loop.ID, err)
					}
				}
			}
			snapshot.Items = append(snapshot.Items, newItem(itemID(reason, loop.ProjectID, loop.ID), KindWaiting, reason, loop.ProjectID, loop.Type, fmt.Sprintf("%s loop #%d is waiting on a human", loop.Type, loop.Seq), detail, link, age, blockedWork, loop.Status, detail))
			continue
		}
		if loop.Status == string(domain.LoopStatusPaused) && metadataString(loop.MetadataJSON, "pauseReason") == "agent_failure_streak" {
			snapshot.Items = append(snapshot.Items, newItem(itemID(ReasonCircuitBreaker, loop.ProjectID, loop.ID), KindStuck, ReasonCircuitBreaker, loop.ProjectID, loop.Type, fmt.Sprintf("%s loop #%d tripped its circuit breaker", loop.Type, loop.Seq), "Repeated agent failures paused this loop", link, age, blockedWork, loop.Status, "agent_failure_streak"))
			continue
		}
		if loop.Type == string(domain.LoopTypeReviewer) && loop.Status == string(domain.LoopStatusPaused) {
			snapshot.Items = append(snapshot.Items, newItem(itemID(ReasonReviewStall, loop.ProjectID, loop.ID), KindWaiting, ReasonReviewStall, loop.ProjectID, loop.Type, fmt.Sprintf("Reviewer loop #%d is stalled", loop.Seq), "Review is paused or waiting", link, age, blockedWork, loop.Status))
		}
	}
	return nil
}

func (c *Collector) collectQueue(now time.Time, records []storage.QueueItemRecord, loopsByID map[string]storage.LoopRecord, activeProjects map[string]struct{}, snapshot *Snapshot) error {
	for _, item := range records {
		if item.ProjectID == nil || item.Attempts < c.retryAfter || queueTerminal(item.Status) {
			continue
		}
		if _, active := activeProjects[*item.ProjectID]; !active {
			continue
		}
		age, err := ageSeconds(now, item.CreatedAt)
		if err != nil {
			return fmt.Errorf("parse queue item %s createdAt: %w", item.ID, err)
		}
		var link string
		if item.LoopID != nil {
			if loop, exists := loopsByID[*item.LoopID]; exists {
				link = c.links.Loop(*item.ProjectID, loop.Seq)
			}
		}
		if link == "" && item.Repo != nil && item.PRNumber != nil {
			link = c.links.PullRequest(*item.ProjectID, *item.Repo, *item.PRNumber)
		}
		link, err = requiredLink(link)
		if err != nil {
			return fmt.Errorf("queue item %s: %w", item.ID, err)
		}
		detail := fmt.Sprintf("%d/%d attempts; status %s", item.Attempts, item.MaxAttempts, item.Status)
		snapshot.Items = append(snapshot.Items, newItem(itemID(ReasonQueueRetries, *item.ProjectID, item.ID), KindStuck, ReasonQueueRetries, *item.ProjectID, item.Type, fmt.Sprintf("%s queue item has repeated retries", item.Type), detail, link, age, 1, strconv.FormatInt(item.Attempts, 10), strconv.FormatInt(item.MaxAttempts, 10), item.Status, value(item.LastErrorKind), value(item.LastError)))
	}
	return nil
}

func (c *Collector) collectUnroutedTriage(ctx context.Context, now time.Time, activeProjects map[string]struct{}, snapshot *Snapshot) error {
	events, err := c.repositories.Events.ListByEntityTypeAndEventTypes(ctx, "github_issue", []string{triager.EnrollmentEventType, triager.ReportEventType, triager.ProjectionEventType, triager.RetirementEventType})
	if err != nil {
		return fmt.Errorf("list triage routing lifecycle: %w", err)
	}
	type state struct {
		enrollment         *triager.Enrollment
		report             *triager.Report
		projected, retired bool
	}
	states := map[string]*state{}
	stateFor := func(key string) *state {
		if states[key] == nil {
			states[key] = &state{}
		}
		return states[key]
	}
	for _, event := range events {
		switch event.EventType {
		case triager.EnrollmentEventType:
			var enrollment triager.Enrollment
			if err := json.Unmarshal([]byte(event.PayloadJSON), &enrollment); err != nil {
				return fmt.Errorf("decode triage enrollment for escalator: %w", err)
			}
			copy := enrollment
			stateFor(enrollment.IdempotencyKey).enrollment = &copy
		case triager.ReportEventType:
			var report triager.Report
			if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
				return fmt.Errorf("decode triage report for escalator: %w", err)
			}
			copy := report
			stateFor(report.IdempotencyKey).report = &copy
		case triager.ProjectionEventType:
			var projection triager.Projection
			if err := json.Unmarshal([]byte(event.PayloadJSON), &projection); err != nil {
				return fmt.Errorf("decode triage projection for escalator: %w", err)
			}
			stateFor(projection.ReportKey).projected = true
		case triager.RetirementEventType:
			var retirement triager.Retirement
			if err := json.Unmarshal([]byte(event.PayloadJSON), &retirement); err != nil {
				return fmt.Errorf("decode triage retirement for escalator: %w", err)
			}
			stateFor(retirement.EnrollmentKey).retired = true
		}
	}
	for key, state := range states {
		if state.enrollment == nil || state.projected || state.retired {
			continue
		}
		// Human-confirmation reports are already represented by the dedicated
		// waiting item above. This category is specifically work that has no ask.
		if state.report != nil && state.report.Policy.Action == triager.ActionAwaitHuman {
			continue
		}
		enrollment := state.enrollment
		if _, active := activeProjects[enrollment.ProjectID]; !active {
			continue
		}
		age, err := ageSeconds(now, enrollment.EnrolledAt)
		if err != nil {
			return fmt.Errorf("parse triage enrollment %s: %w", key, err)
		}
		if time.Duration(age)*time.Second < c.unroutedAfter {
			continue
		}
		link, err := requiredLink(c.links.Issue(enrollment.ProjectID, enrollment.Repo, enrollment.IssueNumber))
		if err != nil {
			return fmt.Errorf("triage enrollment %s: %w", key, err)
		}
		detail := "No Triage report or route was persisted"
		if state.report != nil {
			detail = "A Triage report exists, but no Planner route was persisted"
		}
		snapshot.Items = append(snapshot.Items, newItem(itemID(ReasonTriageNotRouted, enrollment.ProjectID, key), KindStuck, ReasonTriageNotRouted, enrollment.ProjectID, "triager", fmt.Sprintf("%s#%d was enrolled but never routed", enrollment.Repo, enrollment.IssueNumber), detail, link, age, 1, enrollment.EnrolledAt, detail))
	}
	return nil
}

func (c *Collector) collectEligibleAdvise(ctx context.Context, now time.Time, activeProjects map[string]struct{}, snapshot *Snapshot) error {
	events, err := c.repositories.Events.ListByEntityTypeAndEventTypes(ctx, "pull_request", []string{gatekeeper.GateReportEventType})
	if err != nil {
		return fmt.Errorf("list Gatekeeper reports: %w", err)
	}
	latest := map[string]gatekeeper.Report{}
	for _, event := range events {
		var report gatekeeper.Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			return fmt.Errorf("decode Gatekeeper report for escalator: %w", err)
		}
		latest[fmt.Sprintf("%s\x00%s\x00%d", report.ProjectID, strings.ToLower(report.Repo), report.PRNumber)] = report
	}
	for _, report := range latest {
		if _, active := activeProjects[report.ProjectID]; !active || !report.Eligible || !strings.EqualFold(report.Mode, "advise") {
			continue
		}
		age, err := ageSeconds(now, report.EvaluatedAt)
		if err != nil {
			return fmt.Errorf("parse Gatekeeper report %s#%d: %w", report.Repo, report.PRNumber, err)
		}
		link, err := requiredLink(c.links.PullRequest(report.ProjectID, report.Repo, report.PRNumber))
		if err != nil {
			return fmt.Errorf("Gatekeeper report %s#%d: %w", report.Repo, report.PRNumber, err)
		}
		snapshot.Items = append(snapshot.Items, newItem(itemID(ReasonEligibleAdvisePR, report.ProjectID, report.Repo, report.PRNumber), KindWaiting, ReasonEligibleAdvisePR, report.ProjectID, "gatekeeper", fmt.Sprintf("%s#%d is eligible to merge", report.Repo, report.PRNumber), "Gatekeeper advise is waiting for a human merge decision", link, age, 1, report.ObservedHeadSHA, report.Status))
	}
	return nil
}

func (c *Collector) collectStaleHeads(ctx context.Context, now time.Time, activeLoops map[string]storage.LoopRecord, snapshot *Snapshot) error {
	records, err := c.repositories.PullRequestSnapshots.List(ctx)
	if err != nil {
		return fmt.Errorf("list pull request snapshots: %w", err)
	}
	type prKey struct {
		project, repo string
		number        int64
	}
	activePRs := map[prKey]struct{}{}
	for _, loop := range activeLoops {
		if loop.Repo != nil && loop.PRNumber != nil {
			activePRs[prKey{loop.ProjectID, strings.ToLower(*loop.Repo), *loop.PRNumber}] = struct{}{}
		}
	}
	latest := map[prKey]storage.PullRequestSnapshotRecord{}
	for _, record := range records {
		key := prKey{record.ProjectID, strings.ToLower(record.Repo), record.PRNumber}
		if _, active := activePRs[key]; !active {
			continue
		}
		if _, exists := latest[key]; !exists {
			latest[key] = record
		}
	}
	for key, record := range latest {
		age, err := ageSeconds(now, record.CapturedAt)
		if err != nil {
			return fmt.Errorf("parse PR snapshot %s#%d: %w", record.Repo, record.PRNumber, err)
		}
		if time.Duration(age)*time.Second < c.staleAfter {
			continue
		}
		link, err := requiredLink(c.links.PullRequest(record.ProjectID, record.Repo, record.PRNumber))
		if err != nil {
			return fmt.Errorf("PR snapshot %s#%d: %w", record.Repo, record.PRNumber, err)
		}
		snapshot.Items = append(snapshot.Items, newItem(itemID(ReasonStalePRHead, key.project, key.repo, key.number), KindStuck, ReasonStalePRHead, record.ProjectID, "pull_request", fmt.Sprintf("%s#%d has stale head evidence", record.Repo, record.PRNumber), "No current-head snapshot has been captured within the threshold", link, age, 1, record.HeadSHA, record.CapturedAt))
	}
	return nil
}

func buildBacklog(now time.Time, loopsByID map[string]storage.LoopRecord) ([]StageBacklog, error) {
	type key struct{ project, stage string }
	values := map[key]StageBacklog{}
	for _, loop := range loopsByID {
		age, err := ageSeconds(now, loop.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse loop %s createdAt for backlog: %w", loop.ID, err)
		}
		k := key{loop.ProjectID, loop.Type}
		value := values[k]
		value.ProjectID, value.Stage = loop.ProjectID, loop.Type
		value.Depth++
		if age > value.OldestAgeSeconds {
			value.OldestAgeSeconds = age
		}
		values[k] = value
	}
	result := make([]StageBacklog, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result, nil
}

func newItem(id string, kind Kind, reason Reason, projectID, stage, title, detail, link string, age int64, blocked int, fingerprintParts ...string) Item {
	return Item{ID: id, Kind: kind, Reason: reason, ProjectID: projectID, Stage: stage, Title: title, Detail: detail, Link: link, AgeSeconds: age, BlockedWork: blocked, Fingerprint: fingerprint(fingerprintParts...)}
}

func itemID(reason Reason, parts ...any) string {
	values := []string{string(reason)}
	for _, part := range parts {
		values = append(values, fmt.Sprint(part))
	}
	return strings.Join(values, ":")
}

func fingerprint(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}

func metadataString(raw *string, key string) string {
	if raw == nil {
		return ""
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(*raw), &metadata) != nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}

func requiredLink(link string) (string, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return "", fmt.Errorf("action link is empty")
	}
	return link, nil
}

func queueTerminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}
