package planner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

type assessmentFileExecutor struct{ content string }

func (e assessmentFileExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	return assessmentFileExecution{path: filepath.Join(input.WorkingDirectory, plannerAssessmentRelPath), content: e.content}, nil
}

type assessmentFileExecution struct{ path, content string }

func (e assessmentFileExecution) Wait(context.Context) (AgentResult, error) {
	if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
		return AgentResult{}, err
	}
	if err := os.WriteFile(e.path, []byte(e.content), 0o600); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Status: "completed", Summary: "assessment complete"}, nil
}

func TestEvaluatePlannerEscalationCriteria(t *testing.T) {
	tests := []struct {
		name     string
		policy   config.PlannerEscalationConfig
		evidence plannerAssessmentEvidence
		want     string
	}{
		{"files", config.PlannerEscalationConfig{MaxEstimatedFiles: 2}, plannerAssessmentEvidence{EstimatedFiles: 3}, "blast_radius_files"},
		{"packages", config.PlannerEscalationConfig{MaxEstimatedPackages: 1}, plannerAssessmentEvidence{EstimatedPackages: 2}, "blast_radius_packages"},
		{"public api", config.PlannerEscalationConfig{PublicAPI: true}, plannerAssessmentEvidence{Surfaces: []string{"public_api"}}, "surface_public_api"},
		{"config", config.PlannerEscalationConfig{Config: true}, plannerAssessmentEvidence{Surfaces: []string{"config"}}, "surface_config"},
		{"cli", config.PlannerEscalationConfig{CLI: true}, plannerAssessmentEvidence{Surfaces: []string{"cli"}}, "surface_cli"},
		{"storage", config.PlannerEscalationConfig{Storage: true}, plannerAssessmentEvidence{Surfaces: []string{"storage"}}, "surface_storage"},
		{"wire format", config.PlannerEscalationConfig{WireFormat: true}, plannerAssessmentEvidence{Surfaces: []string{"wire_format"}}, "surface_wire_format"},
		{"adr", config.PlannerEscalationConfig{ADRConflict: true}, plannerAssessmentEvidence{ADRConflicts: []string{"ADR-12 conflicts"}}, "adr_conflict"},
		{"authority", config.PlannerEscalationConfig{AuthorityDecision: true}, plannerAssessmentEvidence{AuthorityDecisions: []string{"choose incompatible API"}}, "authority_decision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.evidence.DecisionRequest = "Choose whether Planner may proceed"
			got, err := evaluatePlannerEscalation(test.policy, test.evidence)
			if err != nil {
				t.Fatalf("evaluatePlannerEscalation() error = %v", err)
			}
			if len(got) != 1 || got[0].Criterion != test.want {
				t.Fatalf("criteria = %#v, want %q", got, test.want)
			}
		})
	}
}

func TestEvaluatePlannerEscalationDoesNotFireAtThreshold(t *testing.T) {
	got, err := evaluatePlannerEscalation(config.PlannerEscalationConfig{MaxEstimatedFiles: 3, PublicAPI: false}, plannerAssessmentEvidence{EstimatedFiles: 3, Surfaces: []string{"public_api"}})
	if err != nil || len(got) != 0 {
		t.Fatalf("criteria = %#v, err = %v, want none", got, err)
	}
}

func TestRunAssessSuitabilityReplaysPersistedHumanAnswerWithoutAgent(t *testing.T) {
	fixture := newRunnerFixture(t)
	policyConfig := config.Config{Roles: config.RoleConfigs{Planner: config.PlannerRoleConfig{Escalation: &config.PlannerEscalationConfig{PublicAPI: true}}}}
	metadata, err := loops.WriteHITLAsk(nil, loops.HITLAsk{Question: "Proceed?", Answer: "authorize proceed", Status: "answered"})
	if err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "loop_assess_resume", ProjectID: "project_1", Type: "planner", Status: "running", MetadataJSON: &metadata}
	runner := New(Options{Repos: fixture.repos, AgentExecutor: &fakeAgentExecutor{}, CustomInstructions: &policyConfig, Now: fixture.now})
	checkpoint := plannerCheckpoint{Assessment: &checkpointAssessment{Evidence: plannerAssessmentEvidence{DecisionRequest: "Proceed?"}, Fired: []plannerEscalationCriterion{{Criterion: "surface_public_api"}}, Disposition: "awaiting_human"}}
	got, err := runner.runAssessSuitabilityStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Loop: loop, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runAssessSuitabilityStep() error = %v", err)
	}
	if got.Assessment.Disposition != "authorized" || got.Assessment.HumanAnswer != "authorize proceed" {
		t.Fatalf("assessment = %#v", got.Assessment)
	}
}

