package coordinator

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/dispatch"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestAssignPersonalIssueIfEligibleAssignsSelfAuthoredIssueIdempotently(t *testing.T) {
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Projects[0].PersonalProject = true
	})
	fixture.github.currentLogin = "looper"
	dispatchCfg := dispatch.Config{WorkerTriggerLabels: []string{labels.DefaultWorkerReadyTrigger}}
	action := dispatch.Action{TriggerLabels: []string{labels.DefaultWorkerReadyTrigger}}
	summary := githubinfra.IssueSummary{
		Number: 701,
		Author: "looper",
		Labels: []string{labels.DefaultWorkerReadyTrigger},
		Title:  "Personal implementation",
	}

	assigned, err := fixture.runner.assignPersonalIssueIfEligible(context.Background(), fixture.projectID, "acme/looper", "", summary, action, dispatchCfg)
	if err != nil {
		t.Fatalf("assignPersonalIssueIfEligible() error = %v", err)
	}
	if len(assigned.Assignees) != 1 || assigned.Assignees[0] != "looper" || len(fixture.github.assigned) != 1 {
		t.Fatalf("assigned = %#v, github assignments = %#v, want one self-assignment", assigned, fixture.github.assigned)
	}

	assignedAgain, err := fixture.runner.assignPersonalIssueIfEligible(context.Background(), fixture.projectID, "acme/looper", "", assigned, action, dispatchCfg)
	if err != nil {
		t.Fatalf("second assignPersonalIssueIfEligible() error = %v", err)
	}
	if len(assignedAgain.Assignees) != 1 || len(fixture.github.assigned) != 1 {
		t.Fatalf("second assigned = %#v, github assignments = %#v, want idempotent self-assignment", assignedAgain, fixture.github.assigned)
	}
}

func TestAssignPersonalIssueIfEligiblePreservesSharedAndExplicitAssignments(t *testing.T) {
	tests := map[string]struct {
		personal bool
		author   string
		assignee []string
		action   dispatch.Action
	}{
		"shared project":      {personal: false, author: "looper"},
		"author mismatch":     {personal: true, author: "someone-else"},
		"existing assignee":   {personal: true, author: "looper", assignee: []string{"teammate"}},
		"explicit assignment": {personal: true, author: "looper", action: dispatch.Action{TriggerLabels: []string{labels.DefaultWorkerReadyTrigger}, AssignTo: "teammate"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
				cfg.Projects[0].PersonalProject = test.personal
			})
			fixture.github.currentLogin = "looper"
			action := test.action
			if len(action.TriggerLabels) == 0 {
				action.TriggerLabels = []string{labels.DefaultWorkerReadyTrigger}
			}
			got, err := fixture.runner.assignPersonalIssueIfEligible(context.Background(), fixture.projectID, "acme/looper", "", githubinfra.IssueSummary{Number: 702, Author: test.author, Assignees: test.assignee}, action, dispatch.Config{WorkerTriggerLabels: []string{labels.DefaultWorkerReadyTrigger}})
			if err != nil {
				t.Fatalf("assignPersonalIssueIfEligible() error = %v", err)
			}
			if len(fixture.github.assigned) != 0 || len(got.Assignees) != len(test.assignee) {
				t.Fatalf("got = %#v, github assignments = %#v, want no automatic assignment", got, fixture.github.assigned)
			}
		})
	}
}
