package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

// gatekeeperStatusRevokePendingError marks a needs-human-review projection
// whose label mutations succeeded while the commit-status revocation failed.
// The route fact is established — the durable veto is on the pull request and
// the queue trigger is gone — so the retry marker must retain it and the next
// evaluation retries only the status write.
type gatekeeperStatusRevokePendingError struct{ cause error }

func (e *gatekeeperStatusRevokePendingError) Error() string {
	return "revoke successful Gatekeeper status before queue veto: " + e.cause.Error()
}

func (e *gatekeeperStatusRevokePendingError) Unwrap() error { return e.cause }

// protectedPathTouchedReason is owned by the independent protected-path gate
// (#458), whose Go constant is not yet in this package. The routing layer
// deliberately matches its stable wire value so #460 can merge independently of
// #458 without creating a second reason-code authority. The diff-budget reason
// has no such constraint: ReasonDiffBudgetExceeded already lives in this
// package, so reportNeedsHumanReview uses it directly.
const protectedPathTouchedReason = "protected_path_touched"

type routingLabelPlan struct {
	autoMerge        bool
	needsHumanReview bool
	// revokeGateStatus is set when an already-established auto route becomes
	// blocked. The status is a second queue admission signal, so revoke it
	// before attempting the veto label: if that label write fails, Mergify must
	// still see a non-success status and refuse the stale route.
	revokeGateStatus bool
}

