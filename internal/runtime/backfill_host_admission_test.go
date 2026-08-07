package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	coordinatorrole "github.com/MumuTW/looper/internal/coordinator"
	"github.com/MumuTW/looper/internal/hostresources"
)

func TestRuntimeBackfillRefusesHostPressureBeforeCoordinatorWork(t *testing.T) {
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Daemon.ResourceGuard.Enabled = true
	cfg.Daemon.ResourceGuard.MinDiskFreeGB = 1

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	rt.hostAdmission.read = func(string) hostresources.Snapshot {
		free := uint64(1)
		return hostresources.Snapshot{DiskFreeBytes: &free}
	}
	called := false
	rt.backfillIssues = func(context.Context, coordinatorrole.BackfillInput) (coordinatorrole.BackfillResult, error) {
		called = true
		return coordinatorrole.BackfillResult{}, nil
	}

	_, err = rt.BackfillIssues(context.Background(), coordinatorrole.BackfillInput{ProjectID: "project", Repo: "acme/looper"})
	if !errors.Is(err, coordinatorrole.ErrBackfillUnavailable) {
		t.Fatalf("BackfillIssues() error = %v, want host admission refusal", err)
	}
	if called {
		t.Fatal("backfill callback ran while host admission was refusing work")
	}
}
