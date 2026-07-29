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
