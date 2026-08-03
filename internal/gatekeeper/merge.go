package gatekeeper

// MergeOutcomeEventType records a historical Gatekeeper merge attempt at the
// auto trust level. Auto trust now publishes commit status only and does not
// merge; this event type remains so Auditor and post-merge digest can read
// outcomes written before that contract change.
const MergeOutcomeEventType = "pull_request.merge_gate.merge_attempted"

// MergeOutcome is the durable record of one merge attempt.
type MergeOutcome struct {
	Version   int    `json:"version"`
	ProjectID string `json:"projectId"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"prNumber"`
	// HeadSHA is the commit the decision was made about and, on success, the
	// commit that was merged.
	HeadSHA string `json:"headSha"`
	// MergeStrategy records the configured forge strategy so Auditor can reject
	// a rebase tip as incomplete revert provenance.
	MergeStrategy string `json:"mergeStrategy,omitempty"`
	// TouchedFiles is GitHub's authoritative pull-request file list captured
	// after a successful merge. Auditor may use it as attribution evidence; it
	// is not merge authority and therefore a read failure never undoes a merge.
	TouchedFiles []string `json:"touchedFiles,omitempty"`
	// TouchedFilesAvailable records whether TouchedFiles was read successfully.
	// An empty list is otherwise ambiguous: a pull request with no files is not
	// expected, but a failed optional read must not look like authoritative
	// evidence that the merge touched nothing.
	TouchedFilesAvailable bool `json:"touchedFilesAvailable"`
	// MergeCommitSHA is the default-branch commit GitHub created for the merge.
	// It is distinct from the PR head for squash/rebase merges and is the only
	// commit an Auditor may later revert.
	MergeCommitSHA string `json:"mergeCommitSha,omitempty"`
	// SourceIssue is GitHub's explicit closing-issue relationship captured
	// during confirming evaluation. Empty means Auditor cannot safely reopen an
	// issue as part of a future revert proposal.
	SourceIssue *githubinfra.IssueReference `json:"sourceIssue,omitempty"`
	Merged      bool                        `json:"merged"`
	// Reason explains a refusal. Empty on success.
	Reason string `json:"reason,omitempty"`
	// ConfirmingReasons are the gates that blocked the confirming evaluation, when
	// the first pass said eligible and the second did not.
	ConfirmingReasons []Reason `json:"confirmingReasons,omitempty"`
	AttemptedAt       string   `json:"attemptedAt"`
}

