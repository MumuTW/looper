package loops

import (
	"context"
	"testing"
)

func TestTargetLeaseCapabilityRoundTrip(t *testing.T) {
	capability := TargetLeaseCapability{Key: "project_1|pull_request:acme/looper:42", OwnerToken: "opaque"}
	got, ok := TargetLeaseCapabilityFromContext(WithTargetLeaseCapability(context.Background(), capability))
	if !ok || got != capability {
		t.Fatalf("TargetLeaseCapabilityFromContext() = (%#v, %v), want (%#v, true)", got, ok, capability)
	}
}
