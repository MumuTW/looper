package reviewer

import (
	"reflect"
	"testing"

	"github.com/nexu-io/looper/internal/labels"
)

func TestCriteriaFailureLabelsOnlyRemovesOwnedDispatchLabels(t *testing.T) {
	t.Parallel()

	issueLabels := []string{"triaged", labels.DispatchPlan, "dispatch/plan", "other"}
	if got, want := criteriaFailureLabels(issueLabels), []string{"triaged", labels.DispatchPlan}; !reflect.DeepEqual(got, want) {
		t.Fatalf("criteriaFailureLabels() = %v, want %v", got, want)
	}
}

func TestIssueHasCoordinatorTrackingIgnoresBareDispatchLabels(t *testing.T) {
	t.Parallel()

	if issueHasCoordinatorTracking([]string{"dispatch/plan"}) {
		t.Fatal("issueHasCoordinatorTracking() = true for foreign bare dispatch label")
	}
	if !issueHasCoordinatorTracking([]string{labels.DispatchPlan}) {
		t.Fatal("issueHasCoordinatorTracking() = false for Looper dispatch label")
	}
}
