package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/triage"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestBackfillIssuesTriagesSelfAuthoredUntriagedIssue(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	issues := []githubinfra.IssueSummary{{Number: 1, Labels: nil}}
	fixture.github.issues = issues
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "Bug", Author: "looper",
		CreatedAt: fixture.now.Add(-48 * time.Hour).Format(time.RFC3339),
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:   fixture.projectID,
		Repo:        "acme/looper",
		SkipTriaged: true,
	})

	if err != nil {
		t.Fatalf("BackfillIssues error: %v", err)
	}
	if result.Triaged != 1 {
		t.Fatalf("triaged = %d, want 1; skipped=%d", result.Triaged, result.Skipped)
	}
	if result.Considered != 1 {
		t.Fatalf("considered = %d, want 1", result.Considered)
	}
}

func TestBackfillIssuesSkipsAlreadyTriagedIssue(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "Bug", Author: "looper",
		CreatedAt: fixture.now.Add(-48 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"triaged"},
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:   fixture.projectID,
		Repo:        "acme/looper",
		SkipTriaged: true,
	})

	if err != nil {
		t.Fatalf("BackfillIssues error: %v", err)
	}
	if result.Triaged != 0 {
		t.Fatalf("triaged = %d, want 0; skipped=%d; reasons=%v", result.Triaged, result.Skipped, result.SkipReasons)
	}
	if result.SkipReasons["already_triaged"] != 1 {
		t.Fatalf("already_triaged skips = %d, want 1", result.SkipReasons["already_triaged"])
	}
}

func TestBackfillIssuesBoundedByMaxCount(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{
		{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}, {Number: 5},
	}
	for i := int64(1); i <= 5; i++ {
		fixture.github.details[i] = githubinfra.IssueDetail{
			Number: i, Title: "Bug", Author: "looper",
			CreatedAt: fixture.now.Add(-48 * time.Hour).Format(time.RFC3339),
		}
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:   fixture.projectID,
		Repo:        "acme/looper",
		MaxCount:    3,
		SkipTriaged: true,
	})

	if err != nil {
		t.Fatalf("BackfillIssues error: %v", err)
	}
	if result.Triaged != 3 {
		t.Fatalf("triaged = %d, want 3 (bounded by MaxCount)", result.Triaged)
	}
}

func TestBackfillIssuesFiltersByIssueNumbers(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{
		{Number: 1}, {Number: 2}, {Number: 3},
	}
	for i := int64(1); i <= 3; i++ {
		fixture.github.details[i] = githubinfra.IssueDetail{
			Number: i, Title: "Bug", Author: "looper",
			CreatedAt: fixture.now.Add(-48 * time.Hour).Format(time.RFC3339),
		}
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:    fixture.projectID,
		Repo:         "acme/looper",
		IssueNumbers: []int64{1, 3},
		SkipTriaged:  true,
	})

	if err != nil {
		t.Fatalf("BackfillIssues error: %v", err)
	}
	if result.Triaged != 2 {
		t.Fatalf("triaged = %d, want 2", result.Triaged)
	}
	if result.Considered != 2 {
		t.Fatalf("considered = %d, want 2 (explicit numbers are fetched directly)", result.Considered)
	}
}

func TestBackfillIssuesForceRetriageReTriages(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "Bug", Author: "looper",
		CreatedAt: fixture.now.Add(-48 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"triaged"},
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:     fixture.projectID,
		Repo:          "acme/looper",
		SkipTriaged:   true,
		ForceRetriage: true,
	})

	if err != nil {
		t.Fatalf("BackfillIssues error: %v", err)
	}
	// ForceRetriage bypasses the already-triaged gate in BackfillIssues;
	// decideBackfill has no eligibility gate of its own.
	if result.SkipReasons["already_triaged"] != 0 {
		t.Fatalf("result = %#v, want no already_triaged skip under ForceRetriage", result)
	}
	if result.Considered != 1 {
		t.Fatalf("considered = %d, want 1", result.Considered)
	}
}

