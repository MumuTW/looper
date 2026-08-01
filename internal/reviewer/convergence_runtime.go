package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/reviewer/convergence"
	"github.com/MumuTW/looper/internal/reviewitem"
	"github.com/MumuTW/looper/internal/storage"
)

// convergenceMetadata is the reviewer loop's durable semantic progress. It
// lives in the existing loop metadata instead of a second ledger: the review
// item marker remains the forge authority, while this record is only the
// replayable observation history and decision cache.
type convergenceMetadata struct {
	Policy    convergence.Policy `json:"policy"`
	State     convergence.State  `json:"state"`
	Action    convergence.Action `json:"action,omitempty"`
	Reason    convergence.Reason `json:"reason,omitempty"`
	Status    string             `json:"status,omitempty"`
	UpdatedAt string             `json:"updatedAt,omitempty"`
}

type reviewerConvergenceAwaitingHumanError struct {
	Decision convergence.Decision
}

func (e *reviewerConvergenceAwaitingHumanError) Error() string {
	return fmt.Sprintf("reviewer convergence escalated: %s", e.Decision.Reason)
}

func (r *Runner) convergencePolicyForProject(projectID string) convergence.Policy {
	cfg := config.DefaultReviewerConvergenceConfig()
	if r.projectRoleConfig != nil {
		roles := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID)
		if roles.Reviewer.Behavior.Convergence != nil {
			cfg = *roles.Reviewer.Behavior.Convergence
		}
	} else if r.convergence.MaxConsecutiveUnproductive > 0 || r.convergence.MaxFixerAttemptsPerItem > 0 || r.convergence.MaxTotalRounds > 0 || r.convergence.SeverityFloor != "" {
		cfg = r.convergence
	}
	policy := convergence.Policy{
		MaxConsecutiveUnproductive: cfg.MaxConsecutiveUnproductive,
		MaxFixerAttemptsPerItem:    cfg.MaxFixerAttemptsPerItem,
		MaxTotalRounds:             cfg.MaxTotalRounds,
		SeverityFloor:              convergence.SeverityFloor(cfg.SeverityFloor),
	}
	if policy.MaxConsecutiveUnproductive == 0 {
		policy.MaxConsecutiveUnproductive = 3
	}
	if policy.MaxFixerAttemptsPerItem == 0 {
		policy.MaxFixerAttemptsPerItem = 4
	}
	if policy.MaxTotalRounds == 0 {
		policy.MaxTotalRounds = 40
	}
	if policy.SeverityFloor == "" {
		policy.SeverityFloor = convergence.SeverityFloorNonBlocking
	}
	return policy
}

func convergenceItemsFromDetail(detail PullRequestDetail) convergence.Round {
	items := make([]convergence.Item, 0, len(detail.Comments))
	for index, raw := range detail.Comments {
		id, _ := stringFromAny(raw["id"])
		if id == "" {
			id, _ = stringFromAny(raw["threadId"])
		}
		if id == "" {
			id = fmt.Sprintf("comment-%d", index)
		}
		severityText, _ := stringFromAny(raw["severity"])
		if severityText == "" {
			body, _ := stringFromAny(raw["body"])
			if severity, ok := reviewitem.SeverityFromBody(body); ok {
				severityText = string(severity)
			}
		}
		severity, err := reviewitem.ParseSeverity(severityText)
		if err != nil {
			// Legacy or malformed comments are still visible to the human and
			// Fixer, but cannot become convergence authority without a trusted
			// structured severity marker.
			continue
		}
		status := convergence.ItemStatusOpen
		state, _ := stringFromAny(raw["state"])
		if resolved, ok := raw["isResolved"].(bool); ok && resolved {
			status = convergence.ItemStatusResolved
		} else {
			switch strings.ToLower(strings.TrimSpace(state)) {
			case "resolved", "true":
				status = convergence.ItemStatusResolved
			case "superseded":
				status = convergence.ItemStatusSuperseded
			case "deferred":
				status = convergence.ItemStatusDeferred
			}
		}
		resultText, _ := stringFromAny(raw["fixerResult"])
		var result convergence.FixerResult
		switch strings.ToLower(strings.TrimSpace(resultText)) {
		case string(convergence.FixerResultFixed):
			result = convergence.FixerResultFixed
		case string(convergence.FixerResultDeclined):
			result = convergence.FixerResultDeclined
		case string(convergence.FixerResultDeferred):
			result = convergence.FixerResultDeferred
		}
		attemptKey, _ := stringFromAny(raw["fixerAttemptKey"])
		items = append(items, convergence.Item{ID: id, Severity: severity, Status: status, FixerResult: result, FixerAttemptKey: attemptKey})
	}
	return convergence.Round{Items: items}
}

