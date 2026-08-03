package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

// ReasonRouteRevoked marks a gate report for a pull request whose route was
// retired outside the bounded discovery page (a project that left discovery, or
// a routed pull request absent from the page whose routing inputs changed). An
// empty fingerprint makes the report immediately retryable, and the reason
// keeps the reconcile pass idempotent across ticks.
const ReasonRouteRevoked ReasonCode = "route_revoked"

const (
	outOfPageRouteReconcileInterval = 5 * time.Minute
	maxOutOfPageRouteChecksPerTick  = 20
)

// reportRouteEstablished is the durable-route predicate used by out-of-page
// reconciliation. Legacy reports predate RouteEstablished, so an eligible
// auto-trust report without a projection-failure marker is treated as the
// historical equivalent; current reports carry an explicit pointer and a
// blocked/retry report therefore cannot be mistaken for an active route.
func reportRouteEstablished(report Report) bool {
	if report.RouteEstablished != nil {
		return *report.RouteEstablished
	}
	return config.GatekeeperTrustLevel(strings.TrimSpace(report.Mode)) == config.GatekeeperTrustAuto && report.Eligible && !hasReasonCode(report.Reasons, ReasonRoutingProjectionFailed)
}

// reconcileRoutedReportsOutsideDiscoveryPage reconciles previously published
// routes whose pull requests are absent from the bounded discovery page.
// Discovery is not paginated, so a routed pull request beyond
// DefaultDiscoveryPullRequestLimit is never compared against the current trust
// or policy inputs; an observe demotion would leave its auto-merge label in
// place indefinitely. This pass retires routes whose routing inputs no longer
// permit them. It also preserves durable merge evidence for the Auditor: a
// routed pull request that merged through Mergify is gone from the open list,
// and the Auditor derives candidates only from MergeOutcomeEventType events, so
// without this pass a Mergify merge would silently leave the Auditor's
// candidate set empty. A routed pull request still open outside the page under
// inputs that still permit the route keeps it: the route is not stale, and the
// pull request re-enters the page and is re-evaluated when the repo shrinks.
func (r *Runner) reconcileRoutedReportsOutsideDiscoveryPage(ctx context.Context, input DiscoveryInput, pageEntityIDs map[string]struct{}, previousReports map[string]Report) error {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return nil
	}
	var errs []error
	checks := 0
	for entityID, previous := range previousReports {
		if _, inPage := pageEntityIDs[entityID]; inPage {
			continue
		}
		if !reportRouteEstablished(previous) {
			continue
		}
		if hasReasonCode(previous.Reasons, ReasonRouteRevoked) {
			continue
		}
		if checks >= maxOutOfPageRouteChecksPerTick {
			break
		}
		if !r.allowOutOfPageRouteCheck(input.ProjectID + "\x1f" + entityID) {
			continue
		}
		checks++
		if strings.TrimSpace(previous.Repo) != strings.TrimSpace(input.Repo) {
			if err := r.retireRoutingLabelsForReport(ctx, previous); err != nil {
				errs = append(errs, fmt.Errorf("revoke route for repository change %s: %w", entityID, err))
				continue
			}
			if err := r.markRouteRevoked(ctx, previous); err != nil {
				errs = append(errs, fmt.Errorf("mark repository-changed route %s revoked: %w", entityID, err))
			}
			continue
		}
		// One forge read distinguishes merged from closed from still-open. The
		// routed pull request is outside the page, so its live state cannot be
		// observed by the page evaluation; without this read a demoted project
		// could not tell a merged pull request from an open one.
		detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{
			Repo: previous.Repo, PRNumber: previous.PRNumber, CWD: r.projectCWD(ctx, input.ProjectID),
		})
		if err != nil {
			// Not knowing the state means not knowing whether the route is stale or
			// whether the pull request merged. Leave the route for the next reconcile
			// pass rather than guessing.
			errs = append(errs, fmt.Errorf("read out-of-page routed pull request %s: %w", entityID, err))
			continue
		}
		// GitHub's REST API reports merged pull requests with state "closed" and
		// a non-empty merged_at, so merged detection must read the merge
		// timestamp rather than the state string.
		mergedAt := strings.TrimSpace(detail.MergedAt)
		state := strings.ToUpper(strings.TrimSpace(detail.State))
		switch {
		case mergedAt != "":
			// A routed pull request that merged is durable evidence the Auditor
			// needs: record the merge outcome before retiring the route.
			if err := r.recordMergeEvidence(ctx, previous, mergedAt); err != nil {
				errs = append(errs, fmt.Errorf("record merge evidence %s: %w", entityID, err))
				continue
			}
			if err := r.clearRoutingLabelsForReport(ctx, previous); err != nil {
				errs = append(errs, fmt.Errorf("retire merged route %s: %w", entityID, err))
				continue
			}
		case state == "CLOSED":
			if err := r.clearRoutingLabelsForReport(ctx, previous); err != nil {
				errs = append(errs, fmt.Errorf("retire closed route %s: %w", entityID, err))
				continue
			}
		default:
			if liveHead := strings.TrimSpace(detail.HeadSHA); liveHead != "" && liveHead != strings.TrimSpace(previous.ObservedHeadSHA) && liveHead != strings.TrimSpace(previous.ExpectedHeadSHA) {
				if err := r.retireRoutingLabelsForReport(ctx, previous); err != nil {
					errs = append(errs, fmt.Errorf("retire head-drifted route %s: %w", entityID, err))
					continue
				}
				if err := r.markRouteRevoked(ctx, previous); err != nil {
					errs = append(errs, fmt.Errorf("mark head-drifted route %s revoked: %w", entityID, err))
				}
				continue
			}
			// Still open outside the page. Retire the route only when the current
			// routing inputs no longer permit it (observe demotion or policy
			// denial); a route under inputs that still permit it is not stale and
			// stays in place until the pull request re-enters the page.
			if !r.routingInputsStillPermit(previous) {
				if err := r.retireRoutingLabelsForReport(ctx, previous); err != nil {
					errs = append(errs, fmt.Errorf("retire out-of-page route %s: %w", entityID, err))
					continue
				}
			} else {
				continue
			}
		}
		// Mark revoked only after the retirement succeeded: marking first would
		// make the next reconcile pass skip the entity (ReasonRouteRevoked) and
		// never retry a failed label removal.
		if err := r.markRouteRevoked(ctx, previous); err != nil {
			errs = append(errs, fmt.Errorf("mark out-of-page route %s revoked: %w", entityID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) allowOutOfPageRouteCheck(key string) bool {
	if r == nil {
		return false
	}
	now := r.now().UTC()
	r.outOfPageRouteMu.Lock()
	defer r.outOfPageRouteMu.Unlock()
	if r.outOfPageRouteChecks == nil {
		r.outOfPageRouteChecks = make(map[string]time.Time)
	}
	if last, ok := r.outOfPageRouteChecks[key]; ok && now.Before(last.Add(outOfPageRouteReconcileInterval)) {
		return false
	}
	r.outOfPageRouteChecks[key] = now
	return true
}

// routingInputsStillPermit reports whether the routing inputs that published a
// report (trust level and target policy) still permit the route it carried.
// This is the local check that replaces the page evaluation for a pull request
// outside the bounded discovery page: it costs no forge round trip.
func (r *Runner) routingInputsStillPermit(report Report) bool {
	if r.trustFor(report.ProjectID) != config.GatekeeperTrustAuto {
		return false
	}
	base := strings.TrimSpace(report.Evidence.BaseRefName)
	if base != "" && !r.policyPermitsTarget(report.ProjectID, report.Repo, base) {
		return false
	}
	return true
}

// retireRoutingLabelsForReport retires both routing labels for a previously
// published report without requiring a pull-request head authority: removing a
// stale route can never authorize a merge, and observe demotions must be able
// to retire labels even when the provider no longer returns a head. A routed
// pull request already inside the Mergify queue stays queued unless it carries
// a durable veto, so the retirement leaves needs-human-review in place rather
// than removing both labels.
func (r *Runner) retireRoutingLabelsForReport(ctx context.Context, report Report) error {
	// Reuse the needs-human-review projection: it removes the auto-merge entry
	// trigger and leaves the durable queue veto in place, so a pull request
	// already inside the Mergify queue is dequeued rather than left satisfying
	// the queue rule on stale authority. Label mutations need no pull-request
	// head authority: removing a stale route can never authorize a merge.
	return r.applyRoutingLabelPlan(ctx, report, routingLabelPlan{needsHumanReview: true})
}

func (r *Runner) clearRoutingLabelsForReport(ctx context.Context, report Report) error {
	input := githubinfra.PullRequestLabelsInput{Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID)}
	// Terminal PRs cannot be queued, but clear both labels so a closed route
	// does not trigger external human-review automation. Keep auto-merge last
	// for deterministic audit logs shared with the historical cleanup order.
	if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.NeedsHumanReview}}); err != nil {
		return fmt.Errorf("remove stale %s label: %w", labels.NeedsHumanReview, err)
	}
	if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.AutoMerge}}); err != nil {
		return fmt.Errorf("remove stale %s label: %w", labels.AutoMerge, err)
	}
	return nil
}