func TestBackfillIssuesRequiresForceForTriagedIssue(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "already triaged", Author: "looper",
		State: "open", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339),
		Labels: []string{"triaged"},
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:   fixture.projectID,
		Repo:        "acme/looper",
		SkipTriaged: false,
	})
	if err != nil {
		t.Fatalf("BackfillIssues() error = %v", err)
	}
	if result.Triaged != 0 || result.SkipReasons["force_retriage_required"] != 1 || len(fixture.github.createdBodies) != 0 {
		t.Fatalf("result = %#v createdBodies=%v, want force confirmation before triaged mutation", result, fixture.github.createdBodies)
	}
}

func TestBackfillIssuesMissingDetailDoesNotFail(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}, {Number: 2}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "Bug", Author: "looper",
		CreatedAt: fixture.now.Add(-48 * time.Hour).Format(time.RFC3339),
	}
	// details[2] is missing → ViewIssue returns zero value without an error.

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{
		ProjectID:   fixture.projectID,
		Repo:        "acme/looper",
		SkipTriaged: true,
	})

	if err != nil {
		t.Fatalf("BackfillIssues error: %v", err)
	}
	// Issue 2 has no detail but the stub returns a zero value without an error;
	// the invalid timestamp is a recorded skip, not a provider failure.
	if result.Considered != 2 || result.SkipReasons["invalid_created_at"] != 1 || len(result.FailedIssues) != 0 {
		t.Fatalf("result = %#v, want one invalid-created-at skip and no failures", result)
	}
}

func TestBackfillIssuesRequiresExplicitProjectOptIn(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
	})
	_, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err == nil || !strings.Contains(err.Error(), "backfill is disabled") {
		t.Fatalf("BackfillIssues() error = %v, want disabled opt-in error", err)
	}
}

func TestBackfillIssuesAppliesAgeHoldAndNormalizedLabelGates(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}, {Number: 2}, {Number: 3}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "old", CreatedAt: fixture.now.Add(-40 * 24 * time.Hour).Format(time.RFC3339), Labels: []string{"Backfill"}}
	fixture.github.details[2] = githubinfra.IssueDetail{Number: 2, Title: "held", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339), Labels: []string{" BACKFILL ", " " + strings.ToUpper(labels.HoldGlobal) + " "}}
	fixture.github.details[3] = githubinfra.IssueDetail{Number: 3, Title: "fresh", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339), Labels: []string{"BACKFILL"}}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", LabelFilter: " backfill ", MaxAgeDays: 30})
	if err != nil {
		t.Fatalf("BackfillIssues() error = %v", err)
	}
	if result.Triaged != 1 || result.SkipReasons["too_old"] != 1 || result.SkipReasons["hold"] != 1 {
		t.Fatalf("result = %#v, want one triage plus age/hold skips", result)
	}
}

type failingBackfillLLM struct{}

func (failingBackfillLLM) Complete(context.Context, triage.Request) (string, error) {
	return "", errors.New("provider unavailable")
}

func TestBackfillIssuesReportsLLMFailureAndCountsAttempt(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.runner.triageLLM = failingBackfillLLM{}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}, {Number: 2}}
	for number := int64(1); number <= 2; number++ {
		fixture.github.details[number] = githubinfra.IssueDetail{Number: number, Title: "Bug", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339)}
	}
	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", MaxCount: 1})
	if err != nil {
		t.Fatalf("BackfillIssues() error = %v", err)
	}
	if result.Considered != 1 || len(result.FailedIssues) != 1 || result.Triaged != 0 {
		t.Fatalf("result = %#v, want one failed bounded attempt", result)
	}
}

func TestBackfillIssuesHonorsCancellationBeforeListing(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fixture.runner.BackfillIssues(ctx, BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BackfillIssues() error = %v, want context.Canceled", err)
	}
}

func init() {
	_ = strings.TrimSpace
}