func parseConvergenceMetadata(meta map[string]any) (convergenceMetadata, bool) {
	raw, ok := meta["convergence"]
	if !ok {
		return convergenceMetadata{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return convergenceMetadata{}, false
	}
	var state convergenceMetadata
	if err := json.Unmarshal(encoded, &state); err != nil {
		return convergenceMetadata{}, false
	}
	return state, true
}

func writeConvergenceMetadata(meta map[string]any, state convergenceMetadata) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return err
	}
	meta["convergence"] = raw
	return nil
}

func (r *Runner) recordConvergenceProgress(ctx context.Context, input stepInput, detail PullRequestDetail) error {
	policy := r.convergencePolicyForProject(input.Project.ID)
	round := convergenceItemsFromDetail(detail)
	var decision convergence.Decision
	var writeErr error
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		meta, err := loops.DecodeMetadataObjectForWrite(updated.MetadataJSON)
		if err != nil {
			writeErr = err
			return
		}
		persisted, ok := parseConvergenceMetadata(meta)
		if !ok || persisted.Policy.MaxTotalRounds == 0 {
			persisted.Policy = policy
		}
		decision, err = convergence.Evaluate(persisted.State, round, persisted.Policy)
		if err != nil {
			writeErr = err
			return
		}
		persisted.State = decision.State
		persisted.Action = decision.Action
		persisted.Reason = decision.Reason
		persisted.UpdatedAt = r.nowISO()
		switch decision.Action {
		case convergence.ActionEscalate:
			persisted.Status = "awaiting_human"
		default:
			persisted.Status = "active"
		}
		if decision.Action == convergence.ActionComplete {
			// A clean severity-floor decision stops rediscovery for this head,
			// but the existing reviewer loop remains waiting so a later PR head
			// can still re-enter the loop without changing its lifecycle status.
			persisted.Status = "active"
		}
		writeErr = writeConvergenceMetadata(meta, persisted)
		if writeErr != nil {
			return
		}
		encoded, marshalErr := json.Marshal(meta)
		if marshalErr != nil {
			writeErr = marshalErr
			return
		}
		updated.MetadataJSON = stringPtr(string(encoded))
	}); err != nil {
		return err
	}
	if writeErr != nil {
		return writeErr
	}
	if decision.Action == convergence.ActionEscalate {
		return &reviewerConvergenceAwaitingHumanError{Decision: decision}
	}
	return nil
}

func (r *Runner) recordPublishedReviewAndConvergence(ctx context.Context, input stepInput, pending pendingReviewCheckpoint, reviewEvent ReviewEvent, detail PullRequestDetail) error {
	if err := r.recordPublishedReviewProgress(ctx, input, pending, reviewEvent); err != nil {
		return err
	}
	return r.recordConvergenceProgress(ctx, input, detail)
}

func (r *Runner) handleConvergenceHumanAnswer(ctx context.Context, input stepInput, checkpoint *reviewerCheckpoint) (bool, error) {
	meta := parseJSONObject(input.Loop.MetadataJSON)
	persisted, ok := parseConvergenceMetadata(meta)
	if !ok || persisted.Status != "awaiting_human" {
		return false, nil
	}
	ask, ok := loops.ReadHITLAsk(input.Loop.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return false, &reviewerConvergenceAwaitingHumanError{Decision: convergence.Decision{Action: convergence.ActionEscalate, Reason: persisted.Reason, State: persisted.State}}
	}
	answer := strings.ToLower(strings.TrimSpace(ask.Answer))
	switch answer {
	case "resume", "continue", "retry":
		persisted.Status = "active"
		persisted.Action = ""
		persisted.Reason = convergence.ReasonConverging
		persisted.State.ConsecutiveUnproductive = 0
		persisted.UpdatedAt = r.nowISO()
		checkpoint.SkipReason = "Reviewer convergence loop resumed by human decision"
	case "close", "stop", "terminate":
		persisted.Status = "completed"
		persisted.Action = convergence.ActionComplete
		persisted.Reason = convergence.ReasonSeverityFloorReached
		persisted.UpdatedAt = r.nowISO()
		checkpoint.SkipReason = "Reviewer convergence loop closed by human decision"
	default:
		return false, &reviewerConvergenceAwaitingHumanError{Decision: convergence.Decision{Action: convergence.ActionEscalate, Reason: persisted.Reason, State: persisted.State}}
	}
	ask.Status = "consumed"
	var writeErr error
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		current, err := loops.DecodeMetadataObjectForWrite(updated.MetadataJSON)
		if err != nil {
			writeErr = err
			return
		}
		if err := writeConvergenceMetadata(current, persisted); err != nil {
			writeErr = err
			return
		}
		currentJSONBytes, err := json.Marshal(current)
		if err != nil {
			writeErr = err
			return
		}
		currentJSON, err := loops.WriteHITLAsk(stringPtr(string(currentJSONBytes)), ask)
		if err != nil {
			writeErr = err
			return
		}
		var currentMeta map[string]any
		if err := json.Unmarshal([]byte(currentJSON), &currentMeta); err != nil {
			writeErr = err
			return
		}
		encoded, err := json.Marshal(currentMeta)
		if err != nil {
			writeErr = err
			return
		}
		updated.MetadataJSON = stringPtr(string(encoded))
	}); err != nil {
		return false, err
	}
	if writeErr != nil {
		return false, writeErr
	}
	return true, nil
}

