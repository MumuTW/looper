package config

import "github.com/MumuTW/looper/internal/labels"

// LabelsMatch reports whether itemLabels satisfy required under mode.
//
// It lives here because LabelMode does: the roles all import config already,
// and labels cannot import config, so this is the one place the two meet
// without a new package in between.
//
// There were five copies of this, one per role, and they had drifted. Four
// compared through labels.Has, which normalizes both sides; worker used a
// local matcher that folded case on the observed label but not on the
// configured one, so a trigger label written with a stray space in config —
// nothing trims those — matched everywhere except worker, and worker silently
// claimed nothing.
func LabelsMatch(itemLabels []string, required []string, mode LabelMode) bool {
	if len(required) == 0 {
		return true
	}
	if mode == LabelModeAny {
		for _, label := range required {
			if labels.Has(itemLabels, label) {
				return true
			}
		}
		return false
	}
	for _, label := range required {
		if !labels.Has(itemLabels, label) {
			return false
		}
	}
	return true
}
