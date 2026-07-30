package reproducer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
)

const (
	testProjectID = "project_1"
	testRepo      = "acme/looper"
	testIssue     = int64(41)
)

type fixture struct {
	t         *testing.T
	now       time.Time
	repos     *storage.Repositories
	project   storage.ProjectRecord
	github    *fakeGitHub
	git       *fakeGit
	agent     *fakeAgent
	planner   *fakePlanner
	worktree  string
	proofPass bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "reproducer.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	stamp := now.Format(time.RFC3339Nano)
	worktreeRoot := t.TempDir()
	metadata := `{"worktreeRoot":"` + worktreeRoot + `"}`
	project := storage.ProjectRecord{ID: testProjectID, Name: "Looper", RepoPath: t.TempDir(), MetadataJSON: &metadata, CreatedAt: stamp, UpdatedAt: stamp}
	if err := repos.Projects.Upsert(context.Background(), project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	worktree := filepath.Join(worktreeRoot, "project_1-41-crash")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	return &fixture{
		t: t, now: now, repos: repos, project: project, worktree: worktree,
		github: &fakeGitHub{detail: githubinfra.IssueDetail{
			Number: testIssue, Title: "Crash on empty input", Body: "It panics.",
			URL: "https://github.com/acme/looper/issues/41", State: "open",
		}},
		git:     &fakeGit{worktreePath: worktree, headSHA: "base000"},
		agent:   &fakeAgent{},
		planner: &fakePlanner{},
	}
}

func (f *fixture) runner() *Runner {
	return New(Options{
		Repos: f.repos, GitHub: f.github, Git: f.git, AgentExecutor: f.agent, Planner: f.planner,
		Now: func() time.Time { return f.now },
		proveFailing: func(_ context.Context, input reproduction.Input) (reproduction.Result, error) {
			if f.proofPass {
				return reproduction.Result{Reason: reproduction.ReasonCommandPassedOnBase, Summary: "passed on base"}, nil
			}
			return reproduction.Result{Passed: true, Summary: "observed failing", Output: "FAIL: panic"}, nil
		},
	})
}

func (f *fixture) discover() DiscoveryResult {
	f.t.Helper()
	result, err := f.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: testProjectID, Repo: testRepo})
	if err != nil {
		f.t.Fatalf("DiscoverIssues() error = %v", err)
	}
	return result
}

// seedTriageReport persists the Triage Report that authorizes Reproducer. It is
// the only thing that puts an Issue in Reproducer's scope, which is what keeps
// label/assignee-discovered Issues on today's path.
func (f *fixture) seedTriageReport(classification triager.Classification) triager.Report {
	f.t.Helper()
	report := triager.Report{
		Version: 1, IdempotencyKey: "triage:seed", ProjectID: testProjectID, Repo: testRepo,
		IssueNumber: testIssue, Decision: triager.Decision{Classification: classification},
		Policy: triager.PolicyDecision{Action: triager.ActionRoutePlanner}, CreatedAt: f.now.Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(report)
	if err != nil {
		f.t.Fatalf("marshal report: %v", err)
	}
	projectID, entityType := testProjectID, "github_issue"
	entityID := reproduction.EntityID(testProjectID, testRepo, testIssue)
	if err := f.repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID: "event_triage_seed", EventType: triager.ReportEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, PayloadJSON: string(payload),
		CreatedAt: f.now.Format(time.RFC3339Nano),
	}); err != nil {
		f.t.Fatalf("Events.Append() error = %v", err)
	}
	return report
}

func (f *fixture) status() reproduction.Status {
	f.t.Helper()
	status, err := reproduction.LoadStatus(context.Background(), f.repos, testProjectID, testRepo, testIssue)
	if err != nil {
		f.t.Fatalf("LoadStatus() error = %v", err)
	}
	return status
}

func (f *fixture) writeAgentDraft(command string, files map[string]string) {
	f.t.Helper()
	paths := make([]string, 0, len(files))
	for rel, contents := range files {
		path := filepath.Join(f.worktree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			f.t.Fatalf("WriteFile() error = %v", err)
		}
		paths = append(paths, rel)
	}
	draft := map[string]any{"version": reproduction.ManifestVersion, "command": command, "files": paths, "observedFailure": "panic: index out of range"}
	f.writeWorktreeJSON(reproduction.ManifestRelPath, draft)
}

func (f *fixture) writeCannotReproduce(record CannotReproduce) {
	f.t.Helper()
	record.Version = reproduction.ManifestVersion
	f.writeWorktreeJSON(CannotReproduceRelPath, record)
}

func (f *fixture) writeWorktreeJSON(rel string, value any) {
	f.t.Helper()
	path := filepath.Join(f.worktree, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("MkdirAll() error = %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		f.t.Fatalf("marshal %s: %v", rel, err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		f.t.Fatalf("WriteFile() error = %v", err)
	}
}

type fakeGitHub struct {
	detail githubinfra.IssueDetail
	calls  int
}

func (f *fakeGitHub) ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	f.calls++
	return f.detail, nil
}

type fakeGit struct {
	worktreePath string
	headSHA      string
	dirty        bool
	commits      []planner.CommitInput
	createCalls  int
}

func (f *fakeGit) CreateWorktree(context.Context, planner.CreateWorktreeInput) (planner.CreateWorktreeResult, error) {
	f.createCalls++
	return planner.CreateWorktreeResult{ID: "wt_1", WorktreePath: f.worktreePath, Branch: "looper/planner/41-crash", BaseBranch: "main"}, nil
}

func (f *fakeGit) InspectHead(context.Context, planner.InspectHeadInput) (planner.InspectHeadResult, error) {
	return planner.InspectHeadResult{HeadSHA: f.headSHA, HasUncommittedChanges: f.dirty}, nil
}

func (f *fakeGit) Commit(_ context.Context, input planner.CommitInput) (planner.CommitResult, error) {
	f.commits = append(f.commits, input)
	f.headSHA = "repro111"
	return planner.CommitResult{CommitSHA: f.headSHA}, nil
}

type fakeAgent struct {
	status  string
	prompts []string
	starts  int
}

func (f *fakeAgent) Start(_ context.Context, input planner.AgentRunInput) (planner.AgentExecution, error) {
	f.starts++
	f.prompts = append(f.prompts, input.Prompt)
	status := f.status
	if status == "" {
		status = "completed"
	}
	return fakeExecution{status: status}, nil
}

type fakeExecution struct{ status string }

func (e fakeExecution) Wait(context.Context) (planner.AgentResult, error) {
	return planner.AgentResult{Status: e.status, Summary: "reproduction attempt finished"}, nil
}

type fakePlanner struct {
	parked []planner.ParkIssueInput
	loop   storage.LoopRecord
}

func (f *fakePlanner) ParkIssueForHuman(_ context.Context, input planner.ParkIssueInput) (storage.LoopRecord, error) {
	f.parked = append(f.parked, input)
	if f.loop.ID == "" {
		f.loop = storage.LoopRecord{ID: "loop_parked", ProjectID: testProjectID, Type: "planner", Status: "awaiting_human"}
	}
	return f.loop, nil
}
