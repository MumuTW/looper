package fixer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

const regenerationCommentMarker = "<!-- looper:fixer-regenerate-v1"

type regenerationAction string

const (
	regenerationNone      regenerationAction = "none"
	regenerationCompleted regenerationAction = "completed"
	regenerationEscalated regenerationAction = "escalated"
)

type regenerationHandoffError struct {
	cause      error
	checkpoint fixerCheckpoint
	failure    *loopError
}

func (e *regenerationHandoffError) Error() string { return e.cause.Error() }
func (e *regenerationHandoffError) Unwrap() error { return e.cause }

// fixerRegenerationState is the durable side-effect ledger for #462.  Each
// field is written after its remote action, so a crash can replay only the
// missing suffix of comment -> close -> Planner route.
type fixerRegenerationState struct {
	Authority      string `json:"authority,omitempty"`
	IssueRepo      string `json:"issueRepo,omitempty"`
	IssueNumber    int64  `json:"issueNumber,omitempty"`
	CommentID      int64  `json:"commentId,omitempty"`
	Commented      bool   `json:"commented,omitempty"`
	Closed         bool   `json:"closed,omitempty"`
	Routed         bool   `json:"routed,omitempty"`
	Escalated      bool   `json:"escalated,omitempty"`
	EscalationWhy  string `json:"escalationWhy,omitempty"`
	FailureContext string `json:"failureContext,omitempty"`
}

