// Package hitl holds small shared helpers for mid-run human-in-the-loop asks
// used by worker and fixer. Persistence stays on loops.HITLAsk.
package hitl

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// FingerprintContent returns a stable sha256 hex digest of the joined parts.
// Empty parts are included so callers can pass optional fields without branching.
func FingerprintContent(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(strings.TrimSpace(part)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MaterialFingerprintsMatch reports whether the drift-detection fingerprints on
// a prior ask still match the live values. Empty stored fields are treated as
// "not checked" so worker asks (no fingerprints) always match.
func MaterialFingerprintsMatch(storedHead, liveHead, storedReviewFP, liveReviewFP, storedIntentFP, liveIntentFP string) bool {
	if stored := strings.TrimSpace(storedHead); stored != "" && stored != strings.TrimSpace(liveHead) {
		return false
	}
	if stored := strings.TrimSpace(storedReviewFP); stored != "" && stored != strings.TrimSpace(liveReviewFP) {
		return false
	}
	if stored := strings.TrimSpace(storedIntentFP); stored != "" && stored != strings.TrimSpace(liveIntentFP) {
		return false
	}
	return true
}