// reconcileRoutingLabels projects the durable Gate report onto the two labels
// consumed by the Mergify queue. A label is an authority-bearing projection, so
// an automatic route is preceded by a fresh pull-request state read and a final
// head read. Any state drift causes the projection to be retried on the next
// evaluation rather than routing a different commit or policy state.
func (r *Runner) reconcileRoutingLabels(ctx context.Context, report Report, previous *Report) error {
	trust := r.trustFor(report.ProjectID)
	if trust == config.GatekeeperTrustObserve && !reportIsTerminal(report) && (previous == nil || !previousPublished(*previous)) {
		return nil
	}

	plan := routingLabelPlan{}
	if reportIsTerminal(report) {
		// Terminal pull requests are removal-only regardless of the previous
		// route; retaining a human-review label can trigger unrelated automation.
	} else if trust == config.GatekeeperTrustAuto && report.Eligible {
		plan.autoMerge = true
	} else if trust != config.GatekeeperTrustObserve && reportNeedsHumanReview(previous, report) {
		plan.needsHumanReview = true
	}
	// A route accepted under auto trust remains a live Mergify queue entry until
	// the queue observes a veto. Removing auto trust (to advise or observe) must
	// therefore retain needs-human-review for an open pull request; removing both
	// labels would withdraw Looper's authority while an already-accepted queue
	// item can still merge. Terminal evaluations are removal-only because a closed
	// pull request cannot be queued and must not retain an external human-review
	// signal.
	if trust != config.GatekeeperTrustAuto && previous != nil && reportRouteEstablished(*previous) && !reportIsTerminal(report) {
		plan.needsHumanReview = true
		plan.revokeGateStatus = true
	}
	if trust == config.GatekeeperTrustAuto && previous != nil && reportRouteEstablished(*previous) && plan.needsHumanReview && !reportIsTerminal(report) {
		plan.revokeGateStatus = true
	}
	// Removal-only projections are fail-safe and do not need an authority read:
	// deleting a stale route can never authorize a merge. This also lets observe
	// demotion retire labels even when the provider no longer returns a head.
	if !plan.autoMerge && !plan.needsHumanReview {
		return r.applyRoutingLabelPlan(ctx, report, plan)
	}
	// A human-review veto is a fail-safe projection: it can only block an
	// already accepted queue entry and never authorizes a merge. Do not require
	// another provider read for this path, especially when the report itself
	// records a head/provider failure and therefore cannot carry complete state
	// evidence for the revalidation comparison.
	if !plan.autoMerge && plan.needsHumanReview {
		return r.applyRoutingLabelPlan(ctx, report, plan)
	}

	expectedHead := strings.TrimSpace(report.Evidence.FinalObservedHeadSHA)
	if expectedHead == "" {
		expectedHead = strings.TrimSpace(report.ObservedHeadSHA)
	}
	// A provider failure may leave no observed head. Removing route labels is
	// still safe and prevents a stale auto-merge label from authorizing a queue;
	// adding either route label is never allowed without a head authority.
	if expectedHead == "" {
		if plan.autoMerge || plan.needsHumanReview {
			return fmt.Errorf("cannot add routing labels without an observed pull-request head")
		}
		return r.applyRoutingLabelPlan(ctx, report, plan)
	}
	if plan.autoMerge {
		if err := r.github.ValidateMergifyRouting(ctx, githubinfra.ValidateMergifyRoutingInput{
			Repo: reportRepositoryTarget(report), BaseRefName: strings.TrimSpace(report.Evidence.BaseRefName), CWD: r.projectCWD(ctx, report.ProjectID),
		}); err != nil {
			// A repository contract failure must retire an already-published
			// route before returning. Leaving auto-merge in place while the
			// contract is invalid is the fail-open state this validation is
			// intended to prevent.
			return fmt.Errorf("validate Mergify routing contract: %w", r.retireRoutingLabels(ctx, report, previous, err))
		}
	}
	// Read the PR state after the configuration check so this authority read is
	// immediately adjacent to the final head check and label mutation. A
	// revalidation failure means the evidence the existing route was published
	// on is no longer current: a hold or policy change can happen without moving
	// the head, and leaving auto-merge in place would let Mergify keep the pull
	// request queued on stale authority. Fail closed by retiring both routing
	// labels before returning; removal is fail-safe and the next evaluation
	// re-applies the route if the pull request is eligible again.
	if err := r.revalidateRoutingState(ctx, report, expectedHead); err != nil {
		return r.retireRoutingLabels(ctx, report, previous, err)
	}

	// Review threads are Gatekeeper-owned policy input, not part of the
	// Mergify queue contract. Re-read them immediately before projecting
	// auto-merge so a thread opened after EvaluatePullRequest's initial
	// snapshot cannot slip through the label boundary. An unresolved thread is
	// an explicit Gatekeeper blocker, so the previously published route must be
	// retired rather than left authorizing a queue.
	if plan.autoMerge {
		if err := r.revalidateUnresolvedReviewThreads(ctx, report); err != nil {
			return r.retireRoutingLabels(ctx, report, previous, err)
		}
	}
	// Reviewer convergence is local durable policy input, not part of the forge
	// state read above. Re-read it at the same authority boundary so a newly
	// persisted floor-qualified item cannot arrive between evaluation and an
	// auto-merge projection unnoticed.
	if report.Evidence.ReviewerConvergence != nil || plan.autoMerge {
		if err := r.revalidateReviewerConvergence(ctx, report, expectedHead); err != nil {
			// A convergence change invalidates the route authority. Retire both
			// labels before retrying so a previously published auto-merge label
			// cannot keep queueing a pull request while the local evidence is stale.
			return r.retireRoutingLabels(ctx, report, previous, err)
		}
	}
	if plan.autoMerge {
		if err := r.revalidateCodexReview(ctx, report, expectedHead); err != nil {
			return r.retireRoutingLabels(ctx, report, previous, err)
		}
	}
	// The final head (and, when applicable, merge-base) read must happen after
	// thread revalidation. A push during that provider call otherwise leaves the
	// label write bound to a head that Gatekeeper never confirmed at the end of
	// its authority sequence. Diff-budget counts are also only valid for the
	// persisted base SHA, so a changed base fails closed before projection.
	var currentHead, currentBase string
	viewInput := githubinfra.ViewPullRequestInput{
		Repo: reportRepositoryTarget(report), PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
	}
	baseSensitive := report.Evidence.DiffBudget != nil || r.requiredReviewChangedLinesFor(report.ProjectID) > 0 || len(r.protectedPaths(report.ProjectID)) > 0
	if baseSensitive {
		expectedBase := strings.TrimSpace(report.Evidence.FinalObservedBaseSHA)
		if expectedBase == "" {
			expectedBase = strings.TrimSpace(report.Evidence.BaseSHA)
		}
		if report.Evidence.DiffBudget != nil && strings.TrimSpace(report.Evidence.DiffBudget.BaseSHA) != "" {
			expectedBase = strings.TrimSpace(report.Evidence.DiffBudget.BaseSHA)
		}
		if expectedBase != "" {
			var err error
			currentHead, currentBase, err = r.github.GetPullRequestHeadAndBaseSHA(ctx, viewInput)
			if err != nil {
				return r.retireRoutingLabels(ctx, report, previous, fmt.Errorf("revalidate head and policy base before routing labels: %w", err))
			}
			if strings.TrimSpace(currentBase) != expectedBase {
				return r.retireRoutingLabels(ctx, report, previous, fmt.Errorf("skip routing labels because policy base moved from %s to %s", expectedBase, strings.TrimSpace(currentBase)))
			}
		} else {
			var err error
			currentHead, err = r.github.GetPullRequestHeadSHA(ctx, viewInput)
			if err != nil {
				return r.retireRoutingLabels(ctx, report, previous, fmt.Errorf("revalidate head before routing labels: %w", err))
			}
		}
	} else {
		var err error
		currentHead, err = r.github.GetPullRequestHeadSHA(ctx, viewInput)
		if err != nil {
			return r.retireRoutingLabels(ctx, report, previous, fmt.Errorf("revalidate head before routing labels: %w", err))
		}
	}
	if strings.TrimSpace(currentHead) != expectedHead {
		return r.retireRoutingLabels(ctx, report, previous, fmt.Errorf("skip routing labels because pull request head moved from %s to %s", expectedHead, strings.TrimSpace(currentHead)))
	}
	// Mergify consumes both the label and this commit status. Publish the
	// success status only after every final authority read above has passed, so
	// it is bound to the exact head that the following label mutation will
	// route. A status publication failure must fail closed: without the
	// current-head status, the label alone is not an authorized route.
	if plan.autoMerge {
		if err := r.github.SetCommitStatus(ctx, githubinfra.CommitStatusInput{
			Repo:        reportRepositoryTarget(report),
			SHA:         strings.TrimSpace(currentHead),
			Context:     RequiredStatusContext,
			State:       "success",
			Description: "Current-head Gatekeeper route passed",
			CWD:         r.projectCWD(ctx, report.ProjectID),
		}); err != nil {
			return r.retireRoutingLabels(ctx, report, previous, fmt.Errorf("publish Gatekeeper status before routing labels: %w", err))
		}
	}
	return r.applyRoutingLabelPlan(ctx, report, plan)
}

