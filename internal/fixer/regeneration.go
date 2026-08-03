package fixer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/outboundguard"
	"github.com/MumuTW/looper/internal/storage"
)

const regenerationCommentMarker = "<!-- looper:fixer-regenerate-v1"
const (
	contextWithheldEventType             = "fixer.escalation.context_withheld"
	regenerationCommentWithheldEventType = "fixer.regeneration.comment_withheld"
)

type regenerationAction string

const (
	regenerationNone      regenerationAction = "none"
	regenerationCompleted regenerationAction = "completed"
	regenerationEscalated regenerationAction = "escalated"
)

type regenerationHandoffError struct {
	cause      error
	checkpoint fixerCheckpoint
	failure    *runpipe.LoopError
}

func (e *regenerationHandoffError) Error() string { return e.cause.Error() }
func (e *regenerationHandoffError) Unwrap() error { return e.cause }

// RegenerationPermanentRejectionError signals that planner routing permanently
// rejected the issue (e.g., archived project, held lane). This triggers escalation
// to human intervention instead of retry.
type RegenerationPermanentRejectionError struct {
	Reason string
}

func (e *RegenerationPermanentRejectionError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "planner routing permanently rejected issue"
}

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
	FailureSummary string `json:"failureSummary,omitempty"`
	FailureKind    string `json:"failureKind,omitempty"`
	Attempts       int64  `json:"attempts,omitempty"`
	MaxAttempts    int64  `json:"maxAttempts,omitempty"`
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

func regenerationFailureFromState(state fixerRegenerationState) *runpipe.LoopError {
	kind := runpipe.QueueFailureKind(strings.TrimSpace(state.FailureKind))
	if kind == "" {
		kind = runpipe.FailureRetryableTransient
	}
	message := strings.TrimSpace(state.FailureSummary)
	if message == "" {
		message = "fixer terminal regeneration replay"
	}
	return &runpipe.LoopError{Message: message, Kind: kind}
}

