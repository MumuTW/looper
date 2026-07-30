package fixer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// CleanExaminationEventType records that discovery examined a pull request and
// found nothing to fix, together with what the pull request looked like at the
// time. It exists only to let the next tick skip an examination whose answer
// cannot have changed.
const CleanExaminationEventType = "fixer.discovery.examined_clean"

// maxCleanSkipAge bounds how long a clean examination may be reused. It is the
// backstop for anything the fingerprint cannot observe — a review thread resolved
// without touching the pull request, or a forge that does not move updatedAt on
// some event this lane cares about.
const maxCleanSkipAge = 30 * time.Minute

// cleanExamination is the persisted payload.
type cleanExamination struct {
	Repo        string `json:"repo"`
	PRNumber    int64  `json:"prNumber"`
	Fingerprint string `json:"fingerprint"`
	ExaminedAt  string `json:"examinedAt"`
}

// examinationFingerprint summarises everything about a pull request that the
// discovery list page already carries. Every event that can create fix work moves
// one of these: a new review comment or review moves UpdatedAt, a push moves
// HeadSHA, a hold moves Labels.
//
// Thread *resolution* may not move UpdatedAt, which is deliberately tolerated:
// resolution removes fix work rather than creating it, so reusing an earlier
// conclusion cannot cause work to be missed.
// An empty result means "not fingerprintable" and disables skipping for this pull
// request entirely. UpdatedAt is the only field that moves when a reviewer leaves a
// comment without pushing, so a provider that cannot supply it must never skip —
// otherwise the fingerprint would miss the very trigger this lane exists for.
func examinationFingerprint(pr PullRequestSummary) string {
	if strings.TrimSpace(pr.UpdatedAt) == "" {
		return ""
	}
	labels := append([]string(nil), pr.Labels...)
	sort.Strings(labels)
	return strings.Join([]string{
		pr.HeadSHA,
		pr.UpdatedAt,
		pr.State,
		fmt.Sprintf("%t", pr.IsDraft),
		strings.Join(labels, ","),
	}, "\x1f")
}

// latestCleanExaminations returns the most recent clean-examination fingerprint per
// pull request for one project, keyed by `repo#number`.
//
// One local query for the whole project: this path exists to spend microseconds of
// SQLite instead of seconds of forge round trips, so it must not become per-PR work.
func latestCleanExaminations(ctx context.Context, repos *storage.Repositories, projectID string) (map[string]cleanExamination, error) {
	if repos == nil || repos.Events == nil {
		return nil, nil
	}
	records, err := repos.Events.ListByProjectAndEntityType(ctx, projectID, cleanExaminationEntityType)
	if err != nil {
		return nil, fmt.Errorf("list fixer clean examinations: %w", err)
	}
	examinations := make(map[string]cleanExamination)
	for _, record := range records {
		if record.EventType != CleanExaminationEventType || record.EntityID == nil {
			continue
		}
		var examination cleanExamination
		if err := json.Unmarshal([]byte(record.PayloadJSON), &examination); err != nil {
			// Unreadable evidence means examine, never skip.
			continue
		}
		examinations[*record.EntityID] = examination
	}
	return examinations, nil
}

const cleanExaminationEntityType = "fixer_pull_request"

func cleanExaminationEntityID(repo string, prNumber int64) string {
	return fmt.Sprintf("%s#%d", repo, prNumber)
}

// skipCleanExamination reports whether a pull request's examination can be skipped
// because the last one found nothing to fix and nothing observable has changed.
//
// This only ever suppresses a "nothing to fix" conclusion. Discovery that found
// work is never skipped, so a fingerprint that misses something can at worst delay
// re-confirming a clean result — it can never stop real work from being enqueued.
func skipCleanExamination(previous cleanExamination, hasPrevious bool, fingerprint string, now time.Time) bool {
	if !hasPrevious || strings.TrimSpace(previous.Fingerprint) == "" {
		return false
	}
	if previous.Fingerprint != fingerprint {
		return false
	}
	examinedAt, err := time.Parse(time.RFC3339Nano, previous.ExaminedAt)
	if err != nil {
		return false
	}
	return now.UTC().Sub(examinedAt.UTC()) < maxCleanSkipAge
}

// recordCleanExamination persists that this pull request was examined and found
// clean at this fingerprint.
func (r *Runner) recordCleanExamination(ctx context.Context, projectID, repo string, prNumber int64, fingerprint string) error {
	if r.repos == nil || r.repos.Events == nil || strings.TrimSpace(fingerprint) == "" {
		return nil
	}
	entityType := cleanExaminationEntityType
	entityID := cleanExaminationEntityID(repo, prNumber)
	project := projectID
	payload := cleanExamination{
		Repo: repo, PRNumber: prNumber, Fingerprint: fingerprint,
		ExaminedAt: r.now().UTC().Format(time.RFC3339Nano),
	}
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: CleanExaminationEventType, ProjectID: &project,
		EntityType: &entityType, EntityID: &entityID,
		Payload: payload, CreatedAt: r.now(),
	}); err != nil {
		return fmt.Errorf("record fixer clean examination: %w", err)
	}
	return nil
}
