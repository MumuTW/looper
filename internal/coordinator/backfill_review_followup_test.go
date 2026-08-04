package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/triage"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestBackfillIssuesSkipsPullRequestTargets(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "PR", Author: "looper", IsPullRequest: true,
		State: "open", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339),
	}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", SkipTriaged: true})
	if err != nil {
		t.Fatalf("BackfillIssues() error = %v", err)
	}
	if result.Triaged != 0 || result.SkipReasons["pull_request"] != 1 || len(fixture.github.createdBodies) != 0 {
		t.Fatalf("result = %#v createdBodies=%v, want PR skipped without mutation", result, fixture.github.createdBodies)
	}
}

func TestBackfillIssuesRechecksHoldBeforeMutation(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "held after analysis", Author: "looper",
		State: "open", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339),
	}
	fixture.runner.triageLLM = holdAfterAnalysisLLM{github: fixture.github}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", SkipTriaged: true})
	if err != nil {
		t.Fatalf("BackfillIssues() error = %v", err)
	}
	if result.Triaged != 0 || result.SkipReasons["hold"] != 1 || len(fixture.github.createdBodies) != 0 {
		t.Fatalf("result = %#v createdBodies=%v, want fresh hold to veto mutation", result, fixture.github.createdBodies)
	}
}

func TestBackfillIssuesRechecksOpenAndLabelSelectionBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		freshState string
		freshLabel []string
		wantReason string
	}{
		{name: "closed issue", freshState: "closed", freshLabel: []string{"backfill"}, wantReason: "not_open"},
		{name: "label removed", freshState: "open", freshLabel: nil, wantReason: "label_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
				cfg.Roles.Coordinator.Enabled = true
				cfg.Roles.Coordinator.BackfillEnabled = true
			})
			fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"backfill"}}}
			fixture.github.details[1] = githubinfra.IssueDetail{
				Number: 1, Title: "selection changes during analysis", Author: "looper",
				State: "open", CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339), Labels: []string{"backfill"},
			}
			fixture.runner.triageLLM = selectionMutationLLM{github: fixture.github, state: tc.freshState, labels: tc.freshLabel}

			result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", LabelFilter: "backfill", SkipTriaged: true})
			if err != nil {
				t.Fatalf("BackfillIssues() error = %v", err)
			}
			if result.Triaged != 0 || result.SkipReasons[tc.wantReason] != 1 || len(fixture.github.createdBodies) != 0 {
				t.Fatalf("result = %#v createdBodies=%v, want fresh %s gate", result, fixture.github.createdBodies, tc.wantReason)
			}
		})
	}
}

type holdAfterAnalysisLLM struct {
	github *stubCoordinatorGitHub
}

type selectionMutationLLM struct {
	github *stubCoordinatorGitHub
	state  string
	labels []string
}

func (l selectionMutationLLM) Complete(context.Context, triage.Request) (string, error) {
	detail := l.github.details[1]
	detail.State = l.state
	detail.Labels = append([]string(nil), l.labels...)
	l.github.details[1] = detail
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}`, nil
}

func (l holdAfterAnalysisLLM) Complete(context.Context, triage.Request) (string, error) {
	l.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "held after analysis", Author: "looper",
		State: "open", Labels: []string{labels.HoldGlobal},
		CreatedAt: l.github.details[1].CreatedAt,
	}
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}`, nil
}
