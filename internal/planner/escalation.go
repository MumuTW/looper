package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

const plannerAssessmentRelPath = ".looper/planner-assessment.json"
const maxPlannerAssessmentBytes = 64 << 10

type plannerAssessmentEvidence struct {
	EstimatedFiles     int      `json:"estimatedFiles"`
	EstimatedPackages  int      `json:"estimatedPackages"`
	Surfaces           []string `json:"surfaces,omitempty"`
	ADRConflicts       []string `json:"adrConflicts,omitempty"`
	AuthorityDecisions []string `json:"authorityDecisions,omitempty"`
	Evidence           []string `json:"evidence,omitempty"`
	DecisionRequest    string   `json:"decisionRequest,omitempty"`
}

type plannerEscalationCriterion struct {
	Criterion string   `json:"criterion"`
	Evidence  []string `json:"evidence"`
}

type checkpointAssessment struct {
	Evidence      plannerAssessmentEvidence    `json:"evidence"`
	Fired         []plannerEscalationCriterion `json:"fired,omitempty"`
	Disposition   string                       `json:"disposition,omitempty"`
	HumanAnswer   string                       `json:"humanAnswer,omitempty"`
	AssessedAt    string                       `json:"assessedAt"`
	DispositionAt string                       `json:"dispositionAt,omitempty"`
}

type plannerAwaitingHumanError struct {
	question string
	fired    []plannerEscalationCriterion
}

func (e *plannerAwaitingHumanError) Error() string {
	return "planner is awaiting human authority: " + e.question
}

func plannerEscalationEnabled(policy config.PlannerEscalationConfig) bool {
	return policy.MaxEstimatedFiles > 0 || policy.MaxEstimatedPackages > 0 || policy.PublicAPI || policy.Config || policy.CLI || policy.Storage || policy.WireFormat || policy.ADRConflict || policy.AuthorityDecision
}

func (r *Runner) plannerEscalationPolicy(projectID string) config.PlannerEscalationConfig {
	if r.projectRoleConfig == nil {
		return config.PlannerEscalationConfig{}
	}
	policy := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID).Planner.Escalation
	if policy == nil {
		return config.PlannerEscalationConfig{}
	}
	return *policy
}

func (r *Runner) runAssessSuitabilityStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	policy := r.plannerEscalationPolicy(input.Project.ID)
	if !plannerEscalationEnabled(policy) {
		return checkpoint, nil
	}
	if checkpoint.Assessment != nil {
		if len(checkpoint.Assessment.Fired) == 0 || checkpoint.Assessment.Disposition == "authorized" || checkpoint.Assessment.Disposition == "closed" {
			return checkpoint, nil
		}
		ask, ok := loops.ReadHITLAsk(input.Loop.MetadataJSON)
		if ok && ask.Status == "answered" {
			answer := strings.TrimSpace(ask.Answer)
			checkpoint.Assessment.HumanAnswer = answer
			checkpoint.Assessment.DispositionAt = r.nowISO()
			switch normalizePlannerEscalationAnswer(answer) {
			case "closed":
				checkpoint.Assessment.Disposition = "closed"
				checkpoint.SkipReason = "Planner closed by human decision before spec authoring"
			case "authorized":
				checkpoint.Assessment.Disposition = "authorized"
			default:
				return checkpoint, &plannerAwaitingHumanError{question: "Reply with exactly `authorize proceed` or `close without a spec`.", fired: checkpoint.Assessment.Fired}
			}
			checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
			_ = r.markPlannerAskConsumed(ctx, input.Loop, ask)
			return checkpoint, nil
		}
		return checkpoint, &plannerAwaitingHumanError{question: checkpoint.Assessment.Evidence.DecisionRequest, fired: checkpoint.Assessment.Fired}
	}

	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	prompt := buildPlannerAssessmentPrompt(issue, policy)
	assessmentPath := filepath.Join(worktree.Path, plannerAssessmentRelPath)
	if err := os.Remove(assessmentPath); err != nil && !os.IsNotExist(err) {
		return checkpoint, fmt.Errorf("remove stale planner assessment output: %w", err)
	}
	executionID := eventlog.NewEventID("agent")
	vendor, model, _, useSnapshot, err := r.identityFromRun(input.Run)
	if err != nil {
		return checkpoint, err
	}
	useSnap, snapVendor, snapModel := agentRunSnapshotFields(vendor, model, useSnapshot)
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{
		ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID,
		Prompt: prompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout,
		Metadata:       map[string]any{"loopType": "planner", "phase": "suitability-assessment", "repo": issue.Repo, "issueNumber": issue.IssueNumber},
		IdempotencyKey: fmt.Sprintf("planner-assessment:%s", input.Loop.ID), UseSnapshot: useSnap, SnapshotVendor: snapVendor, SnapshotModel: snapModel,
	})
	if err != nil {
		return checkpoint, err
	}
	if r.onAgentExecutionStarted != nil {
		if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", issue.Repo, issue.IssueNumber), Body: fmt.Sprintf("Planner suitability assessment started for %s", issue.Title), DedupeKey: "runtime.agent.started:planner-assessment:" + input.Run.ID}); err != nil && r.logger != nil {
			r.logger.Warn("planner assessment agent start notification failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
		}
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return checkpoint, err
	}
	if !strings.EqualFold(result.Status, "completed") {
		return checkpoint, &loopError{message: firstNonEmpty(result.Summary, result.Stderr, "Planner assessment agent "+result.Status), kind: FailureRetryableTransient}
	}
	evidence, err := consumePlannerAssessment(worktree.Path)
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureNonRetryable}
	}
	if r.git != nil {
		worktreeRoot, rootErr := plannerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		inspect, inspectErr := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: worktree.BaseBranch})
		if inspectErr != nil {
			return checkpoint, inspectErr
		}
		if inspect.HasUncommittedChanges || len(inspect.NewCommitSHAs) > 0 {
			return checkpoint, &loopError{message: "Planner suitability assessment modified the worktree before spec authoring", kind: FailureNonRetryable}
		}
	}
	fired, err := evaluatePlannerEscalation(policy, evidence)
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureNonRetryable}
	}
	checkpoint.Assessment = &checkpointAssessment{Evidence: evidence, Fired: fired, AssessedAt: r.nowISO()}
	if len(fired) == 0 {
		checkpoint.Assessment.Disposition = "not_required"
		checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
		return checkpoint, nil
	}
	checkpoint.Assessment.Disposition = "awaiting_human"
	checkpoint.ResumePolicy = loops.ResumePolicyReplayStep
	if err := r.persistCheckpoint(ctx, input.Run.ID, stepAssessSuitability, checkpoint); err != nil {
		return checkpoint, err
	}
	return checkpoint, &plannerAwaitingHumanError{question: evidence.DecisionRequest, fired: fired}
}

