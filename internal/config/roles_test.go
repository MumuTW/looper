package config

import "testing"

func TestCodingRolesFromLegacyProjectsSourceAndFields(t *testing.T) {
	roles := RoleConfigs{
		Planner: PlannerRoleConfig{
			AutoDiscovery: true,
			Instructions:  "plan carefully",
			Triggers: IssueRoleTriggersConfig{
				Labels:                     []string{"looper:plan"},
				LabelMode:                  LabelModeAll,
				RequireAssigneeCurrentUser: true,
				PlaneAssigneeID:            "uuid-1",
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
	if planner.Discovery.PlaneAssigneeID != "uuid-1" {
		t.Errorf("planner planeAssigneeId = %q, want uuid-1", planner.Discovery.PlaneAssigneeID)
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
				PlaneAssigneeID:            "uuid-1",
			},
			wantCount: 2,
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
				Labels:                     []string{"looper:plan"},
			},
			wantCount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateRoleDiscovery("myrole", tc.discovery)
			if len(issues) != tc.wantCount {
				t.Errorf("got %d issues %v, want %d", len(issues), issues, tc.wantCount)
			}
		})
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
