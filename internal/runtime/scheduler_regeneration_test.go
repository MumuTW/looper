package runtime

import (
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestFixerRegenerationUnavailableWhenPlannerCannotPublish(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		allowAutoPush     bool
		plannerConfigured bool
		plannerAvailable  bool
		wantReason        string
	}{
		{name: "auto push disabled", allowAutoPush: false, plannerConfigured: true, plannerAvailable: true, wantReason: "allowAutoPush=false"},
		{name: "planner missing", allowAutoPush: true, plannerConfigured: false, plannerAvailable: false, wantReason: "not configured"},
		{name: "planner runner unavailable", allowAutoPush: true, plannerConfigured: true, plannerAvailable: false, wantReason: "not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Defaults.AllowAutoPush = tc.allowAutoPush
			got := fixerRegenerationUnavailableReason(cfg, tc.plannerConfigured, tc.plannerAvailable)
			if !strings.Contains(got, tc.wantReason) {
				t.Fatalf("fixerRegenerationUnavailableReason() = %q, want substring %q", got, tc.wantReason)
			}
		})
	}
}

func TestFixerRegenerationAvailableWhenPlannerCanPublish(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Defaults.AllowAutoPush = true
	if got := fixerRegenerationUnavailableReason(cfg, true, true); got != "" {
		t.Fatalf("fixerRegenerationUnavailableReason() = %q, want available", got)
	}
}