// recordMergeEvidence preserves durable merge evidence for the Auditor when a
// previously routed pull request merged through the Mergify route. The Auditor
// derives candidates exclusively from MergeOutcomeEventType events, so a merge
// that leaves no event is invisible to post-merge failure attribution.
//
// The emission is idempotent per entity and head: the reconcile pass can re-read
// the same merged pull request across ticks when a label retirement fails in
// between, and a duplicate event would project duplicate Auditor candidates.
func (r *Runner) recordMergeEvidence(ctx context.Context, report Report, mergedAt string) error {
	projectID := report.ProjectID
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	headSHA := strings.TrimSpace(report.ObservedHeadSHA)
	if headSHA == "" {
		headSHA = strings.TrimSpace(report.ExpectedHeadSHA)
	}
	if headSHA == "" {
		return fmt.Errorf("merge evidence for %s has no head SHA", entityID)
	}
	if already, err := mergeOutcomeRecorded(ctx, r.repos, entityType, entityID, headSHA); err != nil {
		return err
	} else if already {
		return nil
	}
	mergedAt = strings.TrimSpace(mergedAt)
	if mergedAt == "" {
		mergedAt = r.now().UTC().Format(time.RFC3339Nano)
	}
	return eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: MergeOutcomeEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: MergeOutcome{
			Version: 1, ProjectID: report.ProjectID, Repo: report.Repo, PRNumber: report.PRNumber,
			HeadSHA: headSHA, Merged: true, AttemptedAt: mergedAt,
		},
		CreatedAt: r.now(),
	})
}

