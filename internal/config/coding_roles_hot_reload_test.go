package config

import "testing"

func TestRestartRequiredChangesFlagsAuthoredCodingRoleOverlay(t *testing.T) {
	t.Parallel()
	old := mustNormalize(t)
	updated := old
	updated.Roles.Coding = cloneCodingRoleRegistry(old.Roles.Coding)
	role := updated.Roles.Coding[CodingRoleWorker]
	role.Discovery.Labels = []string{"changed"}
	updated.Roles.Coding[CodingRoleWorker] = role
	if got := RestartRequiredChanges(old, updated); !hasIssuePath(got, "roles.coding.worker") {
		t.Fatalf("RestartRequiredChanges = %v, want registry restart guard", got)
	}
}

func TestRestartRequiredChangesLeavesLegacyProjectionToExistingPolicy(t *testing.T) {
	t.Parallel()
	old := mustNormalize(t)
	updated := old
	updated.Roles.Worker.Instructions = "legacy hot guidance"
	updated.Roles.Coding = CodingRolesFromLegacy(updated.Roles)
	if got := RestartRequiredChanges(old, updated); hasIssuePath(got, "roles.coding.worker") {
		t.Fatalf("RestartRequiredChanges = %v, legacy projection must not add registry guard", got)
	}
}

func TestRestartRequiredChangesIgnoresAuthoredFieldsUnchangedByLegacyHotEdit(t *testing.T) {
	t.Parallel()
	instructions := "authored worker guidance"
	labels := []string{"changed"}
	authored := map[string]PartialCodingRoleConfig{
		CodingRoleWorker: {Instructions: &instructions},
	}
	old := mustNormalize(t, PartialConfig{
		Roles: &PartialRoleConfigs{Coding: authored},
	})
	updated := mustNormalize(t, PartialConfig{
		Roles: &PartialRoleConfigs{
			Coding: authored,
			Worker: &PartialWorkerRoleConfig{
				Triggers: &PartialIssueRoleTriggersConfig{Labels: &labels},
			},
		},
	})

	if got := RestartRequiredChanges(old, updated); hasIssuePath(got, "roles.coding.worker") {
		t.Fatalf("RestartRequiredChanges = %v, unchanged authored instructions must not make a legacy labels edit restart-bound", got)
	}
}

func TestReviewerSelfReviewOverrideDoesNotInsertMissingCodingRole(t *testing.T) {
	t.Parallel()
	value := true
	config := Config{
		Roles: RoleConfigs{
			Coding: map[string]CodingRoleConfig{
				CodingRoleWorker: {Priority: PriorityWorker},
			},
		},
	}
	partial := PartialConfig{
		Roles: &PartialRoleConfigs{
			Reviewer: &PartialReviewerRoleConfig{
				Triggers: &PartialReviewerRoleTriggersConfig{EnableSelfReview: &value},
			},
		},
	}

	applyGlobalReviewerEnableSelfReviewOverride(&config, partial)

	if _, ok := config.Roles.Coding[CodingRoleReviewer]; ok {
		t.Fatal("reviewer coding role was inserted into a registry that did not contain it")
	}
	if got := len(config.Roles.Coding); got != 1 {
		t.Fatalf("coding role registry length = %d, want 1", got)
	}
}
