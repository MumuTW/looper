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
// consumed by the Mergify queue. The report and the label mutation are bound to
// the same fresh head read; a changed head causes the projection to be retried on
// the next evaluation rather than routing a different commit.
func (r *Runner) reconcileRoutingLabels(ctx context.Context, report Report, previous *Report) error {
	trust := r.trustFor(report.ProjectID)
	if trust == config.GatekeeperTrustObserve && (previous == nil || !previousPublished(*previous)) {
		return nil
	}

	plan := routingLabelPlan{}
	if trust != config.GatekeeperTrustObserve && report.Eligible {
		plan.autoMerge = true
	} else if trust != config.GatekeeperTrustObserve && reportNeedsHumanReview(previous, report) {
		plan.needsHumanReview = true
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

	currentHead, err := r.github.GetPullRequestHeadSHA(ctx, githubinfra.ViewPullRequestInput{
		Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID),
	})
	if err != nil {
		return fmt.Errorf("revalidate head before routing labels: %w", err)
	}
	if strings.TrimSpace(currentHead) != expectedHead {
		return fmt.Errorf("skip routing labels because pull request head moved from %s to %s", expectedHead, strings.TrimSpace(currentHead))
	}
	return r.applyRoutingLabelPlan(ctx, report, plan)
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