func (r *Runner) revalidateCodexReview(ctx context.Context, report Report, expectedHead string) error {
	current, err := latestCodexReviewForHead(ctx, r.repos, report.ProjectID, report.Repo, report.PRNumber, expectedHead)
	if err != nil {
		return fmt.Errorf("revalidate Codex review before routing labels: %w", err)
	}
	previous := report.Evidence.CodexReview
	if previous == nil {
		if current.CurrentHeadValid && strings.TrimSpace(report.EvaluatedAt) != "" && strings.TrimSpace(current.RecordedAt) > strings.TrimSpace(report.EvaluatedAt) {
			return fmt.Errorf("skip routing labels because Codex review evidence appeared after evaluation")
		}
		return nil
	}
	if current.CurrentHeadValid != previous.CurrentHeadValid ||
		!strings.EqualFold(current.ReviewedHeadSHA, previous.ReviewedHeadSHA) ||
		!strings.EqualFold(current.Event, previous.Event) ||
		!strings.EqualFold(current.Outcome, previous.Outcome) ||
		strings.TrimSpace(current.RecordedAt) != strings.TrimSpace(previous.RecordedAt) {
		return fmt.Errorf("skip routing labels because Codex review evidence advanced")
	}
	return nil
}

func (r *Runner) revalidateReviewerConvergence(ctx context.Context, report Report, expectedHead string) error {
	current, hasCurrent, err := latestReviewerConvergence(ctx, r.repos, report.ProjectID, report.Repo, report.PRNumber, expectedHead)
	if err != nil {
		return fmt.Errorf("revalidate Reviewer convergence before routing labels: %w", err)
	}
	previous := report.Evidence.ReviewerConvergence
	if previous == nil {
		if hasCurrent && reviewerConvergenceBlocks(current) {
			return fmt.Errorf("skip routing labels because Reviewer convergence now blocks: %s", reviewerConvergenceReasonSubject(current))
		}
		return nil
	}
	if !hasCurrent {
		return fmt.Errorf("skip routing labels because Reviewer convergence evidence disappeared")
	}
	if convergenceRevision(current) != convergenceRevision(*previous) || current.HeadStale != previous.HeadStale {
		return fmt.Errorf("skip routing labels because Reviewer convergence advanced")
	}
	return nil
}

