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
	// Known is true when the scheduler has recorded a capability verdict. An
	// unprobed configured binary is deliberately distinct from an incapable one.
	Known bool `json:"known"`
	// Capable is true when the last capability probe accepted the binary.
	Capable bool `json:"capable"`
	// Capability is the probe token when Capable is true (e.g. review-submit/1).
	Capability string `json:"capability,omitempty"`
	// PublishingDisabled is true when reviewer publishing is fail-closed.
	PublishingDisabled bool `json:"publishingDisabled"`
	// Reason explains a non-capable probe (or missing path). Empty when Capable.
	Reason string `json:"reason,omitempty"`
}

// ReviewPublishReadinessFor reports the scheduler's cached review-publish
// readiness for cfg. It never probes the configured binary itself.
func ReviewPublishReadinessFor(cfg config.Config) ReviewPublishReadiness {
	path := strings.TrimSpace(derefString(cfg.Tools.LooperPath))
	if path == "" {
		return ReviewPublishReadiness{
			Known:              true,
			PublishingDisabled: true,
			Reason:             "trusted looper path is not configured",
		}
	}
	capable, reason, known := trustedReviewCapabilityCached(path)
	if !known {
		return ReviewPublishReadiness{
			LooperPath:         path,
			PublishingDisabled: true,
			Reason:             "capability has not been probed yet",
		}
	}
	out := ReviewPublishReadiness{
		LooperPath:         path,
		Known:              true,
		Capable:            capable,
		PublishingDisabled: !capable,
		Reason:             safeReviewPublishReason(reason),
	}
	if capable {
		out.Capability = forge.TrustedReviewCapabilityToken
		out.Reason = ""
	}
	return out
}

// safeReviewPublishReason intentionally classifies the scheduler diagnostic
// instead of returning it. Probe stdout/stderr can contain tool output or
// secrets and is only suitable for the daemon's local logs.
func safeReviewPublishReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "configured looper binary capability is unavailable"
	}
	switch {
	case strings.HasPrefix(reason, "resolve looper binary:"):
		return "configured looper binary is unavailable"
	case strings.HasPrefix(reason, "`looper review capability` reported"):
		return "configured looper binary does not support review submit"
	case strings.HasPrefix(reason, "`looper review capability` failed"):
		return "configured looper binary capability probe failed"
	default:
		return "configured looper binary capability is unavailable"
	}
}