func buildPlannerAssessmentPrompt(issue *checkpointIssue, policy config.PlannerEscalationConfig) string {
	encoded, _ := json.Marshal(policy)
	return fmt.Sprintf(`Explore the repository only to assess whether Issue %s#%d is suitable for autonomous planning. Do not write a spec, edit product files, commit, push, or use remote APIs.

Issue title: %s
Issue body:
%s

The operator-authored escalation policy is: %s

Write exactly one JSON object to %s with this schema:
{"estimatedFiles":0,"estimatedPackages":0,"surfaces":["public_api|config|cli|storage|wire_format"],"adrConflicts":["specific ADR and contradiction"],"authorityDecisions":["specific decision beyond Planner authority"],"evidence":["path or concrete repository fact"],"decisionRequest":"one specific decision a human must make"}
Use empty arrays when absent. Estimates must be non-negative. Only use the listed surface values. The decision request must be concrete when any configured criterion may fire.`, issue.Repo, issue.IssueNumber, issue.Title, issue.Body, string(encoded), plannerAssessmentRelPath)
}

func consumePlannerAssessment(worktree string) (plannerAssessmentEvidence, error) {
	path := filepath.Join(worktree, plannerAssessmentRelPath)
	info, err := os.Lstat(path)
	if err != nil {
		return plannerAssessmentEvidence{}, fmt.Errorf("planner assessment output missing: %w", err)
	}
	if !info.Mode().IsRegular() {
		return plannerAssessmentEvidence{}, fmt.Errorf("planner assessment output must be a regular file")
	}
	if info.Size() > maxPlannerAssessmentBytes {
		return plannerAssessmentEvidence{}, fmt.Errorf("planner assessment output exceeds %d bytes", maxPlannerAssessmentBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return plannerAssessmentEvidence{}, fmt.Errorf("planner assessment output missing: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return plannerAssessmentEvidence{}, fmt.Errorf("consume planner assessment output: %w", err)
	}
	var evidence plannerAssessmentEvidence
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&evidence); err != nil {
		return plannerAssessmentEvidence{}, fmt.Errorf("invalid planner assessment output: %w", err)
	}
	if evidence.EstimatedFiles < 0 || evidence.EstimatedPackages < 0 {
		return plannerAssessmentEvidence{}, fmt.Errorf("planner assessment estimates must be non-negative")
	}
	allowed := map[string]bool{"public_api": true, "config": true, "cli": true, "storage": true, "wire_format": true}
	seen := map[string]bool{}
	canonical := make([]string, 0, len(evidence.Surfaces))
	for _, surface := range evidence.Surfaces {
		surface = strings.ToLower(strings.TrimSpace(surface))
		if !allowed[surface] {
			return plannerAssessmentEvidence{}, fmt.Errorf("planner assessment has unsupported surface %q", surface)
		}
		if !seen[surface] {
			canonical = append(canonical, surface)
			seen[surface] = true
		}
	}
	evidence.Surfaces = canonical
	return evidence, nil
}

func evaluatePlannerEscalation(policy config.PlannerEscalationConfig, evidence plannerAssessmentEvidence) ([]plannerEscalationCriterion, error) {
	var fired []plannerEscalationCriterion
	add := func(name string, facts ...string) {
		fired = append(fired, plannerEscalationCriterion{Criterion: name, Evidence: facts})
	}
	if policy.MaxEstimatedFiles > 0 && evidence.EstimatedFiles > policy.MaxEstimatedFiles {
		add("blast_radius_files", fmt.Sprintf("estimated %d files exceeds configured maximum %d", evidence.EstimatedFiles, policy.MaxEstimatedFiles))
	}
	if policy.MaxEstimatedPackages > 0 && evidence.EstimatedPackages > policy.MaxEstimatedPackages {
		add("blast_radius_packages", fmt.Sprintf("estimated %d packages exceeds configured maximum %d", evidence.EstimatedPackages, policy.MaxEstimatedPackages))
	}
	surfacePolicy := map[string]bool{"public_api": policy.PublicAPI, "config": policy.Config, "cli": policy.CLI, "storage": policy.Storage, "wire_format": policy.WireFormat}
	for _, surface := range evidence.Surfaces {
		if surfacePolicy[surface] {
			add("surface_"+surface, "assessment identifies "+surface+" change")
		}
	}
	if policy.ADRConflict && len(evidence.ADRConflicts) > 0 {
		add("adr_conflict", evidence.ADRConflicts...)
	}
	if policy.AuthorityDecision && len(evidence.AuthorityDecisions) > 0 {
		add("authority_decision", evidence.AuthorityDecisions...)
	}
	if len(fired) > 0 && strings.TrimSpace(evidence.DecisionRequest) == "" {
		return nil, fmt.Errorf("planner assessment fired escalation criteria without a specific decisionRequest")
	}
	sort.Slice(fired, func(i, j int) bool { return fired[i].Criterion < fired[j].Criterion })
	return fired, nil
}

func normalizePlannerEscalationAnswer(answer string) string {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "authorize proceed", "proceed", "authorize", "yes":
		return "authorized"
	case "close without a spec", "close", "reject", "do not proceed":
		return "closed"
	default:
		return ""
	}
}

