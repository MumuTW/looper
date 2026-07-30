package loops

import "context"

// TargetLeaseCapability is the opaque lease capability a scheduler-held role
// may use while touching its checkout. It is intentionally carried separately
// from loop status: the durable target_leases row remains the authority.
type TargetLeaseCapability struct {
	Key        string
	OwnerToken string
}

type targetLeaseContextKey struct{}

// WithTargetLeaseCapability attaches the already-acquired lease to the
// processor context. Empty capabilities are omitted so non-worktree roles do
// not accidentally look authorized.
func WithTargetLeaseCapability(ctx context.Context, capability TargetLeaseCapability) context.Context {
	if ctx == nil || capability.Key == "" || capability.OwnerToken == "" {
		return ctx
	}
	return context.WithValue(ctx, targetLeaseContextKey{}, capability)
}

// TargetLeaseCapabilityFromContext returns the scheduler-held capability, if
// this call remains inside the processor context that acquired it.
func TargetLeaseCapabilityFromContext(ctx context.Context) (TargetLeaseCapability, bool) {
	if ctx == nil {
		return TargetLeaseCapability{}, false
	}
	capability, ok := ctx.Value(targetLeaseContextKey{}).(TargetLeaseCapability)
	if !ok || capability.Key == "" || capability.OwnerToken == "" {
		return TargetLeaseCapability{}, false
	}
	return capability, true
}