func (r *Runner) revalidateUnresolvedReviewThreads(ctx context.Context, report Report) error {
	threads, err := r.github.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{
		Repo: reportRepositoryTarget(report), PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID), Limit: 1000,
	})
	if err != nil {
		return fmt.Errorf("revalidate review threads before routing labels: %w", err)
	}
	if len(threads) == 1000 {
		return fmt.Errorf("revalidate review threads before routing labels: result is truncated")
	}
	for _, thread := range threads {
		if !thread.IsResolved {
			return fmt.Errorf("skip routing labels because review thread %s is unresolved", strings.TrimSpace(thread.ID))
		}
	}
	return nil
}

func (r *Runner) revalidateRoutingState(ctx context.Context, report Report, expectedHead string) error {
	input := githubinfra.ViewPullRequestInput{
		Repo: reportRepositoryTarget(report), PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
	}
	detail, err := r.github.ViewPullRequestForGatekeeper(ctx, input)
	if err != nil {
		return fmt.Errorf("revalidate pull-request state before routing labels: %w", err)
	}
	if detail.Number != 0 && detail.Number != report.PRNumber {
		return fmt.Errorf("skip routing labels because pull request identity changed from %d to %d", report.PRNumber, detail.Number)
	}
	if strings.TrimSpace(detail.HeadSHA) != expectedHead {
		return fmt.Errorf("skip routing labels because pull request head moved from %s to %s", expectedHead, strings.TrimSpace(detail.HeadSHA))
	}
	// Reports produced by EvaluatePullRequest always carry PullRequestState.
	// A report without it cannot be bound to a verified pull-request state, and
	// this routing path only runs for plans that would add a routing label
	// (auto-merge or needs-human-review). Fail closed on the legacy fallback:
	// an add plan must not project from evidence that never observed the
	// pull-request state, because that is exactly the path where the authority
	// read matters most.
	state := strings.TrimSpace(report.Evidence.PullRequestState)
	if state == "" {
		return fmt.Errorf("skip routing labels because the report carries no pull-request state evidence")
	}
	if !strings.EqualFold(state, strings.TrimSpace(detail.State)) {
		return fmt.Errorf("skip routing labels because pull request state changed from %s to %s", state, strings.TrimSpace(detail.State))
	}
	if report.Evidence.Draft != detail.IsDraft {
		return fmt.Errorf("skip routing labels because draft state changed")
	}
	if strings.TrimSpace(report.Evidence.BaseRefName) != strings.TrimSpace(detail.BaseRefName) {
		return fmt.Errorf("skip routing labels because base branch changed from %s to %s", report.Evidence.BaseRefName, strings.TrimSpace(detail.BaseRefName))
	}
	if !strings.EqualFold(strings.TrimSpace(report.Evidence.ReviewDecision), strings.TrimSpace(detail.ReviewDecision)) {
		return fmt.Errorf("skip routing labels because review decision changed from %s to %s", report.Evidence.ReviewDecision, strings.TrimSpace(detail.ReviewDecision))
	}
	recordedHold := len(report.Evidence.HoldLabels) > 0
	holdLabel := r.projectLabelNamespace(ctx, report.ProjectID).HoldGlobal()
	currentHold := labels.Has(detail.Labels, holdLabel)
	if recordedHold != currentHold {
		return fmt.Errorf("skip routing labels because hold state changed")
	}
	// A do-not-merge veto applied between evaluation and projection must also
	// fail closed: the veto is human authority that overrides auto trust, and
	// the label state is not part of the final head read.
	if report.Evidence.DoNotMergeVeto != labels.Has(detail.Labels, labels.DoNotMerge) {
		return fmt.Errorf("skip routing labels because do-not-merge veto state changed")
	}
	if strings.TrimSpace(report.Evidence.BaseRefName) != "" {
		currentPolicy := r.policyPermitsTarget(report.ProjectID, report.Repo, report.Evidence.BaseRefName)
		if currentPolicy != report.Evidence.ProjectPolicyPermitsTarget {
			return fmt.Errorf("skip routing labels because project policy changed")
		}
	}
	return nil
}