func parseRegenerationState(metadata map[string]any) (fixerRegenerationState, bool) {
	raw, ok := metadata["fixerRegeneration"]
	if !ok || raw == nil {
		return fixerRegenerationState{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fixerRegenerationState{}, false
	}
	var state fixerRegenerationState
	if err := json.Unmarshal(encoded, &state); err != nil || state.Authority == "" {
		return fixerRegenerationState{}, false
	}
	return state, true
}

// handleTerminalExhaustion owns the ordered close-and-regenerate policy.  It
// is called only after the queue row is durably terminal.  A nil callback keeps
// standalone/test runners on the historic behavior; the runtime wires the
// explicit Planner authority callback.
func (r *Runner) handleTerminalExhaustion(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint fixerCheckpoint, failure *loopError) (regenerationAction, error) {
	if r.onRegenerateIssue == nil || failure == nil || failure.kind == FailureManualIntervention {
		return regenerationNone, nil
	}
	if queueItem.Status == "manual_intervention" {
		return regenerationNone, nil
	}
	bridge, ok := r.github.(RegenerationGateway)
	if !ok {
		return regenerationNone, nil
	}

	current := loop
	if r.repos != nil && r.repos.Loops != nil {
		if durable, err := r.repos.Loops.GetByID(ctx, loop.ID); err != nil {
			return regenerationNone, err
		} else if durable != nil {
			current = *durable
		}
	}
	metadata := parseJSONObject(current.MetadataJSON)
	state, exists := parseRegenerationState(metadata)
	if !exists {
		state = fixerRegenerationState{Authority: "fixer-exhaustion:" + current.ID}
	}
	if state.Routed {
		return regenerationCompleted, nil
	}
	if state.Escalated {
		return regenerationEscalated, nil
	}

	pr, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: derefString(queueItem.Repo), PRNumber: derefInt64(queueItem.PRNumber), CWD: project.RepoPath})
	if err != nil {
		return regenerationNone, fmt.Errorf("inspect exhausted fixer PR: %w", err)
	}
	originRepo, originNumber, err := r.regenerationOriginFromLoop(ctx, current)
	if err != nil {
		return regenerationNone, err
	}
	if state.IssueRepo != "" {
		originRepo = state.IssueRepo
	}
	if state.IssueNumber > 0 {
		originNumber = state.IssueNumber
	}
	if originRepo == "" {
		originRepo = derefString(queueItem.Repo)
	}
	state.IssueRepo, state.IssueNumber = originRepo, originNumber

	issue := IssueDetail{Number: originNumber, URL: ""}
	if originNumber > 0 {
		issue, err = bridge.ViewIssue(ctx, ViewIssueInput{Repo: originRepo, IssueNumber: originNumber, CWD: project.RepoPath})
		if err != nil {
			return regenerationNone, fmt.Errorf("inspect originating issue %s#%d: %w", originRepo, originNumber, err)
		}
	}

	failureContext := regenerationFailureContext(current, queueItem, checkpoint, failure)
	state.FailureContext = failureContext
	if reason, guardErr := r.regenerationHumanCommitGuard(ctx, project, derefString(queueItem.Repo), pr); guardErr != nil {
		return regenerationNone, guardErr
	} else if reason != "" || originNumber <= 0 || strings.EqualFold(strings.TrimSpace(issue.State), "closed") {
		if reason == "" {
			switch {
			case originNumber <= 0:
				reason = "originating issue provenance is missing"
			case strings.EqualFold(strings.TrimSpace(issue.State), "closed"):
				reason = "originating issue is already closed"
			}
		}
		if err := r.persistRegenerationEscalation(ctx, current, state, pr.Number, derefString(queueItem.Repo), project.RepoPath, reason); err != nil {
			return regenerationNone, err
		}
		return regenerationEscalated, nil
	}

	if !state.Commented {
		comments, err := bridge.ListIssueComments(ctx, ViewIssueInput{Repo: derefString(queueItem.Repo), IssueNumber: derefInt64(queueItem.PRNumber), CWD: project.RepoPath})
		if err != nil {
			return regenerationNone, fmt.Errorf("inspect exhausted fixer PR comments: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.Body, regenerationCommentMarker) && strings.Contains(comment.Body, "authority="+state.Authority) {
				state.CommentID = comment.ID
				state.Commented = true
				break
			}
		}
	}
	if !state.Commented {
		body := buildRegenerationComment(state.Authority, queueItem, current, checkpoint, failure, originRepo, originNumber, issue.URL)
		comment, err := r.github.CreateIssueComment(ctx, IssueCommentInput{
			Repo:            derefString(queueItem.Repo),
			IssueNumber:     derefInt64(queueItem.PRNumber),
			Body:            body,
			CWD:             project.RepoPath,
			DisclosureAgent: r.agentRuntime,
			DisclosureModel: derefString(r.agentModel),
		})
		if err != nil {
			return regenerationNone, fmt.Errorf("comment exhausted fixer PR: %w", err)
		}
		state.CommentID = comment.ID
		state.Commented = true
		updated, err := r.mergeLoopMetadata(ctx, current, map[string]any{"fixerRegeneration": state})
		if err != nil {
			return regenerationNone, err
		}
		current = updated
	}

	branchOwned := strings.HasPrefix(strings.ToLower(strings.TrimSpace(pr.HeadRefName)), "looper/")
	deleteBranch := branchOwned
	if r.deleteBranchOnRegeneration != nil {
		deleteBranch = branchOwned && r.deleteBranchOnRegeneration(project.ID)
	}
	if !state.Closed {
		if err := bridge.ClosePullRequest(ctx, ClosePullRequestInput{Repo: derefString(queueItem.Repo), PRNumber: derefInt64(queueItem.PRNumber), DeleteBranch: deleteBranch, CWD: project.RepoPath}); err != nil {
			return regenerationNone, fmt.Errorf("close exhausted fixer PR: %w", err)
		}
		state.Closed = true
		updated, err := r.mergeLoopMetadata(ctx, current, map[string]any{"fixerRegeneration": state})
		if err != nil {
			return regenerationNone, err
		}
		current = updated
	}

	if err := r.onRegenerateIssue(ctx, RegenerateIssueInput{
		ProjectID: project.ID, Repo: derefString(queueItem.Repo), IssueRepo: originRepo, IssueNumber: issue.Number,
		IssueTitle: issue.Title, IssueBody: issue.Body, IssueURL: issue.URL, IssueLabels: append([]string(nil), issue.Labels...), IssueAssignees: append([]string(nil), issue.Assignees...),
		FailureSummary: failure.message, FailureContext: failureContext, Attempts: queueItem.Attempts, MaxAttempts: queueItem.MaxAttempts, Authority: state.Authority,
	}); err != nil {
		return regenerationNone, fmt.Errorf("route originating issue back to planner: %w", err)
	}
	if err := bridge.AddIssueLabels(ctx, IssueLabelsInput{Repo: originRepo, IssueNumber: originNumber, Labels: []string{labels.DefaultPlanTrigger}, CWD: project.RepoPath}); err != nil {
		return regenerationNone, fmt.Errorf("mark originating issue for planner: %w", err)
	}
	if err := bridge.RemoveIssueLabels(ctx, IssueLabelsInput{Repo: originRepo, IssueNumber: originNumber, Labels: []string{labels.DefaultWorkerReadyTrigger}, CWD: project.RepoPath}); err != nil {
		return regenerationNone, fmt.Errorf("clear worker-ready label from originating issue: %w", err)
	}
	state.Routed = true
	if _, err := r.mergeLoopMetadata(ctx, current, map[string]any{"fixerRegeneration": state}); err != nil {
		return regenerationNone, err
	}
	return regenerationCompleted, nil
}

func (r *Runner) regenerationHumanCommitGuard(ctx context.Context, project storage.ProjectRecord, repo string, pr PullRequestDetail) (string, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(pr.HeadRefName)), "looper/") {
		return "pull request branch is not Looper-authored", nil
	}
	bridge, ok := r.github.(RegenerationGateway)
	if !ok {
		return "commit provenance gateway is unavailable", nil
	}
	login, err := r.github.GetCurrentUserLogin(ctx, repo, project.RepoPath)
	if err != nil {
		return "authenticated GitHub identity is unavailable", nil
	}
	login = strings.TrimSpace(login)
	commits, err := bridge.ListPullRequestCommits(ctx, ViewPullRequestInput{Repo: repo, PRNumber: pr.Number, CWD: project.RepoPath})
	if err != nil {
		return "unable to verify pull request commit provenance", nil
	}
	for _, commit := range commits {
		for _, author := range []string{strings.TrimSpace(commit.AuthorLogin), strings.TrimSpace(commit.CommitterLogin)} {
			if author == "" {
				return "pull request commit identity is unavailable", nil
			}
			if strings.HasSuffix(strings.ToLower(author), "[bot]") {
				continue
			}
			if login == "" || !strings.EqualFold(author, login) {
				return "pull request contains a human-authored commit", nil
			}
		}
	}
	return "", nil
}

