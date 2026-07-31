package gatekeeper

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestEvaluatePullRequestBlocksProtectedPathAndRecordsEvidence(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.ChangedFiles = []string{
		"docs/README.md",
		"internal/gatekeeper/runner.go",
		"internal/gatekeeper/runner.go",
	}
	runner := New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool {
			return fixture.policyPermits
		},
		ProtectedPathsForProject: func(string) []string {
			return []string{"internal/gatekeeper/**"}
		},
	})

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonProtectedPathTouched) {
		t.Fatalf("report = %#v, want protected-path blocker", report)
	}
	if !slices.Equal(report.Evidence.ProtectedPaths, []string{"internal/gatekeeper/runner.go"}) {
		t.Fatalf("protected path evidence = %#v", report.Evidence.ProtectedPaths)
	}
	if len(report.Reasons) != 1 || report.Reasons[0].Subject != "internal/gatekeeper/runner.go" {
		t.Fatalf("protected path reasons = %#v, want human-readable matched path", report.Reasons)
	}
}

func TestEvaluatePullRequestProtectedPathGateIsNoOpWithoutPatterns(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.ChangedFiles = []string{"internal/gatekeeper/runner.go"}
	runner := fixture.runner()

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || len(report.Evidence.ProtectedPaths) != 0 {
		t.Fatalf("report = %#v, want unchanged eligible result with no protected-path evidence", report)
	}
}

func TestEvaluatePullRequestProtectedPathGateKeepsHeadRaceBlocked(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.ChangedFiles = []string{"internal/gatekeeper/runner.go"}
	fixture.github.finalHeadSHA = "head-2"
	runner := New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		ProtectedPathsForProject: func(string) []string {
			return []string{"internal/gatekeeper/**"}
		},
	})

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonProtectedPathTouched) || !hasReason(report, ReasonHeadStale) {
		t.Fatalf("report = %#v, want protected-path and head-race blockers", report)
	}
}
