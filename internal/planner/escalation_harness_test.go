package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

const escalatingAssessmentJSON = `{"estimatedFilesTouched":24,"estimatedPackagesTouched":6,"filesEvidence":["internal/planner/runner.go","internal/config/types.go"],"packagesEvidence":["internal/planner","internal/config"],"publicSurfaceChange":true,"publicSurfaces":["config_schema"],"publicSurfaceEvidence":"adds roles.planner.escalation","adrConflict":false,"conflictingAdr":"","adrConflictEvidence":"","unauthorizedDecision":false,"decisionRequired":"","rationale":"the change spans planner and config"}`

const benignAssessmentJSON = `{"estimatedFilesTouched":2,"estimatedPackagesTouched":1,"filesEvidence":["internal/planner/runner.go"],"packagesEvidence":["internal/planner"],"publicSurfaceChange":false,"publicSurfaces":[],"publicSurfaceEvidence":"","adrConflict":false,"conflictingAdr":"","adrConflictEvidence":"","unauthorizedDecision":false,"decisionRequired":"","rationale":"one file in one package"}`

// escalationConfig returns a normalized config whose planner escalation policy
// is enabled with the shipped default thresholds.
func escalationConfig(t *testing.T, enabled bool) *config.Config {
	t.Helper()
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatalf("config.Normalize() error = %v", err)
	}
	cfg.Roles.Planner.Escalation.Enabled = enabled
	return &cfg
}

type escalationHarness struct {
	fixture  *runnerFixture
	runner   *Runner
	github   *fakeGitHubGateway
	git      *fakeGitGateway
	executor *fakeAgentExecutor
	loopID   string
	queueID  string
}

// newEscalationHarness routes one issue into Planner and claims its queue item.
func newEscalationHarness(t *testing.T, enabled bool, agentResults ...AgentResult) *escalationHarness {
	t.Helper()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		issueDetail:    IssueDetail{Number: 114, Title: "Planner needs-human exit", Body: "give planner a durable exit", URL: "https://example/issues/114"},
		createPRResult: CreatePullRequestResult{Number: 501, URL: "https://example/pr/501"},
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/114-planner-needs-human-exit", BaseBranch: "main"}}
	executor := &fakeAgentExecutor{results: agentResults}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git,
		AgentExecutor: executor, Logger: fixture.logger, Now: fixture.now,
		AllowAutoPush: boolPtr(true), CustomInstructions: escalationConfig(t, enabled),
	})
	route, err := runner.RouteIssue(context.Background(), RouteIssueInput{
		ProjectID: "project_1", Repo: "acme/looper", Authority: "triage:report-114",
		Issue: IssueSummary{Number: 114, Title: "Planner needs-human exit"},
	})
	if err != nil || len(route.QueueItems) != 1 {
		t.Fatalf("RouteIssue() = (%#v, %v)", route, err)
	}
	return &escalationHarness{fixture: fixture, runner: runner, github: github, git: git, executor: executor, loopID: route.CreatedLoopIDs[0], queueID: route.QueueItems[0].ID}
}

func (h *escalationHarness) process(t *testing.T) ProcessResult {
	t.Helper()
	claim, err := h.fixture.repos.Queue.ClaimNextOfType(context.Background(), h.fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	processed, err := h.runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	return processed
}

func (h *escalationHarness) loop(t *testing.T) storage.LoopRecord {
	t.Helper()
	loop, err := h.fixture.repos.Loops.GetByID(context.Background(), h.loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	return *loop
}

// answerEscalation mirrors what POST /loops/{seq}/respond persists before it
// transitions the loop back to running.
func (h *escalationHarness) answerEscalation(t *testing.T, answer string) {
	t.Helper()
	loop := h.loop(t)
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		t.Fatal("loop carries no HITL ask to answer")
	}
	ask.Answer = answer
	ask.Status = "answered"
	ask.AnsweredAt = h.fixture.nowISO()
	metadataJSON, err := loops.WriteHITLAsk(loop.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.MetadataJSON = &metadataJSON
	loop.Status = "running"
	loop.UpdatedAt = h.fixture.nowISO()
	if err := h.fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if _, err := h.fixture.repos.Queue.RequeueLatestCancelledByLoop(context.Background(), h.loopID, h.fixture.nowISO()); err != nil {
		t.Fatalf("RequeueLatestCancelledByLoop() error = %v", err)
	}
}
