package runtime

import (
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

// ReviewPublishReadiness is the operator-facing view of whether the daemon can
// mint a trusted `looper review submit` wrapper for reviewer publishing.
type ReviewPublishReadiness struct {
	// LooperPath is the resolved tools.looperPath (empty when unset).
	LooperPath string `json:"looperPath,omitempty"`
	// Capable is true when the last capability probe accepted the binary.
	Capable bool `json:"capable"`
	// Capability is the probe token when Capable is true (e.g. review-submit/1).
	Capability string `json:"capability,omitempty"`
	// PublishingDisabled is true when reviewer publishing is fail-closed.
	PublishingDisabled bool `json:"publishingDisabled"`
	// Reason explains a non-capable probe (or missing path). Empty when Capable.
	Reason string `json:"reason,omitempty"`
}

// ReviewPublishReadinessFor reports review-publish readiness for cfg. The probe
// is cached by binary identity (same cache as the scheduler path).
func ReviewPublishReadinessFor(cfg config.Config) ReviewPublishReadiness {
	path := strings.TrimSpace(derefString(cfg.Tools.LooperPath))
	if path == "" {
		return ReviewPublishReadiness{
			PublishingDisabled: true,
			Reason:             "trusted looper path is not configured",
		}
	}
	capable, reason, _ := trustedReviewCapability(path)
	out := ReviewPublishReadiness{
		LooperPath:         path,
		Capable:            capable,
		PublishingDisabled: !capable,
		Reason:             reason,
	}
	if capable {
		out.Capability = forge.TrustedReviewCapabilityToken
		out.Reason = ""
	}
	return out
}