func (r *Runner) persistRegenerationEscalation(ctx context.Context, loop storage.LoopRecord, state fixerRegenerationState, prNumber int64, repo, cwd, reason string) error {
	state.Escalated = true
	state.EscalationWhy = reason
	body := fmt.Sprintf("%s\n\nFixer stopped automatic close-and-regenerate because: %s\nThe PR remains open for human review.\n\nFailure context: %s", regenerationCommentMarker+" authority="+state.Authority+" outcome=escalated -->", reason, state.FailureContext)
	if !state.Commented {
		comment, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: r.agentRuntime, DisclosureModel: derefString(r.agentModel)})
		if err != nil {
			return fmt.Errorf("comment human escalation: %w", err)
		}
		state.CommentID, state.Commented = comment.ID, true
	}
	bridge, ok := r.github.(RegenerationGateway)
	if !ok {
		return fmt.Errorf("human escalation gateway is unavailable")
	}
	if err := bridge.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{labels.NeedsHuman}, CWD: cwd}); err != nil {
		return fmt.Errorf("label human escalation: %w", err)
	}
	_, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerRegeneration": state})
	return err
}

func (r *Runner) regenerationOriginFromLoop(ctx context.Context, loop storage.LoopRecord) (string, int64, error) {
	metadata := parseJSONObject(loop.MetadataJSON)
	candidates := []map[string]any{metadata, mapFromAny(metadata["worker"])}
	if sourceWorkerID, _ := stringFromAny(metadata["sourceWorkerId"]); sourceWorkerID != "" && r.repos != nil && r.repos.Loops != nil {
		worker, err := r.repos.Loops.GetByID(ctx, sourceWorkerID)
		if err != nil {
			return "", 0, err
		}
		if worker != nil {
			workerMetadata := parseJSONObject(worker.MetadataJSON)
			candidates = append(candidates, workerMetadata, mapFromAny(workerMetadata["worker"]))
		}
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		repo, _ := stringFromAny(candidate["issueRepo"])
		if repo == "" {
			repo, _ = stringFromAny(candidate["repo"])
		}
		number := int64FromAny(candidate["issueNumber"])
		if number > 0 {
			return strings.TrimSpace(repo), number, nil
		}
	}
	return "", 0, nil
}

func regenerationFailureContext(loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint fixerCheckpoint, failure *loopError) string {
	return fmt.Sprintf("attempts=%d/%d; failure=%s; kind=%s; headSha=%s; fixItemsFingerprint=%s; loop=%s; queueItem=%s", queueItem.Attempts, queueItem.MaxAttempts, strings.TrimSpace(failure.message), failure.kind, detailHeadSHA(checkpoint.Detail), hashFixItemsState(checkpoint.FixItems), loop.ID, queueItem.ID)
}

func buildRegenerationComment(authority string, queueItem storage.QueueItemRecord, loop storage.LoopRecord, checkpoint fixerCheckpoint, failure *loopError, issueRepo string, issueNumber int64, issueURL string) string {
	issueRef := ""
	if strings.TrimSpace(issueURL) != "" {
		issueRef = " (" + strings.TrimSpace(issueURL) + ")"
	}
	return fmt.Sprintf("%s authority=%s -->\n\nFixer exhausted its retry budget and will close this PR before returning the originating issue to Planner.\n\n- Attempts: %d/%d\n- Failure: %s\n- Failure kind: %s\n- Head fingerprint: `%s`\n- Fix-items fingerprint: `%s`\n- Originating issue: %s#%d%s\n- Loop: `%s`", regenerationCommentMarker, authority, queueItem.Attempts, queueItem.MaxAttempts, strings.TrimSpace(failure.message), failure.kind, detailHeadSHA(checkpoint.Detail), hashFixItemsState(checkpoint.FixItems), issueRepo, issueNumber, issueRef, loop.ID)
}

func (r *Runner) markRegeneratedLoopFailed(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	return r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "failed"
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	})
}

func (r *Runner) applyTerminalRegeneration(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint fixerCheckpoint, failure *loopError) (storage.LoopRecord, regenerationAction, error) {
	action, err := r.handleTerminalExhaustion(ctx, project, loop, queueItem, checkpoint, failure)
	if err != nil {
		return loop, regenerationNone, &regenerationHandoffError{cause: err, checkpoint: checkpoint, failure: failure}
	}
	if action != regenerationCompleted && action != regenerationEscalated {
		return loop, action, nil
	}
	updated, err := r.markRegeneratedLoopFailed(ctx, loop)
	if err != nil {
		return loop, regenerationNone, err
	}
	return updated, action, nil
}

func mapFromAny(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}
