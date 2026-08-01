package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func configureConflictWatchFixture(t *testing.T, repairs int) coordinatorFixture {
	t.Helper()
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.ConflictPolicy = &config.CoordinatorConflictPolicyConfig{MaxRepairs: 2}
		cfg.Roles.Fixer.Triggers.Labels = []string{"looper:fix"}
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	detail := githubinfra.IssueDetail{
		Number: 1, Title: "Bug", Body: "please fix", Author: "octo",
		Labels: []string{"triaged"}, CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}}
	detail.Comments = []githubinfra.CommentInfo{{
		ID: 44, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, mergeWatchMarkerLine(mergewatch.PriorWatchMarker{PRNumber: 77, HeadSHA: "old-head", Retries: 3, ConflictRepairs: repairs})), CreatedAt: fixture.now.Format(time.RFC3339),
	}}
	fixture.github.details[1] = detail
	fixture.github.prDetails[77] = githubinfra.PullRequestDetail{
		Number: 77, Body: "Closes #1", State: "open", HeadSHA: "new-head", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger}, Mergeable: boolPtr(false), MergeableState: "dirty",
		AutoMerge: &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	return fixture
}

func TestMergeWatchConflictRepairsSurviveHeadChangesAndRegenerateAtBoundary(t *testing.T) {
	fixture := configureConflictWatchFixture(t, 1)
	var got ConflictRegenerationInput
	calls := 0
	fixture.runner.regenerateConflict = func(_ context.Context, input ConflictRegenerationInput) (ConflictRegenerationResult, error) {
		calls++
		got = input
		return ConflictRegenerationResult{Completed: true}, nil
	}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if calls != 1 || got.ConflictRepairs != 2 {
		t.Fatalf("regeneration calls/input = %d/%#v, want one call at repair 2", calls, got)
	}
	if len(fixture.github.addedPRLabels) != 0 {
		t.Fatalf("addedPRLabels = %#v, want no further Fixer repair at boundary", fixture.github.addedPRLabels)
	}
	if !containsString(fixture.github.ops, "delete-comment") {
		t.Fatalf("ops = %v, want merge-watch marker cleanup after regeneration", fixture.github.ops)
	}
}

func TestMergeWatchConflictEscalationRemainsDurable(t *testing.T) {
	fixture := configureConflictWatchFixture(t, 1)
	fixture.runner.regenerateConflict = func(_ context.Context, _ ConflictRegenerationInput) (ConflictRegenerationResult, error) {
		return ConflictRegenerationResult{Escalated: true}, nil
	}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.updatedBodies) != 2 || !strings.Contains(fixture.github.updatedBodies[0], "conflict_regen_pending=1") || !strings.Contains(fixture.github.updatedBodies[1], "conflict_regen_escalated=1") {
		t.Fatalf("updatedBodies = %v, want pending fence followed by durable escalation marker", fixture.github.updatedBodies)
	}
	if containsString(fixture.github.ops, "delete-comment") {
		t.Fatalf("ops = %v, want marker retained for human escalation", fixture.github.ops)
	}
}

func TestMergeWatchConflictPollDoesNotConsumeSameHeadRepairBudget(t *testing.T) {
	fixture := configureConflictWatchFixture(t, 0)
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	detail := fixture.github.details[1]
	detail.Comments = []githubinfra.CommentInfo{{
		ID: 44, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, mergeWatchMarkerLine(mergewatch.PriorWatchMarker{PRNumber: 77, HeadSHA: "new-head", Retries: 3, ConflictRepairs: 1})), CreatedAt: fixture.now.Format(time.RFC3339),
	}}
	fixture.github.details[1] = detail
	fixture.now = fixture.now.Add(6 * time.Minute)
	fixture.reconfigure(func(*config.Config) {})
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("same-head DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.updatedBodies) != 2 || !strings.Contains(fixture.github.updatedBodies[1], "conflict_repairs=1") {
		t.Fatalf("updatedBodies = %v, want same-head poll to retain one dispatched repair", fixture.github.updatedBodies)
	}
}

func TestMergeWatchEscalationReevaluatesAfterHeadChanges(t *testing.T) {
	fixture := configureConflictWatchFixture(t, 1)
	detail := fixture.github.details[1]
	detail.Comments = []githubinfra.CommentInfo{{
		ID: 44, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, mergeWatchMarkerLine(mergewatch.PriorWatchMarker{PRNumber: 77, HeadSHA: "new-head", Retries: 3, ConflictRepairs: 2, ConflictRegenerationEscalated: true})), CreatedAt: fixture.now.Format(time.RFC3339),
	}}
	fixture.github.details[1] = detail
	routes := 0
	fixture.runner.regenerateConflict = func(_ context.Context, _ ConflictRegenerationInput) (ConflictRegenerationResult, error) {
		routes++
		return ConflictRegenerationResult{Completed: true}, nil
	}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial DiscoverIssues() error = %v", err)
	}
	detail = fixture.github.details[1]
	detail.Comments = []githubinfra.CommentInfo{{
		ID: 44, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, mergeWatchMarkerLine(mergewatch.PriorWatchMarker{PRNumber: 77, HeadSHA: "new-head", Retries: 3, ConflictRepairs: 2, ConflictRegenerationEscalated: true})), CreatedAt: fixture.now.Format(time.RFC3339),
	}}
	fixture.github.details[1] = detail
	pr := fixture.github.prDetails[77]
	pr.HeadSHA = "next-head"
	fixture.github.prDetails[77] = pr
	fixture.now = fixture.now.Add(6 * time.Minute)
	fixture.reconfigure(func(*config.Config) {})
	fixture.runner.regenerateConflict = func(_ context.Context, _ ConflictRegenerationInput) (ConflictRegenerationResult, error) {
		routes++
		return ConflictRegenerationResult{Completed: true}, nil
	}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("changed-head DiscoverIssues() error = %v", err)
	}
	if routes != 1 {
		t.Fatalf("regeneration routes = %d, want one re-evaluation after head change", routes)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