// retireRoutingLabels removes the queue trigger before returning err. When the
// PR was already routed, it leaves needs-human-review as a durable queue veto;
// a Mergify queue entry must not survive a failed revalidation with no veto.
// A first projection with no prior route remains removal-only.
func (r *Runner) retireRoutingLabels(ctx context.Context, report Report, previous *Report, err error) error {
	// A revalidation failure happens after an eligible report has crossed the
	// queue boundary. Removing auto-merge alone does not dequeue an entry that
	// Mergify already accepted, so keep the durable human-review veto while the
	// route is still potentially open. Terminal/out-of-page cleanup calls the
	// removal-only helper instead.
	plan := routingLabelPlan{}
	if r.trustFor(report.ProjectID) == config.GatekeeperTrustAuto && previous != nil && reportRouteEstablished(*previous) {
		plan.needsHumanReview = true
		plan.revokeGateStatus = true
	}
	cleanupErr := r.applyRoutingLabelPlan(ctx, report, plan)
	if cleanupErr != nil {
		var revokePending *gatekeeperStatusRevokePendingError
		if errors.As(cleanupErr, &revokePending) {
			// The cleanup installed the durable veto and only the commit-status
			// write is outstanding. Preserve the typed state through the
			// wrapper so persist retains the established-route fact and the
			// next evaluation retries the projection instead of deleting the
			// veto this cleanup just installed.
			return &gatekeeperStatusRevokePendingError{cause: fmt.Errorf("%w (remove stale routing labels: %v)", err, revokePending.cause)}
		}
		return fmt.Errorf("%w (remove stale routing labels: %v)", err, cleanupErr)
	}
	return err
}

func (r *Runner) applyRoutingLabelPlan(ctx context.Context, report Report, plan routingLabelPlan) error {
	input := githubinfra.PullRequestLabelsInput{
		Repo: reportRepositoryTarget(report), PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
	}
	switch {
	case plan.autoMerge:
		// Add the queue route before removing the human-escalation label. If the
		// second mutation fails, the existing veto keeps an already-queued PR
		// blocked; removing the veto first would briefly (or permanently) reopen a
		// stale queue entry without a durable route report.
		input.Labels = []string{labels.AutoMerge}
		if err := r.github.AddPullRequestLabels(ctx, input); err != nil {
			return fmt.Errorf("add %s label: %w", labels.AutoMerge, err)
		}
		if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.NeedsHumanReview}}); err != nil {
			return fmt.Errorf("remove stale %s label: %w", labels.NeedsHumanReview, err)
		}
	case plan.needsHumanReview:
		// Attempt the status revocation first, but never let its failure block
		// the label retirement below: during a status-API outage the route must
		// still lose its queue trigger and gain the durable veto, or the
		// supposedly retired pull request keeps both its route and the stale
		// success until the API recovers. The revocation error is returned after
		// the label mutations so the status is retried on the next pass.
		var revokeErr error
		if plan.revokeGateStatus {
			revokeErr = r.revokeGatekeeperStatus(ctx, report)
		}
		// Add the durable queue veto before removing the trigger. If the second
		// mutation fails, the accepted queue entry remains blocked rather than
		// becoming an unvetoed stale authorization between mutations.
		input.Labels = []string{labels.NeedsHumanReview}
		if err := r.github.AddPullRequestLabels(ctx, input); err != nil {
			// Best-effort trigger removal still closes the stale-route path when
			// the veto label write fails (including another PR sharing this head).
			removeErr := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.AutoMerge}})
			if removeErr != nil {
				combined := fmt.Errorf("add %s label: %w (remove stale %s label: %v)", labels.NeedsHumanReview, err, labels.AutoMerge, removeErr)
				if revokeErr != nil {
					// The projection is still pending: keep the typed state so
					// persist retains the established route and the whole
					// projection (veto, trigger removal, status) is retried.
					return &gatekeeperStatusRevokePendingError{cause: combined}
				}
				return combined
			}
			if revokeErr != nil {
				return &gatekeeperStatusRevokePendingError{cause: fmt.Errorf("add %s label: %w", labels.NeedsHumanReview, err)}
			}
			return fmt.Errorf("add %s label: %w", labels.NeedsHumanReview, err)
		}
		if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.AutoMerge}}); err != nil {
			if revokeErr != nil {
				return &gatekeeperStatusRevokePendingError{cause: fmt.Errorf("revoke successful Gatekeeper status before queue veto: %v (remove stale %s label: %w)", revokeErr, labels.AutoMerge, err)}
			}
			return fmt.Errorf("remove stale %s label: %w", labels.AutoMerge, err)
		}
		if revokeErr != nil {
			// The durable veto is on the pull request and the queue trigger is
			// gone; only the commit-status write is outstanding. The typed error
			// lets persist retain the established-route fact so the retry marker
			// cannot read as "no route" and delete the veto it just installed.
			return &gatekeeperStatusRevokePendingError{cause: revokeErr}
		}
	default:
		// Mechanical blockers and observe demotions intentionally leave no
		// routing signal. The queue cannot act, while a later clean evaluation
		// can re-apply the opt-in label.
		if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.AutoMerge}}); err != nil {
			return fmt.Errorf("remove stale %s label: %w", labels.AutoMerge, err)
		}
		if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.NeedsHumanReview}}); err != nil {
			return fmt.Errorf("remove stale %s label: %w", labels.NeedsHumanReview, err)
		}
	}
	return nil
}

