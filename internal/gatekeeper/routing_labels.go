package gatekeeper

import (
	"context"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

// These reason values are owned by the independent policy-gate issues. The
// routing layer deliberately matches their stable wire values instead of
// re-declaring their Go constants, so #460 can merge independently of #458 and
// #459 without creating a second reason-code authority.
const (
	protectedPathTouchedReason = "protected_path_touched"
	diffBudgetExceededReason   = "diff_budget_exceeded"
)

type routingLabelPlan struct {
	autoMerge        bool
	needsHumanReview bool
}

// reconcileRoutingLabels projects the durable Gate report onto the two labels
// consumed by the Mergify queue. A label is an authority-bearing projection, so
// an automatic route is preceded by a fresh pull-request state read and a final
// head read. Any state drift causes the projection to be retried on the next
// evaluation rather than routing a different commit or policy state.
func (r *Runner) reconcileRoutingLabels(ctx context.Context, report Report, previous *Report) error {
	trust := r.trustFor(report.ProjectID)
	if trust == config.GatekeeperTrustObserve && (previous == nil || !previousPublished(*previous)) {
		return nil
	}

	plan := routingLabelPlan{}
	if trust == config.GatekeeperTrustAuto && report.Eligible {
		plan.autoMerge = true
	} else if trust != config.GatekeeperTrustObserve && reportNeedsHumanReview(previous, report) {
		plan.needsHumanReview = true
	}
	// Removal-only projections are fail-safe and do not need an authority read:
	// deleting a stale route can never authorize a merge. This also lets observe
	// demotion retire labels even when the provider no longer returns a head.
	if !plan.autoMerge && !plan.needsHumanReview {
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
			Repo: report.Repo, CWD: r.projectCWD(ctx, report.ProjectID),
		}); err != nil {
			// A repository contract failure must retire an already-published
			// route before returning. Leaving auto-merge in place while the
			// contract is invalid is the fail-open state this validation is
			// intended to prevent.
			cleanupErr := r.applyRoutingLabelPlan(ctx, report, routingLabelPlan{})
			if cleanupErr != nil {
				return fmt.Errorf("validate Mergify routing contract: %w (remove stale routing labels: %v)", err, cleanupErr)
			}
			return fmt.Errorf("validate Mergify routing contract: %w", err)
		}
	}
	// Read the PR state after the configuration check so this authority read is
	// immediately adjacent to the final head check and label mutation.
	if err := r.revalidateRoutingState(ctx, report, expectedHead); err != nil {
		return err
	}

	// Review threads are Gatekeeper-owned policy input, not part of the
	// Mergify queue contract. Re-read them immediately before projecting
	// auto-merge so a thread opened after EvaluatePullRequest's initial
	// snapshot cannot slip through the label boundary.
	if plan.autoMerge {
		if err := r.revalidateUnresolvedReviewThreads(ctx, report); err != nil {
			return err
		}
	}
	// Reviewer convergence is local durable policy input, not part of the forge
	// state read above. Re-read it at the same authority boundary so a newly
	// persisted floor-qualified item cannot arrive between evaluation and an
	// auto-merge projection unnoticed.
	if report.Evidence.ReviewerConvergence != nil || plan.autoMerge {
		if err := r.revalidateReviewerConvergence(ctx, report, expectedHead); err != nil {
			return err
		}
	}
	// The final head (and, when applicable, merge-base) read must happen after
	// thread revalidation. A push during that provider call otherwise leaves the
	// label write bound to a head that Gatekeeper never confirmed at the end of
	// its authority sequence. Diff-budget counts are also only valid for the
	// persisted base SHA, so a changed base fails closed before projection.
	var currentHead, currentBase string
	viewInput := githubinfra.ViewPullRequestInput{
		Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
	}
	if report.Evidence.DiffBudget != nil {
		expectedBase := strings.TrimSpace(report.Evidence.DiffBudget.BaseSHA)
		if expectedBase == "" {
			return fmt.Errorf("skip routing labels because diff-budget evidence has no base SHA")
		}
		var err error
		currentHead, currentBase, err = r.github.GetPullRequestHeadAndBaseSHA(ctx, viewInput)
		if err != nil {
			return fmt.Errorf("revalidate head and diff-budget base before routing labels: %w", err)
		}
		if strings.TrimSpace(currentBase) != expectedBase {
			return fmt.Errorf("skip routing labels because diff-budget base moved from %s to %s", expectedBase, strings.TrimSpace(currentBase))
		}
	} else {
		var err error
		currentHead, err = r.github.GetPullRequestHeadSHA(ctx, viewInput)
		if err != nil {
			return fmt.Errorf("revalidate head before routing labels: %w", err)
		}
	}
	if strings.TrimSpace(currentHead) != expectedHead {
		return fmt.Errorf("skip routing labels because pull request head moved from %s to %s", expectedHead, strings.TrimSpace(currentHead))
	}
	return r.applyRoutingLabelPlan(ctx, report, plan)
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
		Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID), Limit: 1000,
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
		Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
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
	// Reports produced by EvaluatePullRequest always carry PullRequestState;
	// hand-built/legacy reports in storage may omit the full authority evidence,
	// so retain their compatibility fallback while enforcing every field on
	// current state-bound reports.
	if state := strings.TrimSpace(report.Evidence.PullRequestState); state != "" {
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
	}
	recordedHold := len(report.Evidence.HoldLabels) > 0
	currentHold := labels.Has(detail.Labels, labels.HoldGlobal)
	if recordedHold != currentHold {
		return fmt.Errorf("skip routing labels because hold state changed")
	}
	if strings.TrimSpace(report.Evidence.BaseRefName) != "" {
		currentPolicy := r.policyPermitsTarget(report.ProjectID, report.Repo, report.Evidence.BaseRefName)
		if currentPolicy != report.Evidence.ProjectPolicyPermitsTarget {
			return fmt.Errorf("skip routing labels because project policy changed")
		}
	}
	return nil
}

