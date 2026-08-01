package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// TestPersonalProjectAutoAssignsSelfAuthoredIssue verifies that discovery
// auto-assigns a self-authored issue to the local user in a personal project.
func TestPersonalProjectAutoAssignsSelfAuthoredIssue(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Projects[0].PersonalProject = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "looper", CreatedAt: fixture.now.Format(time.RFC3339)}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues error: %v", err)
	}

	assignCount := countOperations(fixture.github.ops, "assign:looper")
	if assignCount != 1 {
		t.Fatalf("assign operations = %d, want 1; ops=%v", assignCount, fixture.github.ops)
	}
}

// TestPersonalProjectSkipsWhenNotOptedIn verifies no auto-assignment occurs
// when PersonalProject is false.
func TestPersonalProjectSkipsWhenNotOptedIn(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Projects[0].PersonalProject = false
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "looper", CreatedAt: fixture.now.Format(time.RFC3339)}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues error: %v", err)
	}

	assignCount := countOperations(fixture.github.ops, "assign:")
	if assignCount != 0 {
		t.Fatalf("assign operations = %d, want 0; ops=%v", assignCount, fixture.github.ops)
	}
}

// TestPersonalProjectSkipsWhenAssigneeExists verifies no auto-assignment
// when the issue already has an assignee.
func TestPersonalProjectSkipsWhenAssigneeExists(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Projects[0].PersonalProject = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "Bug", Author: "looper",
		CreatedAt: fixture.now.Format(time.RFC3339),
		Assignees: []string{"existing-user"},
	}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues error: %v", err)
	}

	assignCount := countOperations(fixture.github.ops, "assign:")
	if assignCount != 0 {
		t.Fatalf("assign operations = %d, want 0 (assignee exists); ops=%v", assignCount, fixture.github.ops)
	}
}

// TestPersonalProjectSkipsWhenAuthorMismatch verifies no auto-assignment
// when the issue author is not the local user.
func TestPersonalProjectSkipsWhenAuthorMismatch(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Projects[0].PersonalProject = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "other-user", CreatedAt: fixture.now.Format(time.RFC3339)}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues error: %v", err)
	}

	assignCount := countOperations(fixture.github.ops, "assign:")
	if assignCount != 0 {
		t.Fatalf("assign operations = %d, want 0 (author mismatch); ops=%v", assignCount, fixture.github.ops)
	}
}

// TestIsPersonalProject verifies the helper function.
func TestIsPersonalProject(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{name: "nil config", cfg: nil, want: false},
		{name: "not opted in", cfg: &config.Config{Projects: []config.ProjectRefConfig{{ID: "p1"}}}, want: false},
		{name: "opted in", cfg: &config.Config{Projects: []config.ProjectRefConfig{{ID: "p1", PersonalProject: true}}}, want: true},
		{name: "wrong project", cfg: &config.Config{Projects: []config.ProjectRefConfig{{ID: "other", PersonalProject: true}}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{config: tt.cfg}
			if tt.name == "nil config" {
				r = nil
			}
			got := r.isPersonalProject("p1")
			if got != tt.want {
				t.Fatalf("isPersonalProject = %v, want %v", got, tt.want)
			}
		})
	}
}

func init() {
	_ = context.Background
	_ = time.Now
}
