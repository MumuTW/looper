package config

import (
	"testing"

	"github.com/MumuTW/looper/internal/labels"
)

func TestPostMergeDigestConfigNormalizesAndMerges(t *testing.T) {
	enabled, includeEmpty, maxItems := true, true, 25
	schedule, timezone := "08:30", "Asia/Taipei"
	cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{PostMergeDigest: &PartialCoordinatorPostMergeDigestConfig{
		Enabled: &enabled, Schedule: &schedule, Timezone: &timezone, IncludeEmpty: &includeEmpty, MaxItems: &maxItems,
	}}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	got := cfg.Roles.Coordinator.PostMergeDigest
	if got == nil || !got.Enabled || got.Schedule != schedule || got.Timezone != timezone || !got.IncludeEmpty || got.MaxItems != maxItems {
		t.Fatalf("post-merge digest = %#v, want normalized configured values", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestPostMergeDigestConfigDefaultsToDisabled(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Roles.Coordinator.PostMergeDigest != nil {
		t.Fatalf("default post-merge digest = %#v, want nil/disabled", cfg.Roles.Coordinator.PostMergeDigest)
	}
}

func TestPostMergeDigestConfigRejectsInvalidScheduleTimezoneAndLimit(t *testing.T) {
	issues := []ValidationIssue{}
	validateCoordinatorRoleConfig(CoordinatorRoleConfig{
		PollInterval:    "1m",
		Triage:          CoordinatorTriageConfig{TriagedLabel: "triaged", MaxIssueAgeDays: 1, MaxPerTick: 1, Disposition: CoordinatorTriageDispositionConfig{OutOfScopeLabel: "wontfix", UnclearLabel: "needs-info"}},
		Dispatch:        CoordinatorDispatchConfig{Mode: "human-gated", HumanGate: CoordinatorDispatchHumanGateConfig{SlashCommands: []string{"/plan"}}, Autonomous: CoordinatorDispatchAutonomousConfig{DelayMinutes: 1, HoldLabel: labels.HoldGlobal}},
		MergeWatch:      CoordinatorMergeWatchConfig{TransientRetries: 1, MaxIndeterminateDuration: "1m"},
		PostMergeDigest: &CoordinatorPostMergeDigestConfig{Enabled: true, Schedule: "25:99", Timezone: "Not/AZone", MaxItems: 0},
	}, "roles.coordinator", &issues)
	for _, want := range []string{"roles.coordinator.postMergeDigest.schedule", "roles.coordinator.postMergeDigest.timezone", "roles.coordinator.postMergeDigest.maxItems"} {
		found := false
		for _, issue := range issues {
			if issue.Path == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("issues = %#v, want %s validation", issues, want)
		}
	}
}

func TestClonePartialRoleConfigsPreservesPostMergeDigest(t *testing.T) {
	enabled, maxItems := true, 10
	schedule, timezone := "09:00", "UTC"
	original := &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{PostMergeDigest: &PartialCoordinatorPostMergeDigestConfig{Enabled: &enabled, Schedule: &schedule, Timezone: &timezone, MaxItems: &maxItems}}}
	cloned := clonePartialRoleConfigs(original)
	if cloned == nil || cloned.Coordinator == nil || cloned.Coordinator.PostMergeDigest == nil || cloned.Coordinator.PostMergeDigest.MaxItems == nil || *cloned.Coordinator.PostMergeDigest.MaxItems != maxItems {
		t.Fatalf("cloned post-merge digest = %#v", cloned)
	}
	*original.Coordinator.PostMergeDigest.MaxItems = 1
	if *cloned.Coordinator.PostMergeDigest.MaxItems != maxItems {
		t.Fatal("clone aliases post-merge digest pointer")
	}
}
