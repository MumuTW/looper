package coordinator

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
)

func TestIssueDispatchLabelPrefersConfiguredLabelOverLegacyCompatibilityLabel(t *testing.T) {
	namespace := labels.NewNamespace("team.looper:")
	got, ok := issueDispatchLabelForNamespace([]string{"dispatch/plan", "team.looper:dispatch:plan"}, namespace)
	if !ok || got != "team.looper:dispatch:plan" {
		t.Fatalf("issueDispatchLabelForNamespace() = %q, %t, want configured dispatch label", got, ok)
	}
}

func TestRetriageCleanupPatternsStayInsideConfiguredNamespace(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	namespace := labels.NewNamespace("team.looper:")
	patterns := retriageCleanupPatterns(cfg.Roles, "triaged", namespace)
	if containsExact(patterns, "dispatch/plan") || containsExact(patterns, labels.DispatchPlan) {
		t.Fatalf("cleanup patterns = %v, want no bare/default dispatch labels", patterns)
	}
	if !containsExact(patterns, namespace.DispatchPlan()) {
		t.Fatalf("cleanup patterns = %v, want %q", patterns, namespace.DispatchPlan())
	}
}

func TestTriageCompletionLabelUsesProjectNamespace(t *testing.T) {
	if got := triageCompletionLabel("triaged", labels.DefaultNamespace()); got != "triaged" {
		t.Fatalf("default triage completion label = %q, want bare triaged", got)
	}
	custom := labels.NewNamespace("team.looper:")
	if got := triageCompletionLabel("triaged", custom); got != "team.looper:triaged" {
		t.Fatalf("custom triage completion label = %q, want namespaced marker", got)
	}
	if got := triageCompletionLabel("team.looper:triaged", custom); got != "team.looper:triaged" {
		t.Fatalf("already namespaced completion label = %q, want unchanged marker", got)
	}
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
