package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// MergeOutcomeEventType records what Gatekeeper did with an eligible pull
// request at the auto trust level, including every reason it declined.
const MergeOutcomeEventType = "pull_request.merge_gate.merge_attempted"

// MergeOutcomePendingReason is intentionally not a forge refusal.  It marks
// the durable point immediately before the external merge call, so recovery
// can reconcile a merge that succeeded after the final append was lost.
const MergeOutcomePendingReason = "merge_pending_before_forge"

// MergeOutcome is the durable record of one merge attempt.
type MergeOutcome struct {
	Version   int    `json:"version"`
	ProjectID string `json:"projectId"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"prNumber"`
	// HeadSHA is the commit the decision was made about and, on success, the
	// commit that was merged.
	HeadSHA        string                      `json:"headSha"`
	BaseSHA        string                      `json:"baseSha,omitempty"`
	MergeCommitSHA string                      `json:"mergeCommitSha,omitempty"`
	SourceIssue    *githubinfra.IssueReference `json:"sourceIssue,omitempty"`
	// TouchedFiles is GitHub's authoritative pull-request file list captured
	// after a successful merge. Auditor may use it as attribution evidence.
	TouchedFiles          []string `json:"touchedFiles,omitempty"`
	TouchedFilesAvailable bool     `json:"touchedFilesAvailable,omitempty"`
	Merged                bool     `json:"merged"`
	// Pending is true only for the pre-forge reservation.  The forge's observed
	// PR state, never this marker or an agent report, is authoritative for the
	// eventual Merged value.
	Pending bool `json:"pending,omitempty"`
	// Reason explains a refusal. Empty on success.
	Reason string `json:"reason,omitempty"`
	// ConfirmingReasons are the gates that blocked the confirming evaluation, when
	// the first pass said eligible and the second did not.
	ConfirmingReasons []Reason `json:"confirmingReasons,omitempty"`
	AttemptedAt       string   `json:"attemptedAt"`
}

const (
	refusalHeadMoved      = "head_moved_between_evaluations"
	refusalBaseMoved      = "base_moved_between_evaluations"
	refusalNoLongerClean  = "gates_no_longer_pass"
	refusalMergeFailed    = "forge_refused_the_merge"
	refusalClosedUnmerged = "forge_closed_without_merge"
)