// handleTerminalExhaustion owns the ordered close-and-regenerate policy.  It
// is called only after the queue row is durably terminal.  A nil callback keeps
// standalone/test runners on the historic behavior; the runtime wires the
// explicit Planner authority callback.
func (r *Runner) handleTerminalExhaustion(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint fixerCheckpoint, failure *runpipe.LoopError) (regenerationAction, error) {
	if r.onRegenerateIssue == nil || failure == nil || failure.Kind == runpipe.FailureManualIntervention {
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
	// The queue row is terminal before this policy runs. Always use that
	// durable row for the attempt numbers; the claimed input is the pre-failure
	// snapshot and is one attempt behind on the final retry.
	if r.repos != nil && r.repos.Queue != nil && strings.TrimSpace(queueItem.ID) != "" {
		if durable, err := r.repos.Queue.GetByID(ctx, queueItem.ID); err != nil {
			return regenerationNone, err
		} else if durable != nil {
			queueItem = *durable
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
	if state.FailureSummary == "" {
		state.FailureSummary = failure.Message
	}
	if state.FailureKind == "" {
		state.FailureKind = string(failure.Kind)
	}
	if state.Attempts == 0 && queueItem.Attempts != 0 {
		state.Attempts = queueItem.Attempts
	}
	if state.MaxAttempts == 0 && queueItem.MaxAttempts != 0 {
		state.MaxAttempts = queueItem.MaxAttempts
	}
	if queueItem.Attempts == 0 && state.Attempts != 0 {
		queueItem.Attempts = state.Attempts
	}
	if queueItem.MaxAttempts == 0 && state.MaxAttempts != 0 {
		queueItem.MaxAttempts = state.MaxAttempts
	}
	if state.FailureContext == "" {
		state.FailureContext = regenerationFailureContext(current, queueItem, checkpoint, failure)
	}
	// Persist the replay authority before the first forge side effect. This
	// leaves a durable handoff even if PR/Issue inspection or comment creation
	// fails, so the failed queue row can be safely re-driven.
	if !exists || state.FailureContext != "" {
		updated, err := r.mergeLoopMetadata(ctx, current, map[string]any{"fixerRegeneration": state})
		if err != nil {
			return regenerationNone, err
		}
		current = updated
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

	failureContext := state.FailureContext
	if strings.TrimSpace(failureContext) == "" {
		failureContext = regenerationFailureContext(current, queueItem, checkpoint, failure)
		state.FailureContext = failureContext
	}
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
	if strings.EqualFold(strings.TrimSpace(pr.State), "merged") {
		return r.persistRegenerationAbort(ctx, current, state, "pull request was merged concurrently; regeneration aborted")
	}
	if r.regenerationAvailability != nil {
		if reason := strings.TrimSpace(r.regenerationAvailability(project.ID)); reason != "" {
			if err := r.persistRegenerationEscalation(ctx, current, state, pr.Number, derefString(queueItem.Repo), project.RepoPath, reason); err != nil {
				return regenerationNone, err
			}
			return regenerationEscalated, nil
		}
	}

	if !state.Commented {
		commentWithheld, err := r.hasRegenerationCommentWithheldEvent(ctx, current.ID, derefString(queueItem.Repo), derefInt64(queueItem.PRNumber), state.Authority)
		if err != nil {
			return regenerationNone, fmt.Errorf("inspect withheld regeneration comment evidence: %w", err)
		}
		if commentWithheld {
			// The deterministic event is the durable authority for an omitted
			// optional publication. Promote it to the ordinary checkpoint so a
			// replay cannot submit the rejected bodies again.
			state.Commented = true
		} else {
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
			if !outboundguard.IsRejection(err) {
				return regenerationNone, fmt.Errorf("comment exhausted fixer PR: %w", err)
			}
			// The original body includes durable failure context and can be
			// deterministically rejected by the outbound gate. Retry once with
			// daemon-composed metadata only; a transport/forge error remains
			// retryable, while a second gate rejection settles the optional
			// comment step so close-and-regenerate can still make progress.
			fallback := buildRegenerationCommentWithoutFailureContext(state.Authority, queueItem, current, checkpoint, originRepo, originNumber, issue.URL)
			fallbackComment, fallbackErr := r.github.CreateIssueComment(ctx, IssueCommentInput{
				Repo:            derefString(queueItem.Repo),
				IssueNumber:     derefInt64(queueItem.PRNumber),
				Body:            fallback,
				CWD:             project.RepoPath,
				DisclosureAgent: r.agentRuntime,
				DisclosureModel: derefString(r.agentModel),
			})
			if fallbackErr != nil {
				if !outboundguard.IsRejection(fallbackErr) {
					return regenerationNone, fmt.Errorf("comment exhausted fixer PR fallback: %w", fallbackErr)
				}
				r.logWarn("fixer regeneration comment withheld by content safety gate", map[string]any{"loopId": current.ID, "repo": derefString(queueItem.Repo), "prNumber": queueItem.PRNumber, "error": fallbackErr.Error()})
				if err := r.appendRegenerationCommentWithheldEvent(ctx, current, derefString(queueItem.Repo), derefInt64(queueItem.PRNumber), state.Authority); err != nil {
					return regenerationNone, err
				}
				// Commented means the optional publication step is settled; the
				// durable close/route checkpoints remain the lifecycle authority.
				state.Commented = true
			} else {
				state.CommentID, state.Commented = fallbackComment.ID, true
			}
		} else {
			state.CommentID = comment.ID
			state.Commented = true
		}
		updated, err := r.mergeLoopMetadata(ctx, current, map[string]any{"fixerRegeneration": state})
		if err != nil {
			return regenerationNone, err
		}
		current = updated
	}

	branchOwned := strings.HasPrefix(strings.ToLower(strings.TrimSpace(pr.HeadRefName)), "looper/")
	// Branch deletion is destructive.  The runtime supplies the effective
	// project policy (including the documented default); an unconfigured runner
	// must not infer permission from branch ownership alone.
	deleteBranch := false
	if r.deleteBranchOnRegeneration != nil {
		deleteBranch = branchOwned && r.deleteBranchOnRegeneration(project.ID)
	}
	if !state.Closed {
		if err := bridge.ClosePullRequest(ctx, ClosePullRequestInput{Repo: derefString(queueItem.Repo), PRNumber: derefInt64(queueItem.PRNumber), DeleteBranch: deleteBranch, CWD: project.RepoPath}); err != nil {
			if errors.Is(err, githubinfra.ErrPullRequestAlreadyMerged) {
				return r.persistRegenerationAbort(ctx, current, state, "pull request was merged concurrently; regeneration aborted")
			}
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
		FailureSummary: failure.Message, FailureContext: failureContext, Attempts: queueItem.Attempts, MaxAttempts: queueItem.MaxAttempts, Authority: state.Authority,
	}); err != nil {
		// If planner permanently rejects the routing (e.g., archived project, held lane),
		// escalate to human intervention instead of retrying indefinitely.
		var permanentRejection *RegenerationPermanentRejectionError
		if errors.As(err, &permanentRejection) {
			reason := permanentRejection.Reason
			if reason == "" {
				reason = "planner routing was permanently rejected"
			}
			// PR is already closed at this point. Add looper:needs-human to the originating issue
			// so humans are aware the regeneration failed permanently.
			if err := bridge.AddIssueLabels(ctx, IssueLabelsInput{Repo: originRepo, IssueNumber: originNumber, Labels: []string{labels.NeedsHuman}, CWD: project.RepoPath}); err != nil {
				return regenerationNone, fmt.Errorf("label originating issue for permanent rejection: %w", err)
			}
			state.Escalated = true
			state.EscalationWhy = reason
			if _, err := r.mergeLoopMetadata(ctx, current, map[string]any{"fixerRegeneration": state}); err != nil {
				return regenerationNone, err
			}
			return regenerationEscalated, nil
		}
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
	state.EscalationWhy = reason
	body := fmt.Sprintf("%s\n\nFixer stopped automatic close-and-regenerate because: %s\nThe PR remains open for human review.\n\nFailure context: %s", regenerationCommentMarker+" authority="+state.Authority+" outcome=escalated -->", reason, state.FailureContext)
	bridge, ok := r.github.(RegenerationGateway)
	if !ok {
		return fmt.Errorf("human escalation gateway is unavailable")
	}
	contextWithheld := false
	if !state.Commented {
		var err error
		contextWithheld, err = r.hasContextWithheldEvent(ctx, loop.ID, repo, prNumber, reason)
		if err != nil {
			return fmt.Errorf("inspect withheld-context escalation evidence: %w", err)
		}
		if contextWithheld {
			// The durable event is the settled-comment authority when the
			// explanation was omitted. Persist the ordinary checkpoint as soon as
			// replay observes that evidence so later retries do not need to infer
			// publication state from the event log again.
			state.Commented = true
		}
		if !contextWithheld {
			comments, err := bridge.ListIssueComments(ctx, ViewIssueInput{Repo: repo, IssueNumber: prNumber, CWD: cwd})
			if err != nil {
				return fmt.Errorf("inspect human escalation comments: %w", err)
			}
			for _, comment := range comments {
				if strings.Contains(comment.Body, regenerationCommentMarker) && strings.Contains(comment.Body, "authority="+state.Authority) && strings.Contains(comment.Body, "outcome=escalated") {
					state.CommentID, state.Commented = comment.ID, true
					break
				}
			}
		}
	}
	if !state.Commented && !contextWithheld {
		comment, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: r.agentRuntime, DisclosureModel: derefString(r.agentModel)})
		switch {
		case err == nil:
			state.CommentID, state.Commented = comment.ID, true
		case outboundguard.IsRejection(err):
			// The body embeds the durable FailureContext, so every retry would
			// compose the byte-identical comment and hit the identical gate
			// rejection — there is no agent session here to rewrite it. Complete
			// the escalation with a context-free fallback instead; the full
			// context stays in local loop metadata.
			updated, fallbackErr := r.escalateWithoutFailureContext(ctx, loop, state, prNumber, repo, cwd, reason)
			if fallbackErr != nil {
				return fmt.Errorf("comment human escalation: %w", fallbackErr)
			}
			state = updated
		default:
			return fmt.Errorf("comment human escalation: %w", err)
		}
	}
	// Checkpoint the comment before the label mutation. If labeling fails, a
	// replay finds the same authority marker instead of posting a duplicate.
	if _, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerRegeneration": state}); err != nil {
		return err
	}
	if err := bridge.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{labels.NeedsHuman}, CWD: cwd}); err != nil {
		return fmt.Errorf("label human escalation: %w", err)
	}
	state.Escalated = true
	_, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerRegeneration": state})
	return err
}

