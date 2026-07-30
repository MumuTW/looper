package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func occupancyLoop(id, loopType, status, targetID string, seq int64) storage.LoopRecord {
	target := targetID
	return storage.LoopRecord{ID: id, Seq: seq, ProjectID: "project_1", Type: loopType, TargetType: "issue", TargetID: &target, Status: status}
}

func TestFindIssueOccupantSeesTheRolesThatCollide(t *testing.T) {
	for _, role := range []string{"fixer", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			loops := []storage.LoopRecord{occupancyLoop("loop_a", role, "running", "issue:acme/looper:66", 7)}
			occupant := findIssueOccupant(loops, "project_1", "acme/looper", 66, "")
			if occupant == nil {
				t.Fatalf("findIssueOccupant() = nil, want the %s loop", role)
			}
			if occupant.LoopID != "loop_a" || occupant.Type != role || occupant.Seq != 7 {
				t.Fatalf("occupant = %#v, want loop_a/%s/7", occupant, role)
			}
		})
	}
}

// Planner writes the spec a worker implements, so a live planner loop is the
// handoff this pipeline is built on — refusing the worker would break it. A
// second worker is prevented upstream by loop reuse, not here.
func TestFindIssueOccupantIgnoresRolesThatAreNotCollisions(t *testing.T) {
	for _, role := range []string{"planner", "worker"} {
		t.Run(role, func(t *testing.T) {
			loops := []storage.LoopRecord{occupancyLoop("loop_"+role, role, "running", "issue:acme/looper:66", 11)}
			if occupant := findIssueOccupant(loops, "project_1", "acme/looper", 66, ""); occupant != nil {
				t.Fatalf("findIssueOccupant() = %#v, want a %s loop treated as no collision", occupant, role)
			}
		})
	}
}

func TestFindIssueOccupantIgnoresLoopsThatNoLongerHoldTheIssue(t *testing.T) {
	cases := map[string]storage.LoopRecord{
		"completed":       occupancyLoop("loop_done", "fixer", "completed", "issue:acme/looper:66", 1),
		"failed":          occupancyLoop("loop_failed", "fixer", "failed", "issue:acme/looper:66", 2),
		"terminated":      occupancyLoop("loop_term", "fixer", "terminated", "issue:acme/looper:66", 3),
		"another issue":   occupancyLoop("loop_other", "fixer", "running", "issue:acme/looper:99", 4),
		"another repo":    occupancyLoop("loop_repo", "fixer", "running", "issue:acme/other:66", 5),
		"another project": {ID: "loop_proj", Seq: 6, ProjectID: "project_2", Type: "fixer", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:66"), Status: "running"},
	}
	for name, loop := range cases {
		t.Run(name, func(t *testing.T) {
			if occupant := findIssueOccupant([]storage.LoopRecord{loop}, "project_1", "acme/looper", 66, ""); occupant != nil {
				t.Fatalf("findIssueOccupant() = %#v, want nil", occupant)
			}
		})
	}
}

// A worker that has retargeted onto its pull request still occupies the issue it
// came from; the issue number survives in worker metadata rather than the target.
func TestFindIssueOccupantSeesAFixerOnThePullRequestForTheIssue(t *testing.T) {
	targetID := "pr:acme/looper:4242"
	metadata := `{"worker":{"issueNumber":66,"repo":"acme/looper"}}`
	loops := []storage.LoopRecord{{
		ID: "loop_pr", Seq: 8, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Status: "running", MetadataJSON: &metadata,
	}}
	occupant := findIssueOccupant(loops, "project_1", "acme/looper", 66, "")
	if occupant == nil || occupant.LoopID != "loop_pr" {
		t.Fatalf("findIssueOccupant() = %#v, want the fixer working the issue's pull request", occupant)
	}
}

func TestFindIssueOccupantHonoursTheExclusion(t *testing.T) {
	loops := []storage.LoopRecord{occupancyLoop("loop_self", "fixer", "running", "issue:acme/looper:66", 9)}
	if occupant := findIssueOccupant(loops, "project_1", "acme/looper", 66, "loop_self"); occupant != nil {
		t.Fatalf("findIssueOccupant() = %#v, want the excluded loop skipped", occupant)
	}
}

func TestFindIssueOccupantRejectsUnusableInput(t *testing.T) {
	loops := []storage.LoopRecord{occupancyLoop("loop_a", "fixer", "running", "issue:acme/looper:66", 1)}
	if occupant := findIssueOccupant(loops, "project_1", "", 66, ""); occupant != nil {
		t.Fatalf("findIssueOccupant(no repo) = %#v, want nil", occupant)
	}
	if occupant := findIssueOccupant(loops, "project_1", "acme/looper", 0, ""); occupant != nil {
		t.Fatalf("findIssueOccupant(no issue) = %#v, want nil", occupant)
	}
}

func TestHandlerWorkerCreateRefusesAnIssueAFixerIsAlreadyWorking(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	repo := "acme/looper"
	targetID := "issue:acme/looper:66"
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop_fixer_66x", Seq: 501, ProjectID: "project_1", Type: "fixer",
		TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","issueNumber":66,"baseBranch":"main"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	message := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "fixer loop loop_fixer_66x") {
		t.Fatalf("error message = %q, want it to name the occupying loop", message)
	}

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("Queue.List() = %#v, want nothing enqueued for an occupied issue", queueItems)
	}
}

func TestHandlerWorkerCreateAllowsForcingPastAnOccupiedIssue(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	repo := "acme/looper"
	targetID := "issue:acme/looper:66"
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop_fixer_66", Seq: 502, ProjectID: "project_1", Type: "fixer",
		TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","issueNumber":66,"baseBranch":"main","force":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
}