func (r *Runner) revokeGatekeeperStatus(ctx context.Context, report Report) error {
	sha := strings.TrimSpace(report.Evidence.FinalObservedHeadSHA)
	if sha == "" {
		sha = strings.TrimSpace(report.ObservedHeadSHA)
	}
	if sha == "" {
		return fmt.Errorf("report carries no observed pull-request head")
	}
	return r.github.SetCommitStatus(ctx, githubinfra.CommitStatusInput{
		Repo:        reportRepositoryTarget(report),
		SHA:         sha,
		Context:     RequiredStatusContext,
		State:       "failure",
		Description: "Gatekeeper route blocked; human review required",
		CWD:         r.projectCWD(ctx, report.ProjectID),
	})
}

func reportNeedsHumanReview(previous *Report, report Report) bool {
	hasRepeatedReviewChanges := previous != nil && hasReasonCode(previous.Reasons, ReasonReviewChangesRequested) && hasReasonCode(report.Reasons, ReasonReviewChangesRequested)
	if previous != nil && reportRouteEstablished(*previous) {
		// Every newly ineligible report needs the explicit queue veto. Removing
		// only auto-merge cannot dequeue an entry Mergify already accepted.
		if !report.Eligible {
			return true
		}
		// These blockers can arrive after Mergify accepted the queue entry. Keep
		// the explicit veto until a fresh eligible evaluation replaces it; merely
		// removing auto-merge does not dequeue an already accepted PR.
		for _, reason := range report.Reasons {
			switch reason.Code {
			case ReasonHold, ReasonUnresolvedReviewThread, ReasonReviewerConvergence, ReasonHeadStale:
				return true
			}
		}
	}
	for _, reason := range report.Reasons {
		switch string(reason.Code) {
		case protectedPathTouchedReason, string(ReasonDiffBudgetExceeded):
			return true
		}
	}
	return hasRepeatedReviewChanges
}

func reportIsTerminal(report Report) bool {
	if hasReasonCode(report.Reasons, ReasonPullRequestNotOpen) {
		return true
	}
	state := strings.ToUpper(strings.TrimSpace(report.Evidence.PullRequestState))
	return state != "" && state != "OPEN"
}

func hasReasonCode(reasons []Reason, want ReasonCode) bool {
	for _, reason := range reasons {
		if reason.Code == want {
			return true
		}
	}
	return false
}
