package config

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/labels"
)

func TestCodingRolesFromLegacyProjectsSourceAndFields(t *testing.T) {
	roles := RoleConfigs{
		Planner: PlannerRoleConfig{
			AutoDiscovery: true,
			Instructions:  "plan carefully",
			Triggers: IssueRoleTriggersConfig{
				Labels:                     []string{labels.DefaultPlanTrigger},
				LabelMode:                  LabelModeAll,
				RequireAssigneeCurrentUser: true,
			},
		},
		Fixer: FixerRoleConfig{
			AutoDiscovery: true,
			Triggers: FixerRoleTriggersConfig{
				IncludeDrafts: false,
				AuthorFilter:  FixerAuthorFilterCurrentUser,
				Labels:        []string{},
				LabelMode:     LabelModeAll,
			},
		},
	}

	coding := CodingRolesFromLegacy(roles)

	planner, ok := coding[CodingRolePlanner]
	if !ok {
		t.Fatalf("planner missing from projected roles")
	}
	if planner.Discovery.Source != WorkSourceIssue {
		t.Errorf("planner source = %q, want %q", planner.Discovery.Source, WorkSourceIssue)
	}
	if !planner.Discovery.RequireAssigneeCurrentUser {
		t.Errorf("planner requireAssigneeCurrentUser was not carried over")
	}
	if planner.Instructions != "plan carefully" {
		t.Errorf("planner instructions = %q", planner.Instructions)
	}

	fixer, ok := coding[CodingRoleFixer]
	if !ok {
		t.Fatalf("fixer missing from projected roles")
	}
	if fixer.Discovery.Source != WorkSourcePullRequest {
		t.Errorf("fixer source = %q, want %q", fixer.Discovery.Source, WorkSourcePullRequest)
	}
	if fixer.Discovery.AuthorFilter != AuthorFilterCurrentUser {
		t.Errorf("fixer authorFilter = %q, want %q", fixer.Discovery.AuthorFilter, AuthorFilterCurrentUser)
	}

	gatekeeper, ok := coding[RoleGatekeeper]
	if !ok {
		t.Fatal("gatekeeper missing from projected roles")
	}
	if !gatekeeper.Discovery.Enabled || gatekeeper.Discovery.Source != WorkSourcePullRequest {
		t.Fatalf("gatekeeper discovery = %#v, want enabled pull-request source", gatekeeper.Discovery)
	}
	if gatekeeper.Discovery.LabelMode != LabelModeAll {
		t.Fatalf("gatekeeper label mode = %q, want %q", gatekeeper.Discovery.LabelMode, LabelModeAll)
	}
	if len(gatekeeper.Discovery.Labels) != 0 {
		t.Fatalf("gatekeeper labels = %v, want source-based discovery without label gate", gatekeeper.Discovery.Labels)
	}
}

// Labels must be copied, not aliased: the scheduler hands discovery inputs to
// concurrent lanes, and a shared backing array would let one role's lane
// mutate another's trigger list.
func TestCodingRolesFromLegacyCopiesLabels(t *testing.T) {
	roles := RoleConfigs{
		Planner: PlannerRoleConfig{
			Triggers: IssueRoleTriggersConfig{Labels: []string{"a", "b"}},
		},
	}

	coding := CodingRolesFromLegacy(roles)
	coding[CodingRolePlanner].Discovery.Labels[0] = "mutated"

	if roles.Planner.Triggers.Labels[0] != "a" {
		t.Errorf("legacy labels were aliased; got %q", roles.Planner.Triggers.Labels[0])
	}
}

func TestValidateRoleDiscoveryRejectsCrossSourceFields(t *testing.T) {
	cases := []struct {
		name      string
		discovery RoleDiscoveryConfig
		wantCount int
	}{
		{
			name: "issue source rejects pr-only fields",
			discovery: RoleDiscoveryConfig{
				Source:               WorkSourceIssue,
				IncludeDrafts:        true,
				AuthorFilter:         AuthorFilterCurrentUser,
				RequireReviewRequest: true,
				EnableSelfReview:     true,
			},
			wantCount: 4,
		},
		{
			name: "pull_request source rejects issue-only fields",
			discovery: RoleDiscoveryConfig{
				Source:                     WorkSourcePullRequest,
				RequireAssigneeCurrentUser: true,
			},
			wantCount: 1,
		},
		{
			name:      "unknown source is rejected",
			discovery: RoleDiscoveryConfig{Source: "epic"},
			wantCount: 1,
		},
		{
			name: "matching source passes",
			discovery: RoleDiscoveryConfig{
				Source:                     WorkSourceIssue,
				RequireAssigneeCurrentUser: true,
				Labels:                     []string{labels.DefaultPlanTrigger},
			},
			wantCount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateRoleDiscovery("roles.coding.myrole", tc.discovery)
			if len(issues) != tc.wantCount {
				t.Errorf("got %d issues %v, want %d", len(issues), issues, tc.wantCount)
			}
			for _, issue := range issues {
				if !strings.HasPrefix(issue.Path, "roles.coding.myrole.") {
					t.Errorf("issue path %q does not point into roles.coding.myrole", issue.Path)
				}
			}
		})
	}
}

// Lane order comes from priority, not the role name. A custom role must be
// able to sit between two shipped roles, which alphabetical ordering could
// never express.
func TestCodingRoleNamesOrdersByPriority(t *testing.T) {
	roles := RoleConfigs{Coding: map[string]CodingRoleConfig{
		"worker":   {Priority: PriorityWorker},
		"planner":  {Priority: PriorityPlanner},
		"auditor":  {Priority: PriorityReviewer - 1},
		"fixer":    {Priority: PriorityFixer},
		"reviewer": {Priority: PriorityReviewer},
	}}

	got := CodingRoleNames(roles)
	want := []string{"planner", "auditor", "reviewer", "fixer", "worker"}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lane order = %v, want %v", got, want)
		}
	}
}

// A Config that never went through Normalize must still yield the shipped
// roles: presenting zero roles would read as "discovery is off" rather than
// "this config was assembled directly".
func TestEffectiveCodingRolesFallsBackToLegacy(t *testing.T) {
	roles := RoleConfigs{Planner: PlannerRoleConfig{AutoDiscovery: true}}

	effective := EffectiveCodingRoles(roles)

	if len(effective) != 5 {
		t.Fatalf("got %d roles, want the 5 shipped roles", len(effective))
	}
	if !effective[CodingRolePlanner].Discovery.Enabled {
		t.Errorf("planner autoDiscovery was not carried into the fallback")
	}
}

func TestCodingRoleNamesIsSorted(t *testing.T) {
	roles := RoleConfigs{Coding: map[string]CodingRoleConfig{
		"worker": {}, "planner": {}, "auditor": {},
	}}

	got := CodingRoleNames(roles)
	want := []string{"auditor", "planner", "worker"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
