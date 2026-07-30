package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/labels"
)

func TestLabelsMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		itemLabels []string
		required   []string
		mode       LabelMode
		want       bool
	}{
		{name: "no requirement matches anything", itemLabels: nil, required: nil, mode: LabelModeAll, want: true},
		{name: "all present", itemLabels: []string{"a", "b"}, required: []string{"a", "b"}, mode: LabelModeAll, want: true},
		{name: "all missing one", itemLabels: []string{"a"}, required: []string{"a", "b"}, mode: LabelModeAll, want: false},
		{name: "any present", itemLabels: []string{"b"}, required: []string{"a", "b"}, mode: LabelModeAny, want: true},
		{name: "any none present", itemLabels: []string{"c"}, required: []string{"a", "b"}, mode: LabelModeAny, want: false},

		// Nothing trims a configured trigger label, and forge label names are
		// unique case-insensitively, so both sides are normalized. Worker used
		// to normalize only the observed label and therefore claimed nothing
		// when a project's config carried a stray space.
		{name: "configured label with a stray space still matches", itemLabels: []string{labels.DefaultWorkerReadyTrigger}, required: []string{fmt.Sprintf(" %s ", labels.DefaultWorkerReadyTrigger)}, mode: LabelModeAll, want: true},
		{name: "observed label with a stray space still matches", itemLabels: []string{fmt.Sprintf(" %s ", labels.DefaultWorkerReadyTrigger)}, required: []string{labels.DefaultWorkerReadyTrigger}, mode: LabelModeAll, want: true},
		{name: "case differences still match", itemLabels: []string{strings.ToUpper(labels.DefaultWorkerReadyTrigger)}, required: []string{labels.DefaultWorkerReadyTrigger}, mode: LabelModeAll, want: true},
		{name: "a different label still does not match", itemLabels: []string{labels.DefaultWorkerReadyTrigger + ":extra"}, required: []string{labels.DefaultWorkerReadyTrigger}, mode: LabelModeAll, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := LabelsMatch(test.itemLabels, test.required, test.mode); got != test.want {
				t.Fatalf("LabelsMatch(%q, %q, %q) = %v, want %v", test.itemLabels, test.required, test.mode, got, test.want)
			}
		})
	}
}