// escalateWithoutFailureContext completes a human escalation after the content
// safety gate rejected the recorded failure context. The fallback body is built
// from daemon-composed text only, so it cannot inherit the rejected content,
// and it keeps the authority marker a replay deduplicates on. If the fallback
// is rejected too, the comment is omitted and the needs-human label still
// lands: a content rejection defers the explanation, never the escalation
// itself. Any other failure (forge outage, transport error) is returned so the
// run is retried instead of silently dropping the comment.
func (r *Runner) escalateWithoutFailureContext(ctx context.Context, loop storage.LoopRecord, state fixerRegenerationState, prNumber int64, repo, cwd, reason string) (fixerRegenerationState, error) {
	fallback := fmt.Sprintf("%s\n\nFixer stopped automatic close-and-regenerate because: %s\nThe PR remains open for human review.\n\nFailure context withheld: the outbound content safety gate rejected it. Inspect the loop's local metadata for the recorded failure details.", regenerationCommentMarker+" authority="+state.Authority+" outcome=escalated -->", reason)
	comment, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: fallback, CWD: cwd, DisclosureAgent: r.agentRuntime, DisclosureModel: derefString(r.agentModel)})
	if err == nil {
		state.CommentID, state.Commented = comment.ID, true
		return state, nil
	}
	if !outboundguard.IsRejection(err) {
		return state, fmt.Errorf("post withheld-context escalation comment: %w", err)
	}
	r.logWarn("fixer escalation comment withheld by content safety gate", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber, "error": err.Error()})
	// The event is both the audit evidence for the withheld context and the
	// durable marker that settles this exact escalation comment step. A
	// deterministic target-scoped primary key makes replay and concurrent
	// recovery idempotent even when the metadata checkpoint after this append
	// fails. If the event cannot be
	// recorded, do not advance to the label: the next retry must not infer that
	// the comment step was settled without durable evidence.
	if err := r.appendContextWithheldEvent(ctx, loop, repo, prNumber, reason); err != nil {
		return state, err
	}
	return state, nil
}

