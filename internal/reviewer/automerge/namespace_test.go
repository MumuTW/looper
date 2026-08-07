package automerge

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
)

func TestDecideForNamespaceScopesOptInToProjectLabels(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewerAutoMergeConfig{Enabled: true, Strategy: config.ReviewerAutoMergeStrategySquash}
	protection := BranchProtectionSnapshot{Exists: true, HasRequiredChecks: true}
	settings := RepoSettingsSnapshot{AllowSquashMerge: true, AllowAutoMerge: true}
	namespace := labels.NewNamespace("team.looper:")

	if got := DecideForNamespace(PRSnapshot{Labels: []string{"team.looper:worker-ready"}, HasTrackedIssueLink: true}, cfg, protection, settings, namespace); got.Reason != "" {
		t.Fatalf("custom namespace decision = %#v, want opt-in", got)
	}
	if got := DecideForNamespace(PRSnapshot{Labels: []string{labels.DefaultWorkerReadyTrigger}, HasTrackedIssueLink: true}, cfg, protection, settings, namespace); got.Reason != RefusalReasonScope {
		t.Fatalf("default namespace decision = %#v, want scope refusal", got)
	}
}
