package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

// maxSkipAge bounds how long a pull request may be skipped on an unchanged
// fingerprint. Branch protection rules, project policy, and Reviewer
// convergence metadata are inputs to the gate that the list page cannot
// observe at all, so an unchanged pull request is re-evaluated periodically
// regardless. This is the backstop for every invalidator this fingerprint does
// not model.
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
//
// BaseSHA is included only when the diff budget is enabled. It does not move on
// an ordinary push, but when the base branch is force-pushed or otherwise
// rewritten GitHub recomputes changedFiles and deletions against a different
// merge base without moving the head or any other list-page field. Omitting it
// then would let a stale diff-budget verdict be reused for up to maxSkipAge
// after the observed counts crossed a configured limit. With both bounds at
// their default of zero the gate is unlimited, so a base advance changes
// nothing Gatekeeper enforces; including BaseSHA unconditionally would make
// every base-branch commit invalidate the cached report for every open pull
// request, paying for a full re-evaluation (and its forge round trips) to
// recompute a verdict that cannot have changed.
func sourceFingerprint(pullRequest githubinfra.PullRequestSummary, budgetEnabled bool) string {
	labels := append([]string(nil), pullRequest.Labels...)
	sort.Strings(labels)
	fields := []string{
		pullRequest.HeadSHA,
		pullRequest.UpdatedAt,
		pullRequest.State,
		pullRequest.ReviewDecision,
		pullRequest.BaseRefName,
		fmt.Sprintf("%t", pullRequest.IsDraft),
		fmt.Sprintf("%t", pullRequest.HasConflicts),
		strings.Join(labels, ","),
	}
	if budgetEnabled {
		fields = append(fields, pullRequest.BaseSHA)
	}
	return strings.Join(fields, "\x1f")
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

// reportAwaitsCurrentHeadReview reports whether a gate report is blocked on a
// current-head Reviewer review. It inspects both the reason and the persisted
// evidence: a provider block after the review check replaces the reason but
// leaves Evidence.CodexReview.CurrentHeadValid false, so checking the evidence
// alone would miss that case and checking the reason alone would miss a
// provider-blocked report. Both signals are authoritative.
var _ = reportAwaitsCurrentHeadReview

func reportAwaitsCurrentHeadReview(report Report) bool {
	if report.Evidence.CodexReview != nil && !report.Evidence.CodexReview.CurrentHeadValid {
		return true
	}
	for _, reason := range report.Reasons {
		if reason.Code == ReasonCodexReviewMissing {
			return true
		}
	}
	return false
}

func reportAwaitsCheckState(report Report) bool {
	for _, reason := range report.Reasons {
		if _, ok := checkReasonCodes[reason.Code]; ok {
			return true
		}
	}
	return false
}

// reportAwaitsConvergenceState reports whether the durable Reviewer state can
// change merge eligibility without changing the forge list fingerprint. A
// blocked convergence report must be re-read until its floor-qualified items
// are closed or deferred; otherwise unchanged discovery could retain a stale
// blocker after the Reviewer makes progress.
func reportAwaitsConvergenceState(report Report) bool {
	evidence := report.Evidence.ReviewerConvergence
	return evidence != nil && reviewerConvergenceBlocks(*evidence)
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
	records, err := repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, projectID, "pull_request", []string{GateReportEventType})
	if err != nil {
		return nil, fmt.Errorf("list gate reports: %w", err)
	}
	// One record per pull request already: SQLite selected the newest, so this
	// pass decodes exactly what the caller uses. The query is scoped to one
	// project, so the entity id alone is an unambiguous key here.
	reports := make(map[string]Report)
	for _, record := range records {
		if record.EntityID == nil {
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
// waiting on check state, the report is younger than maxSkipAge, and — at auto
// trust — the report does not carry a failed required check.
func skipUnchanged(previous Report, hasPrevious bool, fingerprint string, trust config.GatekeeperTrustLevel, now time.Time) (Report, bool) {
	if !hasPrevious || strings.TrimSpace(previous.SourceFingerprint) == "" {
		return Report{}, false
	}
	if previous.SourceFingerprint != fingerprint {
		return Report{}, false
	}
	if reportAwaitsCheckState(previous) {
		return Report{}, false
	}
	// At auto trust the merge route is applied only during an evaluation. A
	// failed check that is manually rerun to success turns the gate green
	// without moving any field the list page can observe, so reusing the
	// failed report would leave a now-eligible PR unqueued until maxSkipAge.
	// Re-evaluate instead so the route is published promptly.
	if trust == config.GatekeeperTrustAuto && reportHasFailedCheck(previous) {
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

func reportHasFailedCheck(report Report) bool {
	for _, reason := range report.Reasons {
		if reason.Code == ReasonCheckFailed {
			return true
		}
	}
	return false
}