// confirmAndMerge re-runs the full evaluation and merges only if it still passes
// against the same head.
//
// The Gate report is audit evidence, not merge authority: holds, reviews,
// threads, and project policy can all change without moving the head, so a
// report is only ever a statement about the moment it was made. Acting on one
// requires making it true again first.
//
// The confirming pass is a complete evaluation, not a head comparison. A cheaper
// check would miss exactly the changes the invariant names.
func (r *Runner) confirmAndMerge(ctx context.Context, input EvaluationInput, report Report) (Report, error) {
	outcome := MergeOutcome{
		Version: 1, ProjectID: report.ProjectID, Repo: report.Repo, PRNumber: report.PRNumber,
		HeadSHA: report.Evidence.FinalObservedHeadSHA, AttemptedAt: r.now().UTC().Format(time.RFC3339Nano),
	}

	confirmInput := input
	confirmInput.Confirming = true
	confirmInput.ExpectedHeadSHA = outcome.HeadSHA
	confirmation, err := r.EvaluatePullRequest(ctx, confirmInput)
	if err != nil {
		return Report{}, err
	}

	switch {
	case confirmation.Evidence.FinalObservedHeadSHA != outcome.HeadSHA:
		outcome.Reason = refusalHeadMoved
	case strings.TrimSpace(confirmation.Evidence.BaseRefName) != strings.TrimSpace(report.Evidence.BaseRefName):
		outcome.Reason = refusalBaseMoved
	case !confirmation.Eligible:
		outcome.Reason = refusalNoLongerClean
		outcome.ConfirmingReasons = confirmation.Reasons
	}
	if outcome.Reason != "" {
		if outcome.Reason == refusalBaseMoved {
			// The confirming evaluation was persisted before it could compare its
			// base with the primary pass. Persist a second confirming projection
			// with an explicit provider block so the durable report and published
			// status cannot continue to describe an eligible decision.
			confirmation.Reasons = append(confirmation.Reasons, Reason{Code: ReasonProviderStateAmbiguous, Subject: "base_branch"})
			persistedConfirmation, persistErr := r.persist(withConfirming(ctx), confirmation)
			if persistErr != nil {
				return Report{}, persistErr
			}
			confirmation = persistedConfirmation
		}
		if len(outcome.ConfirmingReasons) == 0 && len(confirmation.Reasons) > 0 {
			outcome.ConfirmingReasons = confirmation.Reasons
		}
		if err := r.persistMergeOutcome(ctx, outcome); err != nil {
			return Report{}, err
		}
		// The primary pass may have deferred status publication because discovery
		// batches all pull requests by commit. Once the confirming pass blocks,
		// publish that verdict before returning so branch protection cannot keep a
		// stale success visible. Discovery republishes the same blocked verdict
		// from the returned report; setCommitStatusIfChanged makes that harmless.
		if err := r.publishConfirmingCommitStatus(ctx, confirmation); err != nil {
			return Report{}, fmt.Errorf("publish confirming blocked status: %w", err)
		}
		return confirmation, nil
	}
	// Record the attempt before crossing the forge boundary.  If the process
	// loses the post-merge append, this pending record gives the next discovery
	// tick a durable entity and head to reconcile without retrying blindly. It
	// must also precede the green status so persistence failure cannot leave a
	// success signal visible without a durable reservation.
	attempt := outcome
	attempt.Pending = true
	attempt.Reason = MergeOutcomePendingReason
	if err := r.persistMergeOutcome(ctx, attempt); err != nil {
		return Report{}, err
	}

	// Discovery defers the primary status until all pull requests sharing a head
	// have been aggregated. The confirming pass is the final authority for an
	// auto merge, so publish its eligible status before crossing the forge merge
	// boundary; otherwise required-status protection rejects the merge and the
	// next tick becomes the first attempt.
	if err := r.publishConfirmingCommitStatus(ctx, confirmation); err != nil {
		return Report{}, fmt.Errorf("publish confirming eligible status: %w", err)
	}

	cwd := r.projectCWD(ctx, report.ProjectID)
	if err := r.github.MergePullRequest(ctx, githubinfra.PullRequestMergeInput{
		Repo: report.Repo, PRNumber: report.PRNumber,
		Strategy: r.mergeStrategy(report.ProjectID), HeadSHA: outcome.HeadSHA,
		BaseSHA:    confirmation.Evidence.FinalObservedBaseSHA,
		BaseBranch: confirmation.Evidence.BaseRefName,
		CWD:        cwd,
	}); err != nil {
		if githubinfra.IsTransientError(err) || githubinfra.IsAuthorizationError(err) {
			return Report{}, err
		}
		// The forge has its own view — branch protection, a race with another
		// merge — and refusing is a legitimate answer, not a lane failure.
		outcome.Reason = refusalMergeFailed
		outcome.Pending = false
		if recordErr := r.persistMergeOutcome(ctx, outcome); recordErr != nil {
			return Report{}, recordErr
		}
		if r.logWarn != nil {
			r.logWarn("gatekeeper: the forge refused the merge", map[string]any{
				"repo": report.Repo, "pr": report.PRNumber, "error": err.Error(),
			})
		}
		return confirmation, nil
	}

	// MergePullRequest only reports whether the forge accepted the mutation.
	// Read the merged PR back before recording success so the durable outcome
	// carries the forge-assigned merge commit and can be attributed downstream.
	mergedDetail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{
		Repo: report.Repo, PRNumber: report.PRNumber, CWD: cwd,
	})
	if err != nil {
		return Report{}, fmt.Errorf("read merged pull request #%d: %w", report.PRNumber, err)
	}
	outcome.MergeCommitSHA = strings.TrimSpace(mergedDetail.MergeCommitSHA)
	if outcome.MergeCommitSHA == "" {
		return Report{}, fmt.Errorf("read merged pull request #%d: merge commit SHA is missing", report.PRNumber)
	}
	outcome.Merged = true
	outcome.Pending = false
	if err := r.persistMergeOutcome(ctx, outcome); err != nil {
		return Report{}, err
	}
	return confirmation, nil
}

