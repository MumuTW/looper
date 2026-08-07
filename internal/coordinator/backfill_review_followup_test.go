package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/triage"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/network/protocol"
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

func TestBackfillIssuesPreservesPreAnalysisCommentSnapshot(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
	})
	original := githubinfra.CommentInfo{ID: 7, Author: "octo", Body: "original context", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}
	humanReply := githubinfra.CommentInfo{ID: 8, Author: "octo", Body: "new human evidence", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "comment race", Author: "octo", State: "open",
		CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339), Comments: []githubinfra.CommentInfo{original},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{original}}
	fixture.runner.triageLLM = concurrentHumanCommentLLM{github: fixture.github, comment: humanReply}

	result, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", SkipTriaged: true})
	if err != nil {
		t.Fatalf("BackfillIssues() error = %v", err)
	}
	// Backfill preserves the existing partial-application contract: deterministic
	// classification labels are written, while the stale comment and triaged
	// marker are vetoed by the new human evidence.
	if result.Triaged != 1 || len(fixture.github.createdBodies) != 0 || countAddedIssueOperations(fixture.github.addedLabels, 1, "triaged") != 0 {
		t.Fatalf("result = %#v ops=%v addedLabels=%v, want human comment and triaged marker veto", result, fixture.github.ops, fixture.github.addedLabels)
	}
	if countAddedIssueOperations(fixture.github.addedLabels, 1, "kind/bug", "area/coordinator", "complexity/m", labels.DispatchPlan) != 1 {
		t.Fatalf("addedLabels = %v, want the documented partial classification application", fixture.github.addedLabels)
	}
}

func TestBackfillIssuesRequiresCurrentCoordinatorLeaseInRoutedMode(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
		cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	})
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1"},
		Lease:      protocol.CoordinatorLease{HolderNodeID: "other-node", FencingToken: 12, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "routed backfill", Author: "octo", State: "open",
		CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339),
	}

	_, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", SkipTriaged: true})
	if !errors.Is(err, ErrBackfillUnavailable) {
		t.Fatalf("BackfillIssues() error = %v, want ErrBackfillUnavailable", err)
	}
	if len(fixture.github.createdBodies) != 0 || len(fixture.github.addedLabels) != 0 {
		t.Fatalf("routed non-holder mutated GitHub: created=%v added=%v", fixture.github.createdBodies, fixture.github.addedLabels)
	}
}

func TestBackfillIssuesRefusesLeaseLossBeforeMutation(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.BackfillEnabled = true
		cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	})
	holder := protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1"},
		Lease:      protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 12, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	lost := holder
	lost.Lease = protocol.CoordinatorLease{HolderNodeID: "other-node", FencingToken: 13, ExpiresAt: timePtr(fixture.now.Add(time.Minute))}
	fixture.network.statusSequence = []protocol.NodeStatusResponse{holder, lost}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "lease race", Author: "octo", State: "open",
		CreatedAt: fixture.now.Add(-24 * time.Hour).Format(time.RFC3339),
	}

	_, err := fixture.runner.BackfillIssues(context.Background(), BackfillInput{ProjectID: fixture.projectID, Repo: "acme/looper", SkipTriaged: true})
	if !errors.Is(err, ErrBackfillUnavailable) {
		t.Fatalf("BackfillIssues() error = %v, want ErrBackfillUnavailable after lease loss", err)
	}
	if len(fixture.github.createdBodies) != 0 || len(fixture.github.addedLabels) != 0 || len(fixture.github.removedLabels) != 0 {
		t.Fatalf("lease-lost backfill mutated GitHub: created=%v added=%v removed=%v", fixture.github.createdBodies, fixture.github.addedLabels, fixture.github.removedLabels)
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

type concurrentHumanCommentLLM struct {
	github  *stubCoordinatorGitHub
	comment githubinfra.CommentInfo
}

func (l concurrentHumanCommentLLM) Complete(context.Context, triage.Request) (string, error) {
	detail := l.github.details[1]
	detail.Comments = append(detail.Comments, l.comment)
	l.github.details[1] = detail
	l.github.comments[1] = [][]githubinfra.CommentInfo{{detail.Comments[0], l.comment}}
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["looper:dispatch:plan"]}}`, nil
}

func (l selectionMutationLLM) Complete(context.Context, triage.Request) (string, error) {
	detail := l.github.details[1]
	detail.State = l.state
	detail.Labels = append([]string(nil), l.labels...)
	l.github.details[1] = detail
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["looper:dispatch:plan"]}}`, nil
}

func (l holdAfterAnalysisLLM) Complete(context.Context, triage.Request) (string, error) {
	l.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "held after analysis", Author: "looper",
		State: "open", Labels: []string{labels.HoldGlobal},
		CreatedAt: l.github.details[1].CreatedAt,
	}
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["looper:dispatch:plan"]}}`, nil
}