func (r *Runner) suspendPlannerForHuman(ctx context.Context, loop storage.LoopRecord, run storage.RunRecord, queue storage.QueueItemRecord, checkpoint plannerCheckpoint, awaiting *plannerAwaitingHumanError) (ProcessResult, error) {
	nowISO := r.nowISO()
	criteria := make([]string, 0, len(awaiting.fired))
	consequences := map[string]string{"authorize proceed": "Planner writes and publishes the spec from the persisted assessment.", "close without a spec": "Planner completes this loop without writing or publishing a spec."}
	for _, item := range awaiting.fired {
		criteria = append(criteria, item.Criterion+": "+strings.Join(item.Evidence, "; "))
	}
	ask := loops.HITLAsk{Question: awaiting.question, Options: []string{"authorize proceed", "close without a spec"}, Status: "awaiting", AskedAt: nowISO, Recommendation: strings.Join(criteria, "\n"), Consequences: consequences}
	var writeErr error
	_, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask)
		if werr != nil {
			writeErr = werr
			return
		}
		updated.MetadataJSON = &meta
		updated.Status = "awaiting_human"
		updated.LastRunAt = &nowISO
		updated.NextRunAt = nil
	})
	if err != nil {
		return ProcessResult{}, err
	}
	if writeErr != nil {
		return ProcessResult{}, writeErr
	}
	reason := "planner suspended awaiting human authority"
	if _, err := r.repos.Queue.CancelByLoop(ctx, loop.ID, nowISO, &reason); err != nil {
		return ProcessResult{}, err
	}
	summary := "Awaiting human decision: " + awaiting.question
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "planner.awaiting_human", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"criteria": criteria, "decisionRequest": awaiting.question}})
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queue.ID, Status: "awaiting_human", Summary: summary}, nil
}

func (r *Runner) markPlannerAskConsumed(ctx context.Context, loop storage.LoopRecord, ask loops.HITLAsk) error {
	ask.Status = "consumed"
	var writeErr error
	_, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask)
		if werr != nil {
			writeErr = werr
			return
		}
		updated.MetadataJSON = &meta
	})
	if err == nil {
		err = writeErr
	}
	return err
}
