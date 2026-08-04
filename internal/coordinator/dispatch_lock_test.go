package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/dispatch"
	"github.com/MumuTW/looper/internal/coordinator/triage"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

func TestApplyDispatchesReReadsIssueUnderPerIssueLock(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
	})
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1, Title: "freshly changed", Author: "octo", State: "open",
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	roles := config.ProjectRoleConfigs(*fixture.cfg, fixture.projectID)
	triageCfg := roleConfigToTriageConfig(roles.Coordinator)
	dispatchCfg := roleConfigToDispatchConfig(roles.Coordinator, roles)
	stale := loadedIssue{
		summary: githubinfra.IssueSummary{Number: 1, Labels: []string{"triaged", dispatch.DispatchPlan}},
		issue:   triage.Issue{Number: 1, Labels: []string{"triaged", dispatch.DispatchPlan}, Comments: []triage.Comment{{ID: 9, Author: "octo", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}},
	}

	if err := fixture.runner.applyDispatches(context.Background(), fixture.projectID, "acme/looper", "", []loadedIssue{stale}, triageCfg, dispatchCfg, dependencyState{}, downstreamTriggerLabels{}); err != nil {
		t.Fatalf("applyDispatches() error = %v", err)
	}
	if len(fixture.github.ops) != 0 {
		t.Fatalf("dispatch consumed stale snapshot: ops=%v", fixture.github.ops)
	}
}