// mergeOutcomeRecorded reports whether a successful merge outcome for the given
// entity and head already exists in the event log.
func mergeOutcomeRecorded(ctx context.Context, repos *storage.Repositories, entityType, entityID, headSHA string) (bool, error) {
	if repos == nil || repos.Events == nil {
		return false, nil
	}
	records, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		return false, fmt.Errorf("list merge outcome events for %s: %w", entityID, err)
	}
	for _, record := range records {
		if record.EventType != MergeOutcomeEventType {
			continue
		}
		var outcome MergeOutcome
		if err := json.Unmarshal([]byte(record.PayloadJSON), &outcome); err != nil {
			continue
		}
		if outcome.Merged && strings.TrimSpace(outcome.HeadSHA) == headSHA {
			return true, nil
		}
	}
	return false, nil
}

// markRouteRevoked appends a terminal gate report with an empty discovery
// fingerprint and ReasonRouteRevoked so the reconcile pass is idempotent: a
// pull request outside the page is never re-read for the same stale route, and
// if it later re-enters the page the empty fingerprint forces a fresh
// evaluation instead of reusing the retired report.
func (r *Runner) markRouteRevoked(ctx context.Context, report Report) error {
	revoked := report
	revoked.SourceFingerprint = ""
	revoked.Reasons = append([]Reason(nil), report.Reasons...)
	revoked.Reasons = append(revoked.Reasons, Reason{Code: ReasonRouteRevoked})
	sortReasons(revoked.Reasons)
	revoked.Eligible = false
	revoked.RouteEstablished = boolPointer(false)
	revoked.Status = StatusBlocked
	revoked.EvaluatedAt = r.now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	entityType, entityID := "pull_request", fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	// Append strictly after the evaluation's final report (which persist stamps
	// at +1ms) so latestGateReports, which keeps the newest record per entity,
	// resolves to the revoked marker rather than the published report.
	return r.appendGateReportAt(ctx, revoked, entityType, entityID, r.now().Add(2*time.Millisecond))
}

// RevokeProjectRoutes retires every previously published routing label for a
// project. The scheduler calls it when a project leaves discovery (archived or
// absent from the captured catalog): without it, no later evaluation would
// remove a persistent auto-merge label, so Mergify could merge the pull request
// after the operator disabled all Looper processing for the project.
func (r *Runner) RevokeProjectRoutes(ctx context.Context, projectID string) error {
	if r == nil || r.github == nil || r.repos == nil || r.repos.Events == nil {
		return nil
	}
	reports, err := latestGateReports(ctx, r.repos, projectID)
	if err != nil {
		return fmt.Errorf("list gate reports for project route revocation: %w", err)
	}
	var errs []error
	for entityID, report := range reports {
		if !previousPublished(report) {
			continue
		}
		if hasReasonCode(report.Reasons, ReasonRouteRevoked) {
			continue
		}
		if reportRouteEstablished(report) {
			if err := r.retireRoutingLabelsForReport(ctx, report); err != nil {
				errs = append(errs, fmt.Errorf("revoke route %s: %w", entityID, err))
				continue
			}
		}
		if err := r.applyVerdict(ctx, verdictActionRetire, report); err != nil {
			errs = append(errs, fmt.Errorf("retire verdict %s: %w", entityID, err))
			continue
		}
		if err := r.markRouteRevoked(ctx, report); err != nil {
			errs = append(errs, fmt.Errorf("mark route %s revoked: %w", entityID, err))
		}
	}
	return errors.Join(errs...)
}