func TestRunAssessSuitabilityContinuesNormallyWhenNoCriterionFires(t *testing.T) {
	fixture := newRunnerFixture(t)
	policyConfig := config.Config{Roles: config.RoleConfigs{Planner: config.PlannerRoleConfig{Escalation: &config.PlannerEscalationConfig{MaxEstimatedFiles: 5, PublicAPI: true}}}}
	worktree := t.TempDir()
	runner := New(Options{Repos: fixture.repos, AgentExecutor: assessmentFileExecutor{content: `{"estimatedFiles":3,"estimatedPackages":1,"surfaces":[],"adrConflicts":[],"authorityDecisions":[],"evidence":["internal/planner"],"decisionRequest":""}`}, CustomInstructions: &policyConfig, Now: fixture.now})
	checkpoint := plannerCheckpoint{Issue: &checkpointIssue{Repo: "acme/looper", IssueNumber: 114, Title: "Plan safely"}, Worktree: &checkpointWorktree{Path: worktree}}
	got, err := runner.runAssessSuitabilityStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Loop: storage.LoopRecord{ID: "loop_normal"}, Run: storage.RunRecord{ID: "run_normal"}, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runAssessSuitabilityStep() error = %v", err)
	}
	if got.Assessment == nil || got.Assessment.Disposition != "not_required" {
		t.Fatalf("assessment = %#v", got.Assessment)
	}
	if _, err := os.Stat(filepath.Join(worktree, plannerAssessmentRelPath)); !os.IsNotExist(err) {
		t.Fatalf("assessment sentinel was not consumed: %v", err)
	}
}

func TestSuspendPlannerForHumanParksWithoutFailureOrRetry(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	projectID, loopID := "project_1", "loop_awaiting_authority"
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: loopID, Seq: 114, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	run := storage.RunRecord{ID: "run_assessment", LoopID: loopID, Status: "running", CurrentStep: stringPtr(string(stepAssessSuitability)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	queue := storage.QueueItemRecord{ID: "queue_assessment", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", DedupeKey: "planner:114", Priority: storage.QueuePriorityPlanner, Status: "running", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatal(err)
	}
	runner := New(Options{Repos: fixture.repos, Now: fixture.now})
	checkpoint := plannerCheckpoint{Assessment: &checkpointAssessment{Evidence: plannerAssessmentEvidence{DecisionRequest: "Choose API compatibility"}, Fired: []plannerEscalationCriterion{{Criterion: "surface_public_api", Evidence: []string{"public API"}}}, Disposition: "awaiting_human"}, ResumePolicy: loops.ResumePolicyReplayStep}
	result, err := runner.suspendPlannerForHuman(ctx, loop, run, queue, checkpoint, &plannerAwaitingHumanError{question: "Choose API compatibility", fired: checkpoint.Assessment.Fired})
	if err != nil {
		t.Fatalf("suspendPlannerForHuman() error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q", result.Status)
	}
	storedLoop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	if storedLoop.Status != "awaiting_human" || storedLoop.NextRunAt != nil {
		t.Fatalf("loop = %#v", storedLoop)
	}
	storedRun, _ := fixture.repos.Runs.GetByID(ctx, run.ID)
	if storedRun.Status != "interrupted" || storedRun.ErrorMessage != nil {
		t.Fatalf("run = %#v, want interrupted without error", storedRun)
	}
	storedQueue, _ := fixture.repos.Queue.GetByID(ctx, queue.ID)
	if storedQueue.Status != "cancelled" || storedQueue.LastErrorKind != nil {
		t.Fatalf("queue = %#v, want cancelled without failure kind", storedQueue)
	}
	var persisted plannerCheckpoint
	if err := json.Unmarshal([]byte(*storedRun.CheckpointJSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Assessment == nil || persisted.Assessment.Evidence.DecisionRequest == "" {
		t.Fatalf("checkpoint = %#v", persisted)
	}
}
