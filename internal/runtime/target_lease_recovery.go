package runtime

import (
	"context"
	"strings"

	"github.com/nexu-io/looper/internal/processidentity"
	"github.com/nexu-io/looper/internal/storage"
)

// reclaimVerifiablyGoneTargetLeases settles only automation leases whose
// persisted process identity proves they cannot still own the checkout. A
// missing identity, probe failure, or matching process is deliberately kept:
// elapsed time and a successful PID lookup are not authority to reclaim.
func (r *Runtime) reclaimVerifiablyGoneTargetLeases(ctx context.Context, repos *storage.Repositories) (int64, error) {
	if repos == nil || repos.TargetLeases == nil {
		return 0, nil
	}
	leases, err := repos.TargetLeases.List(ctx)
	if err != nil {
		return 0, err
	}
	var reclaimed int64
	for _, lease := range leases {
		if lease.OwnerKind != "automation" || lease.ProcessPID == nil || lease.ProcessStartTime == nil || *lease.ProcessPID <= 0 || *lease.ProcessStartTime <= 0 {
			continue
		}
		if processidentity.RequiresBootID() && (lease.ProcessBootID == nil || strings.TrimSpace(*lease.ProcessBootID) == "") {
			continue
		}
		pid := int(*lease.ProcessPID)
		command, commandErr := r.readProcessCommand(ctx, pid)
		if commandErr != nil {
			continue
		}
		gone := strings.TrimSpace(command) == ""
		if !gone {
			start, startErr := r.readProcessStart(ctx, pid)
			if startErr != nil {
				continue
			}
			gone = start != *lease.ProcessStartTime
		}
		if !gone && lease.ProcessBootID != nil && strings.TrimSpace(*lease.ProcessBootID) != "" {
			bootID, bootErr := r.readProcessBootID(ctx, pid)
			if bootErr != nil {
				continue
			}
			gone = strings.TrimSpace(bootID) != strings.TrimSpace(*lease.ProcessBootID)
		}
		if !gone {
			continue
		}
		released, releaseErr := repos.TargetLeases.Release(ctx, lease.TargetKey, lease.OwnerToken)
		if releaseErr != nil {
			return reclaimed, releaseErr
		}
		if released {
			reclaimed++
		}
	}
	return reclaimed, nil
}
