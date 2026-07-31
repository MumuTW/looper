package reviewer

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/reviewer/convergence"
	"github.com/MumuTW/looper/internal/storage"
)

func TestConvergenceItemsFromDetailUsesStructuredSeverityAndFixerEvidence(t *testing.T) {
	round := convergenceItemsFromDetail(PullRequestDetail{Comments: []map[string]any{
		{"id": "comment-1", "threadId": "thread-1", "state": "UNRESOLVED", "body": "Fix it\n\n<!-- looper:review-item severity=blocking -->", "fixerResult": "declined", "fixerAttemptKey": "fingerprint-1"},
		{"id": "comment-2", "state": "UNRESOLVED", "body": "prose says blocking"},
		{"id": "comment-3", "state": "RESOLVED", "body": "Nit\n\n<!-- looper:review-item severity=nit -->"},
	}})
	if len(round.Items) != 2 {
		t.Fatalf("round.Items = %#v, want two marker-backed items", round.Items)
	}
	if got := round.Items[0]; got.ID != "comment-1" || got.Status != convergence.ItemStatusOpen || got.FixerResult != convergence.FixerResultDeclined || got.FixerAttemptKey != "fingerprint-1" {
		t.Fatalf("first item = %#v", got)
	}
	if got := round.Items[1]; got.Status != convergence.ItemStatusResolved || got.Severity != "nit" {
		t.Fatalf("second item = %#v", got)
	}
}

func TestConvergenceMetadataRoundTripsJSON(t *testing.T) {
	meta := map[string]any{}
	want := convergenceMetadata{Policy: convergence.DefaultPolicy(), State: convergence.State{TotalRounds: 2}, Action: convergence.ActionEscalate, Reason: convergence.ReasonStalled, Status: "awaiting_human"}
	if err := writeConvergenceMetadata(meta, want); err != nil {
		t.Fatal(err)
	}
	got, ok := parseConvergenceMetadata(meta)
	if !ok || got.State.TotalRounds != 2 || got.Action != convergence.ActionEscalate || got.Reason != convergence.ReasonStalled || got.Status != "awaiting_human" {
		t.Fatalf("round trip = (%#v, %v)", got, ok)
	}
}

func TestRecordConvergenceProgressPersistsAndEscalates(t *testing.T) {
	fixture := newRunnerFixture(t)
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, Now: fixture.now,
		Convergence: config.ReviewerConvergenceConfig{MaxConsecutiveUnproductive: 2, MaxFixerAttemptsPerItem: 4, MaxTotalRounds: 40, SeverityFloor: config.ReviewerSeverityFloorNonBlocking},
	})
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop-convergence-runtime", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatal(err)
	}
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("project = %#v, err=%v", project, err)
	}
	input := stepInput{Project: *project, Loop: loop, Repo: repo, PRNumber: prNumber}
	detail := PullRequestDetail{HeadSHA: "head-1", Comments: []map[string]any{{"id": "comment-1", "state": "UNRESOLVED", "body": "Fix\n\n<!-- looper:review-item severity=blocking -->"}}}
	if err := runner.recordConvergenceProgress(context.Background(), input, detail); err != nil {
		t.Fatalf("first recordConvergenceProgress() error = %v", err)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("persisted loop = %#v, err=%v", persisted, err)
	}
	if got, ok := parseConvergenceMetadata(parseJSONObject(persisted.MetadataJSON)); !ok || got.State.TotalRounds != 1 || got.Status != "active" {
		t.Fatalf("first convergence metadata = %#v, ok=%v", got, ok)
	}
	if err := runner.recordConvergenceProgress(context.Background(), input, detail); err != nil {
		t.Fatalf("second recordConvergenceProgress() error = %v", err)
	}
	if err := runner.recordConvergenceProgress(context.Background(), input, detail); err == nil {
		t.Fatal("third recordConvergenceProgress() error = nil, want stall escalation")
	}
	persisted, err = fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("persisted escalated loop = %#v, err=%v", persisted, err)
	}
	got, ok := parseConvergenceMetadata(parseJSONObject(persisted.MetadataJSON))
	if !ok || got.State.TotalRounds != 3 || got.Status != "awaiting_human" || got.Reason != convergence.ReasonStalled {
		t.Fatalf("escalated convergence metadata = %#v, ok=%v", got, ok)
	}
}
