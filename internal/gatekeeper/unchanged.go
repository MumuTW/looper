package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

// maxSkipAge bounds how long a pull request may be skipped on an unchanged
// fingerprint. Branch protection rules and project policy are inputs to the gate
// that the list page cannot observe at all, so an unchanged pull request is
// re-evaluated periodically regardless. This is the backstop for every
// invalidator this fingerprint does not model.
const maxSkipAge = 30 * time.Minute

// sourceFingerprint summarises everything about a pull request that the shared
// discovery list page can observe. One list call already carries these fields for
// every open pull request, so comparing them costs nothing beyond the call the
// lane already makes.
//
// Deliberately excluded: check-run state. `gh pr list` does not carry it, and a
// check completing moves neither UpdatedAt nor any field here — which is why
// pull requests whose gate is waiting on a check are never skipped (see
// reportAwaitsCheckState).
func sourceFingerprint(pullRequest githubinfra.PullRequestSummary) string {
	labels := append([]string(nil), pullRequest.Labels...)
	sort.Strings(labels)
	return strings.Join([]string{
		pullRequest.HeadSHA,
		pullRequest.UpdatedAt,
		pullRequest.State,
		pullRequest.ReviewDecision,
		pullRequest.BaseRefName,
		fmt.Sprintf("%t", pullRequest.IsDraft),
		fmt.Sprintf("%t", pullRequest.HasConflicts),
		strings.Join(labels, ","),
	}, "\x1f")
}

// checkReasonCodes are the gate reasons that resolve on their own, without
// anything changing on the pull request. A pending check turning green or a
// missing check appearing changes the gate while every field the list page can
// see stays identical, so detecting it promptly requires re-evaluating.
//
// Failed and cancelled checks are deliberately absent, and measurement is why. On
// a live daemon 18 of 19 open pull requests carried required_check_failed at any
// moment, so treating it as volatile made almost nothing skippable and cost most
// of this optimisation's value. Unlike a pending check, a failed one does not fix
// itself: it changes on a re-run, and a re-run that accompanies a push moves the
// head SHA and is caught by the fingerprint anyway. The only missed case is a
// manual re-run with no push, which the maxSkipAge ceiling bounds — a cheap price
// on an observe-only report.
var checkReasonCodes = map[ReasonCode]struct{}{
	ReasonCheckMissing: {},
	ReasonCheckPending: {},
}

func reportAwaitsCheckState(report Report) bool {
	for _, reason := range report.Reasons {
		if _, ok := checkReasonCodes[reason.Code]; ok {
			return true
		}
	}
	return false
}

// latestGateReports returns the most recent gate report per pull request for one
// project, keyed by the report's entity id (`repo#number`).
//
// It is a single local query for the whole project rather than one per pull
// request: the point of this path is to spend microseconds of SQLite to avoid
// seconds of forge round trips, so it must not reintroduce per-PR work of its own.
func latestGateReports(ctx context.Context, repos *storage.Repositories, projectID string) (map[string]Report, error) {
	if repos == nil || repos.Events == nil {
		return nil, nil
	}
	records, err := repos.Events.ListByProjectAndEntityType(ctx, projectID, "pull_request")
	if err != nil {
		return nil, fmt.Errorf("list gate reports: %w", err)
	}
	// Records arrive oldest first, so a later gate report for the same entity
	// overwrites an earlier one and the map ends up holding the newest.
	reports := make(map[string]Report)
	for _, record := range records {
		if record.EventType != GateReportEventType || record.EntityID == nil {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(record.PayloadJSON), &report); err != nil {
			// A payload this runner cannot read is treated as no report at all, so the
			// pull request is evaluated rather than skipped on unreadable evidence.
			continue
		}
		reports[*record.EntityID] = report
	}
	return reports, nil
}

// skipUnchanged reports whether a pull request can reuse its previous gate report
// instead of being re-evaluated, and that report when so.
//
// Skipping is refused unless every one of these holds: a previous report exists,
// it recorded a fingerprint, the fingerprint still matches, the gate is not
// waiting on check state, and the report is younger than maxSkipAge.
func skipUnchanged(previous Report, hasPrevious bool, fingerprint string, now time.Time) (Report, bool) {
	if !hasPrevious || strings.TrimSpace(previous.SourceFingerprint) == "" {
		return Report{}, false
	}
	if previous.SourceFingerprint != fingerprint {
		return Report{}, false
	}
	if reportAwaitsCheckState(previous) {
		return Report{}, false
	}
	evaluatedAt, err := time.Parse(time.RFC3339Nano, previous.EvaluatedAt)
	if err != nil {
		return Report{}, false
	}
	if now.UTC().Sub(evaluatedAt.UTC()) >= maxSkipAge {
		return Report{}, false
	}
	return previous, true
}