func (r *Runner) applyRoutingLabelPlan(ctx context.Context, report Report, plan routingLabelPlan) error {
	input := githubinfra.PullRequestLabelsInput{
		Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
	}
	switch {
	case plan.autoMerge:
		// Remove the human-escalation route before adding the queue route. If
		// the second mutation fails, the PR remains out of the queue rather than
		// being blocked by a stale human-review label forever.
		if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.NeedsHumanReview}}); err != nil {
			return fmt.Errorf("remove stale %s label: %w", labels.NeedsHumanReview, err)
		}
		input.Labels = []string{labels.AutoMerge}
		if err := r.github.AddPullRequestLabels(ctx, input); err != nil {
			return fmt.Errorf("add %s label: %w", labels.AutoMerge, err)
		}
	case plan.needsHumanReview:
		// Stop the queue first. Adding the human route after removal is
		// fail-safe if the second operation is rejected by the forge.
		if err := r.github.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Labels: []string{labels.AutoMerge}}); err != nil {
			return fmt.Errorf("remove stale %s label: %w", labels.AutoMerge, err)
		}
		input.Labels = []string{labels.NeedsHumanReview}
		if err := r.github.AddPullRequestLabels(ctx, input); err != nil {
			return fmt.Errorf("add %s label: %w", labels.NeedsHumanReview, err)
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

func reportNeedsHumanReview(previous *Report, report Report) bool {
	hasRepeatedReviewChanges := previous != nil && hasReasonCode(previous.Reasons, ReasonReviewChangesRequested) && hasReasonCode(report.Reasons, ReasonReviewChangesRequested)
	for _, reason := range report.Reasons {
		switch string(reason.Code) {
		case protectedPathTouchedReason, diffBudgetExceededReason:
			return true
		}
	}
	return hasRepeatedReviewChanges
}

func hasReasonCode(reasons []Reason, want ReasonCode) bool {
	for _, reason := range reasons {
		if reason.Code == want {
			return true
		}
	}
	return false
}