func (r *Runner) suspendReviewerForConvergence(ctx context.Context, loop storage.LoopRecord, run storage.RunRecord, queue storage.QueueItemRecord, checkpoint reviewerCheckpoint, awaiting *reviewerConvergenceAwaitingHumanError) (ProcessResult, error) {
	decision := awaiting.Decision
	question := fmt.Sprintf("Reviewer convergence escalated (%s). Continue the loop or close this PR review?", decision.Reason)
	recommendation := fmt.Sprintf("round=%d, consecutive_unproductive=%d, open_items=%s", decision.State.TotalRounds, decision.State.ConsecutiveUnproductive, strings.Join(openConvergenceItemIDs(decision.State), ", "))
	ask := loops.HITLAsk{Question: question, Options: []string{"resume", "close"}, Status: "awaiting", AskedAt: r.nowISO(), Recommendation: recommendation, RecommendedOption: "resume", Consequences: map[string]string{"resume": "Reset the stall counter and continue on the next reviewer/fixer round.", "close": "Stop the reviewer loop and leave the current PR for human handling."}, Confidence: "high"}
	checkpoint.ResumePolicy = loops.ResumePolicyReplayStep
	var writeErr error
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		metadataJSON, err := loops.WriteHITLAsk(updated.MetadataJSON, ask)
		if err != nil {
			writeErr = err
			return
		}
		updated.MetadataJSON = stringPtr(metadataJSON)
		updated.Status = "awaiting_human"
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	if writeErr != nil {
		return ProcessResult{}, writeErr
	}
	reason := "reviewer convergence escalated"
	if _, err := r.repos.Queue.CancelByLoop(ctx, loop.ID, r.nowISO(), &reason); err != nil {
		return ProcessResult{}, err
	}
	summary := "Awaiting human decision: " + question
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "reviewer.convergence.escalated", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "pull_request", entityID: fmt.Sprintf("%s#%d", derefString(loop.Repo), derefInt64(loop.PRNumber)), payload: map[string]any{"reason": string(decision.Reason), "round": decision.State.TotalRounds, "openItemIds": openConvergenceItemIDs(decision.State)}})
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queue.ID, Status: "awaiting_human", Summary: summary}, nil
}

func openConvergenceItemIDs(state convergence.State) []string {
	ids := make([]string, 0)
	for id, item := range state.Items {
		if item.Status == convergence.ItemStatusOpen && !item.Stuck {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (r *Runner) convergenceLoopStatus(metadataJSON *string) string {
	meta := parseJSONObject(metadataJSON)
	persisted, ok := parseConvergenceMetadata(meta)
	if !ok {
		return ""
	}
	switch persisted.Status {
	case "completed":
		return "completed"
	case "awaiting_human":
		return "awaiting_human"
	default:
		return ""
	}
}

func (r *Runner) snapshotConvergencePolicy(meta map[string]any, projectID string) error {
	if _, exists := meta["convergence"]; exists {
		return nil
	}
	return writeConvergenceMetadata(meta, convergenceMetadata{Policy: r.convergencePolicyForProject(projectID), Status: "active", UpdatedAt: r.nowISO()})
}