func contextWithheldEventID(loopID, repo string, prNumber int64, reason string) string {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return ""
	}
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%s", loopID, strings.TrimSpace(repo), prNumber, strings.TrimSpace(reason))
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s:%s:%x", contextWithheldEventType, loopID, digest[:])
}

func legacyContextWithheldEventID(loopID string) string {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return ""
	}
	return contextWithheldEventType + ":" + loopID
}

func contextWithheldEventMatches(event storage.EventLogRecord, repo string, prNumber int64, reason string) bool {
	var payload struct {
		Repo     string `json:"repo"`
		PRNumber int64  `json:"prNumber"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload.Repo) == strings.TrimSpace(repo) && payload.PRNumber == prNumber && strings.TrimSpace(payload.Reason) == strings.TrimSpace(reason)
}

func (r *Runner) hasContextWithheldEvent(ctx context.Context, loopID, repo string, prNumber int64, reason string) (bool, error) {
	eventID := contextWithheldEventID(loopID, repo, prNumber, reason)
	if eventID == "" || r.repos == nil || r.repos.Events == nil {
		return false, nil
	}
	events, err := r.repos.Events.ListByEntityAndEventTypes(ctx, "loop", strings.TrimSpace(loopID), []string{contextWithheldEventType})
	if err != nil {
		return false, err
	}
	legacyEventID := legacyContextWithheldEventID(loopID)
	for _, event := range events {
		if (event.ID == eventID || event.ID == legacyEventID) && contextWithheldEventMatches(event, repo, prNumber, reason) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) appendContextWithheldEvent(ctx context.Context, loop storage.LoopRecord, repo string, prNumber int64, reason string) error {
	eventID := contextWithheldEventID(loop.ID, repo, prNumber, reason)
	if eventID == "" || r.repos == nil || r.repos.Events == nil {
		return nil
	}
	err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		ID:         eventID,
		EventType:  contextWithheldEventType,
		ProjectID:  runpipe.OptionalString(loop.ProjectID),
		LoopID:     runpipe.OptionalString(loop.ID),
		EntityType: runpipe.OptionalString("loop"),
		EntityID:   runpipe.OptionalString(loop.ID),
		ActorType:  runpipe.OptionalString("system"),
		ActorID:    runpipe.OptionalString("fixer-loop"),
		Payload:    map[string]any{"repo": strings.TrimSpace(repo), "prNumber": prNumber, "reason": strings.TrimSpace(reason)},
		CreatedAt:  r.now(),
	})
	if err == nil {
		return nil
	}
	// A concurrent replay may have inserted the deterministic record first.
	// Verify that case rather than treating the unique-key collision as a
	// failed escalation; all other append failures remain retryable.
	recorded, lookupErr := r.hasContextWithheldEvent(ctx, loop.ID, repo, prNumber, reason)
	if lookupErr == nil && recorded {
		return nil
	}
	if lookupErr != nil {
		return fmt.Errorf("record withheld-context escalation evidence: %w (verify existing evidence: %v)", err, lookupErr)
	}
	return fmt.Errorf("record withheld-context escalation evidence: %w", err)
}

func regenerationCommentWithheldEventID(loopID, repo string, prNumber int64, authority string) string {
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return ""
	}
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%s", loopID, strings.TrimSpace(repo), prNumber, strings.TrimSpace(authority))
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s:%s:%x", regenerationCommentWithheldEventType, loopID, digest[:])
}

func regenerationCommentWithheldEventMatches(event storage.EventLogRecord, repo string, prNumber int64, authority string) bool {
	var payload struct {
		Repo      string `json:"repo"`
		PRNumber  int64  `json:"prNumber"`
		Authority string `json:"authority"`
	}
	if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload.Repo) == strings.TrimSpace(repo) && payload.PRNumber == prNumber && strings.TrimSpace(payload.Authority) == strings.TrimSpace(authority)
}

func (r *Runner) hasRegenerationCommentWithheldEvent(ctx context.Context, loopID, repo string, prNumber int64, authority string) (bool, error) {
	eventID := regenerationCommentWithheldEventID(loopID, repo, prNumber, authority)
	if eventID == "" || r.repos == nil || r.repos.Events == nil {
		return false, nil
	}
	events, err := r.repos.Events.ListByEntityAndEventTypes(ctx, "loop", strings.TrimSpace(loopID), []string{regenerationCommentWithheldEventType})
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.ID == eventID && regenerationCommentWithheldEventMatches(event, repo, prNumber, authority) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) appendRegenerationCommentWithheldEvent(ctx context.Context, loop storage.LoopRecord, repo string, prNumber int64, authority string) error {
	eventID := regenerationCommentWithheldEventID(loop.ID, repo, prNumber, authority)
	if eventID == "" || r.repos == nil || r.repos.Events == nil {
		return nil
	}
	err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		ID:         eventID,
		EventType:  regenerationCommentWithheldEventType,
		ProjectID:  runpipe.OptionalString(loop.ProjectID),
		LoopID:     runpipe.OptionalString(loop.ID),
		EntityType: runpipe.OptionalString("loop"),
		EntityID:   runpipe.OptionalString(loop.ID),
		ActorType:  runpipe.OptionalString("system"),
		ActorID:    runpipe.OptionalString("fixer-loop"),
		Payload:    map[string]any{"repo": strings.TrimSpace(repo), "prNumber": prNumber, "authority": strings.TrimSpace(authority)},
		CreatedAt:  r.now(),
	})
	if err == nil {
		return nil
	}
	// The event ID is a durable uniqueness key. A concurrent replay may have
	// inserted the same evidence first; accept that exact record and retry any
	// other storage failure instead of treating the optional comment as settled.
	recorded, lookupErr := r.hasRegenerationCommentWithheldEvent(ctx, loop.ID, repo, prNumber, authority)
	if lookupErr == nil && recorded {
		return nil
	}
	if lookupErr != nil {
		return fmt.Errorf("record withheld regeneration comment evidence: %w (verify existing evidence: %v)", err, lookupErr)
	}
	return fmt.Errorf("record withheld regeneration comment evidence: %w", err)
}

func (r *Runner) persistRegenerationAbort(ctx context.Context, loop storage.LoopRecord, state fixerRegenerationState, reason string) (regenerationAction, error) {
	state.Escalated = true
	state.EscalationWhy = reason
	if _, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerRegeneration": state}); err != nil {
		return regenerationNone, err
	}
	return regenerationEscalated, nil
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

func regenerationFailureContext(loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint fixerCheckpoint, failure *runpipe.LoopError) string {
	return fmt.Sprintf("attempts=%d/%d; failure=%s; kind=%s; headSha=%s; fixItemsFingerprint=%s; loop=%s; queueItem=%s", queueItem.Attempts, queueItem.MaxAttempts, strings.TrimSpace(failure.Message), failure.Kind, detailHeadSHA(checkpoint.Detail), hashFixItemsState(checkpoint.FixItems), loop.ID, queueItem.ID)
}

func buildRegenerationComment(authority string, queueItem storage.QueueItemRecord, loop storage.LoopRecord, checkpoint fixerCheckpoint, failure *runpipe.LoopError, issueRepo string, issueNumber int64, issueURL string) string {
	issueRef := ""
	if strings.TrimSpace(issueURL) != "" {
		issueRef = " (" + strings.TrimSpace(issueURL) + ")"
	}
	return fmt.Sprintf("%s authority=%s -->\n\nFixer exhausted its retry budget and will close this PR before returning the originating issue to Planner.\n\n- Attempts: %d/%d\n- Failure: %s\n- Failure kind: %s\n- Head fingerprint: `%s`\n- Fix-items fingerprint: `%s`\n- Originating issue: %s#%d%s\n- Loop: `%s`", regenerationCommentMarker, authority, queueItem.Attempts, queueItem.MaxAttempts, strings.TrimSpace(failure.Message), failure.Kind, detailHeadSHA(checkpoint.Detail), hashFixItemsState(checkpoint.FixItems), issueRepo, issueNumber, issueRef, loop.ID)
}

// buildRegenerationCommentWithoutFailureContext keeps the normal close-and-
// regenerate path useful when the original body is rejected by the outbound
// safety gate. It carries only daemon-composed fingerprints and routing
// identity; the durable failure context remains in loop metadata.
func buildRegenerationCommentWithoutFailureContext(authority string, queueItem storage.QueueItemRecord, loop storage.LoopRecord, checkpoint fixerCheckpoint, issueRepo string, issueNumber int64, issueURL string) string {
	issueRef := ""
	if strings.TrimSpace(issueURL) != "" {
		issueRef = fmt.Sprintf(" (%s)", strings.TrimSpace(issueURL))
	}
	return fmt.Sprintf("%s authority=%s -->\n\nFixer exhausted its retry budget and will close this PR before returning the originating issue to Planner.\n\n- Attempts: %d/%d\n- Failure context withheld: the outbound content safety gate rejected the detailed body; inspect the loop's local metadata for the recorded failure.\n- Head fingerprint: `%s`\n- Fix-items fingerprint: `%s`\n- Originating issue: %s#%d%s\n- Loop: `%s`", regenerationCommentMarker, authority, queueItem.Attempts, queueItem.MaxAttempts, detailHeadSHA(checkpoint.Detail), hashFixItemsState(checkpoint.FixItems), issueRepo, issueNumber, issueRef, loop.ID)
}

func (r *Runner) markRegeneratedLoopFailed(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	return r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "failed"
		updated.LastRunAt = runpipe.StringPtr(r.nowISO())
		updated.NextRunAt = nil
	})
}

func (r *Runner) requeueRegenerationHandoff(ctx context.Context, loop storage.LoopRecord, queueItem storage.QueueItemRecord) error {
	if r.repos == nil || r.repos.Queue == nil || strings.TrimSpace(loop.ID) == "" || strings.TrimSpace(queueItem.ID) == "" {
		return nil
	}
	if durable, err := r.repos.Queue.GetByID(ctx, queueItem.ID); err != nil {
		return err
	} else if durable != nil {
		queueItem = *durable
	}
	queuedAt := r.nowISO()
	if r.retryBaseDelay > 0 {
		queuedAt = eventlog.FormatJavaScriptISOString(r.now().Add(r.retryBaseDelay).UTC())
	}
	var err error
	requeued := false
	switch queueItem.Status {
	case "failed", "manual_intervention":
		var affected int64
		affected, err = r.repos.Queue.RequeueFailedByIDWithAttempts(ctx, loop.ID, queueItem.ID, queuedAt, queueItem.Attempts)
		requeued = affected > 0
	case "running":
		message := "fixer regeneration handoff requires replay"
		err = r.repos.Queue.MarkRetryIfRunning(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: queuedAt, Attempts: queueItem.Attempts, ErrorMessage: &message, ErrorKind: "fixer_regeneration", UpdatedAt: r.nowISO()})
		if err == nil {
			if persisted, getErr := r.repos.Queue.GetByID(ctx, queueItem.ID); getErr != nil {
				err = getErr
			} else {
				requeued = persisted != nil && persisted.Status == "queued"
			}
		}
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue fixer regeneration replay: %w", err)
	}
	// The normal scheduler excludes paused/failed loops from its claim query.
	// Make this durable replay row claimable while preserving the regeneration
	// ledger; the replay path marks the loop failed again after the suffix is
	// complete.
	if !requeued {
		return nil
	}
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		if updated.Status != "terminated" && updated.Status != string(domain.LoopStatusHumanTakeover) {
			updated.Status = "queued"
			updated.NextRunAt = runpipe.StringPtr(queuedAt)
		}
	}); err != nil {
		return fmt.Errorf("queue fixer regeneration replay loop: %w", err)
	}
	if r.onQueueItemEnqueued != nil {
		r.onQueueItemEnqueued()
	}
	return nil
}

func (r *Runner) applyTerminalRegeneration(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint fixerCheckpoint, failure *runpipe.LoopError) (storage.LoopRecord, regenerationAction, error) {
	action, err := r.handleTerminalExhaustion(ctx, project, loop, queueItem, checkpoint, failure)
	if err != nil {
		if replayErr := r.requeueRegenerationHandoff(ctx, loop, queueItem); replayErr != nil {
			return loop, regenerationNone, &regenerationHandoffError{cause: fmt.Errorf("%w; replay queue: %v", err, replayErr), checkpoint: checkpoint, failure: failure}
		}
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
