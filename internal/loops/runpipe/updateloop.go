package runpipe

import (
	"context"
	"fmt"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

// UpdateLoopOptions selects the per-runner semantics of UpdateLoop. Each
// flag preserves a behavior one or more runners already had; the census on
// #537 records which runner uses which and why the differences are (or are
// not) load-bearing.
type UpdateLoopOptions struct {
	// RequireExists refuses the update when the loop record is missing,
	// instead of falling back to the caller's copy and effectively
	// resurrecting it. The planner runs strict: a missing planner loop is a
	// bug, not a recovery case.
	RequireExists bool
	// GuardMetadata validates that the PRE-mutation metadata decodes as an
	// object whenever the mutation changes metadata, so a malformed stored
	// value blocks the write instead of being silently replaced (the fail-
	// loud line from #86). Reviewer and fixer run with the guard.
	GuardMetadata bool
	// MonotonicUpdatedAt stamps UpdatedAt strictly after the previous
	// value even when the clock has not advanced, preserving update
	// ordering under rapid successive writes. The fixer runs monotonic.
	MonotonicUpdatedAt bool
}

// UpdateLoop re-reads the loop, applies mutate to the freshest copy, and
// upserts — never resurrecting a terminated loop, which short-circuits
// untouched. It is the shared shape behind every runner's updateLoop; the
// options carry the semantics that differ per runner.
func UpdateLoop(ctx context.Context, repos *storage.Repositories, now func() time.Time, loop storage.LoopRecord, opts UpdateLoopOptions, mutate func(*storage.LoopRecord)) (storage.LoopRecord, error) {
	current, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if current == nil && opts.RequireExists {
		return storage.LoopRecord{}, fmt.Errorf("loop not found: %s", loop.ID)
	}
	if current != nil && current.Status == "terminated" {
		return *current, nil
	}
	updated := loop
	if current != nil {
		updated = *current
	}
	metadataBefore := updated.MetadataJSON
	mutate(&updated)
	if opts.GuardMetadata && derefValue(metadataBefore) != derefValue(updated.MetadataJSON) {
		if _, err := loops.DecodeMetadataObjectForWrite(metadataBefore); err != nil {
			return storage.LoopRecord{}, err
		}
	}
	if opts.MonotonicUpdatedAt {
		updated.UpdatedAt = eventlog.NextJavaScriptISOString(now(), updated.UpdatedAt)
	} else {
		updated.UpdatedAt = eventlog.FormatJavaScriptISOString(now())
	}
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return storage.LoopRecord{}, err
	}
	return updated, nil
}

func derefValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