const (
	refusalHeadMoved     = "head_moved_between_evaluations"
	refusalNoLongerClean = "gates_no_longer_pass"
	refusalMergeFailed   = "forge_refused_the_merge"
	refusalMergePending  = "merge_not_completed"
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
//
// The merge that follows binds only the head (see MergePullRequest). GitHub's
// merge API accepts no parameter that atomically pins the base, so when the diff
// budget is enabled and the base branch advances between the confirming pass's
// final revalidation read and the merge call, the merge can proceed against a
// new base whose recomputed diff exceeds the budget. The confirming pass narrows
// that window to the calls between the final read and the merge but cannot close
// it; this is a documented blind spot of the diff-budget gate rather than a
// property this path can enforce.
func (r *Runner) confirmAndMerge(ctx context.Context, input EvaluationInput, report Report) error {
	outcome := MergeOutcome{
		Version: 1, ProjectID: report.ProjectID, Repo: report.Repo, PRNumber: report.PRNumber,
		HeadSHA: report.Evidence.FinalObservedHeadSHA, AttemptedAt: r.now().UTC().Format(time.RFC3339Nano),
	}

	confirmInput := input
	confirmInput.Confirming = true
	confirmInput.ExpectedHeadSHA = outcome.HeadSHA
	confirmation, err := r.EvaluatePullRequest(ctx, confirmInput)
	if err != nil {
		return err
	}

	switch {
	case confirmation.Evidence.FinalObservedHeadSHA != outcome.HeadSHA:
		outcome.Reason = refusalHeadMoved
	case !confirmation.Eligible:
		outcome.Reason = refusalNoLongerClean
		outcome.ConfirmingReasons = confirmation.Reasons
	}
	if outcome.Reason != "" {
		return r.persistMergeOutcome(ctx, outcome)
	}
	strategy := r.mergeStrategy(report.ProjectID)
	outcome.MergeStrategy = string(strategy)

	if err := r.github.MergePullRequest(ctx, githubinfra.EnableAutoMergeInput{
		Repo: report.Repo, PRNumber: report.PRNumber,
		Strategy: strategy, HeadSHA: outcome.HeadSHA,
		CWD: r.projectCWD(ctx, report.ProjectID),
	}); err != nil {
		// The forge has its own view — branch protection, a race with another
		// merge — and refusing is a legitimate answer, not a lane failure.
		outcome.Reason = refusalMergeFailed
		if recordErr := r.persistMergeOutcome(ctx, outcome); recordErr != nil {
			return recordErr
		}
		if r.logWarn != nil {
			r.logWarn("gatekeeper: the forge refused the merge", map[string]any{
				"repo": report.Repo, "pr": report.PRNumber, "error": err.Error(),
			})
		}
		return nil
	}

	mergedDetail, detailErr := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID)})
	if detailErr != nil || strings.TrimSpace(mergedDetail.MergedAt) == "" {
		outcome.Reason = refusalMergePending
		if detailErr != nil && r.logWarn != nil {
			r.logWarn("gatekeeper: merge request accepted but completion was not observable", map[string]any{"repo": report.Repo, "pr": report.PRNumber, "error": detailErr.Error()})
		}
		if recordErr := r.persistMergeOutcome(ctx, outcome); recordErr != nil {
			return recordErr
		}
		return nil
	}
	outcome.Merged = true
	if strategy != config.ReviewerAutoMergeStrategyRebase {
		outcome.MergeCommitSHA = strings.TrimSpace(mergedDetail.MergeCommitSHA)
	}
	// Read the closing relationship after completion. The merge-watch response
	// is the authority that proves the merge; this second read only enriches the
	// durable outcome and is safe to leave empty when the optional relationship
	// is unavailable on an Enterprise API.
	if sourceDetail, sourceErr := r.github.ViewPullRequestForGatekeeper(ctx, githubinfra.ViewPullRequestInput{Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID)}); sourceErr == nil && strings.TrimSpace(sourceDetail.HeadSHA) == outcome.HeadSHA {
		outcome.SourceIssue = sameRepositorySourceIssue(sourceDetail.ClosingIssues, report.Repo)
	}
	files, filesErr := r.github.ListPullRequestFiles(ctx, githubinfra.ViewPullRequestInput{Repo: report.Repo, PRNumber: report.PRNumber, CWD: r.projectCWD(ctx, report.ProjectID)})
	if filesErr != nil {
		if r.logWarn != nil {
			r.logWarn("gatekeeper: could not capture merged pull request files", map[string]any{"repo": report.Repo, "pr": report.PRNumber, "error": filesErr.Error()})
		}
	} else {
		outcome.TouchedFiles = files
		if mergedDetail.DiffStats != nil && mergedDetail.DiffStats.ChangedFiles > len(files) {
			if r.logWarn != nil {
				r.logWarn("gatekeeper: pull request file evidence is truncated", map[string]any{"repo": report.Repo, "pr": report.PRNumber, "returned": len(files), "changedFiles": mergedDetail.DiffStats.ChangedFiles})
			}
		} else {
			outcome.TouchedFilesAvailable = true
		}
	}
	return r.persistMergeOutcome(ctx, outcome)
}

func sameRepositorySourceIssue(issues []githubinfra.IssueReference, repo string) *githubinfra.IssueReference {
	var source *githubinfra.IssueReference
	for _, issue := range issues {
		if issue.Number <= 0 || !strings.EqualFold(strings.TrimSpace(issue.Repo), strings.TrimSpace(repo)) {
			continue
		}
		if source != nil {
			return nil
		}
		copied := issue
		source = &copied
	}
	return source
}

func (r *Runner) mergeStrategy(projectID string) config.ReviewerAutoMergeStrategy {
	if r.mergeStrategyForProject == nil {
		return config.ReviewerAutoMergeStrategySquash
	}
	strategy := r.mergeStrategyForProject(projectID)
	if strings.TrimSpace(string(strategy)) == "" {
		return config.ReviewerAutoMergeStrategySquash
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

var _ = func(r *storage.Repositories) { _ = r.Events }