func (r *Runner) mergeStrategy(projectID string) config.MergeStrategy {
	if r.mergeStrategyForProject == nil {
		return config.MergeStrategySquash
	}
	strategy := r.mergeStrategyForProject(projectID)
	if strings.TrimSpace(string(strategy)) == "" {
		return config.MergeStrategySquash
	}
	return strategy
}

func (r *Runner) persistMergeOutcome(ctx context.Context, outcome MergeOutcome) error {
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", outcome.Repo, outcome.PRNumber)
	projectID := outcome.ProjectID
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: MergeOutcomeEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID,
		Payload: outcome, CreatedAt: r.now(),
	}); err != nil {
		return fmt.Errorf("persist merge outcome: %w", err)
	}
	return nil
}

// reconcilePendingMergeOutcomes settles the durable pre-forge marker after a
// process crash or a post-merge SQLite failure.  The latest event for each PR
// is considered; a settled refusal or success therefore supersedes any older
// pending attempt.  Only the forge's current PR state authorizes a success.
func (r *Runner) reconcilePendingMergeOutcomes(ctx context.Context, projectID, repo, cwd string) error {
	if r == nil || r.repos == nil || r.repos.Events == nil || r.github == nil {
		return nil
	}
	// The merge-outcome event type is the authority for this recovery pass. The
	// repository returns only the latest matching row per pull request, so a
	// long Gatekeeper/report history and unrelated pull-request events never
	// enter this reconciliation query or its decoder.
	records, err := r.repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, projectID, "pull_request", []string{MergeOutcomeEventType})
	if err != nil {
		return fmt.Errorf("list pending merge outcomes: %w", err)
	}
	pendingByEntity := map[string]MergeOutcome{}
	for _, record := range records {
		var outcome MergeOutcome
		if err := json.Unmarshal([]byte(record.PayloadJSON), &outcome); err != nil {
			continue
		}
		if record.EntityID == nil || strings.TrimSpace(*record.EntityID) == "" {
			continue
		}
		if outcome.Pending {
			pendingByEntity[*record.EntityID] = outcome
		}
	}
	entityIDs := make([]string, 0, len(pendingByEntity))
	for entityID := range pendingByEntity {
		entityIDs = append(entityIDs, entityID)
	}
	sort.Strings(entityIDs)
	for _, entityID := range entityIDs {
		outcome := pendingByEntity[entityID]
		detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: outcome.Repo, PRNumber: outcome.PRNumber, CWD: cwd})
		if err != nil {
			// A missing or temporarily unavailable PR is not evidence of either
			// outcome. Leave the marker durable for the next reconciliation tick.
			if r.logWarn != nil {
				r.logWarn("gatekeeper: pending merge outcome could not be reconciled", map[string]any{
					"repo": outcome.Repo, "pr": outcome.PRNumber, "error": err.Error(),
				})
			}
			continue
		}
		merged := strings.EqualFold(strings.TrimSpace(detail.State), "merged") || strings.TrimSpace(detail.MergedAt) != ""
		closed := strings.EqualFold(strings.TrimSpace(detail.State), "closed")
		if !merged && !closed {
			continue
		}
		outcome.Pending = false
		outcome.Reason = ""
		// A merged state alone does not identify which commit was merged. The
		// forge head is the authority for that identity; never attribute a
		// later head's merge to this pending attempt.
		observedHead := strings.TrimSpace(detail.HeadSHA)
		if merged && observedHead == "" {
			// Missing head evidence is indeterminate. Keep the pending marker for
			// a later read instead of settling a success we cannot attribute.
			continue
		}
		if merged && strings.TrimSpace(detail.MergeCommitSHA) == "" {
			// A merged state without the forge-assigned commit is not enough to
			// attribute the durable success. Keep the pre-forge marker pending
			// until a later authoritative read supplies the identity.
			continue
		}
		if merged && !strings.EqualFold(observedHead, strings.TrimSpace(outcome.HeadSHA)) {
			outcome.Merged = false
			outcome.Reason = refusalHeadMoved
		} else if merged {
			outcome.Merged = true
			outcome.MergeCommitSHA = strings.TrimSpace(detail.MergeCommitSHA)
		} else {
			outcome.Merged = false
			outcome.Reason = refusalClosedUnmerged
		}
		if err := r.persistMergeOutcome(ctx, outcome); err != nil {
			return err
		}
	}
	return nil
}
