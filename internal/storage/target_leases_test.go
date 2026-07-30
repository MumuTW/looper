package storage

import (
	"context"
	"testing"
)

func TestTargetLeasesAcquireAndReleaseRequireOwnerToken(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	first := TargetLeaseRecord{
		TargetKey:  "project_1|pull_request:acme/looper:42",
		OwnerToken: "opaque-first",
		OwnerKind:  "automation",
		OwnerID:    "queue_1",
		Purpose:    "claim",
		AcquiredAt: "2026-07-31T08:00:00.000Z",
		UpdatedAt:  "2026-07-31T08:00:00.000Z",
	}
	acquired, err := repos.TargetLeases.Acquire(ctx, first)
	if err != nil || !acquired {
		t.Fatalf("Acquire(first) = (%v, %v), want (true, nil)", acquired, err)
	}

	second := first
	second.OwnerToken = "opaque-second"
	second.OwnerID = "queue_2"
	acquired, err = repos.TargetLeases.Acquire(ctx, second)
	if err != nil || acquired {
		t.Fatalf("Acquire(second) = (%v, %v), want (false, nil)", acquired, err)
	}

	released, err := repos.TargetLeases.Release(ctx, first.TargetKey, second.OwnerToken)
	if err != nil || released {
		t.Fatalf("Release(stale token) = (%v, %v), want (false, nil)", released, err)
	}
	got, err := repos.TargetLeases.Get(ctx, first.TargetKey)
	if err != nil || got == nil || got.OwnerToken != first.OwnerToken || got.OwnerID != first.OwnerID {
		t.Fatalf("Get() = (%#v, %v), want first holder", got, err)
	}

	released, err = repos.TargetLeases.Release(ctx, first.TargetKey, first.OwnerToken)
	if err != nil || !released {
		t.Fatalf("Release(holder token) = (%v, %v), want (true, nil)", released, err)
	}
	acquired, err = repos.TargetLeases.Acquire(ctx, second)
	if err != nil || !acquired {
		t.Fatalf("Acquire(second after release) = (%v, %v), want (true, nil)", acquired, err)
	}
}
