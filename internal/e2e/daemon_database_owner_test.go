package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/e2e/harness"
)

func TestLooperdDatabaseOwnershipIsScopedToSQLiteAuthority(t *testing.T) {
	bins := harness.MustBinaries(t)
	firstHome := harness.NewTempHome(t)
	firstPort := harness.MustFreePort(t)
	firstConfig := harness.DefaultConfig(t, firstHome, harness.ConfigOptions{Port: firstPort, Projects: []config.ProjectRefConfig{}})
	firstConfig.Webhook.Enabled = false
	harness.WriteConfig(t, firstHome.ConfigPath, firstConfig, nil)
	first := harness.StartLooperd(t, bins, firstHome, firstHome.ConfigPath, nil, firstConfig.Server.Host, firstConfig.Server.Port)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer readyCancel()
	if _, err := first.WaitForReady(readyCtx); err != nil {
		t.Fatalf("first daemon readiness: %v", err)
	}

	secondHome := harness.NewTempHome(t)
	secondPort := harness.MustFreePort(t)
	secondProjectID := "second-daemon-must-not-register"
	secondConfig := harness.DefaultConfig(t, secondHome, harness.ConfigOptions{
		Port: secondPort,
		Projects: []config.ProjectRefConfig{{
			ID:       secondProjectID,
			Name:     "Second daemon",
			Repo:     "acme/second",
			RepoPath: secondHome.WorkingDir,
		}},
	})
	secondConfig.Webhook.Enabled = false
	secondConfig.Storage.DBPath = firstConfig.Storage.DBPath
	secondConfig.Storage.BackupDir = &secondHome.BackupDir
	secondConfigPath := filepath.Join(secondHome.LooperHome, "same-database.json")
	harness.WriteConfig(t, secondConfigPath, secondConfig, nil)
	second := harness.StartLooperd(t, bins, secondHome, secondConfigPath, nil, secondConfig.Server.Host, secondConfig.Server.Port)
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer blockedCancel()
	if _, err := second.WaitForReady(blockedCtx); err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatalf("second daemon readiness error = %v, want fail-fast exit", err)
	}
	stderr, err := os.ReadFile(filepath.Join(secondHome.ArtifactsDir, "looperd.stderr.log"))
	if err != nil {
		t.Fatalf("read second daemon stderr: %v", err)
	}
	if !strings.Contains(string(stderr), "database compatibility lock is held") {
		t.Fatalf("second daemon stderr = %q, want database ownership failure", stderr)
	}

	_, firstRepos := openRepos(t, firstConfig.Storage.DBPath)
	project, err := firstRepos.Projects.GetByID(context.Background(), secondProjectID)
	if err != nil {
		t.Fatalf("lookup second daemon project: %v", err)
	}
	if project != nil {
		t.Fatalf("second daemon mutated shared database: project = %#v", project)
	}

	independentHome := harness.NewTempHome(t)
	independentPort := harness.MustFreePort(t)
	independentConfig := harness.DefaultConfig(t, independentHome, harness.ConfigOptions{Port: independentPort, Projects: []config.ProjectRefConfig{}})
	independentConfig.Webhook.Enabled = false
	harness.WriteConfig(t, independentHome.ConfigPath, independentConfig, nil)
	independent := harness.StartLooperd(t, bins, independentHome, independentHome.ConfigPath, nil, independentConfig.Server.Host, independentConfig.Server.Port)
	independentCtx, independentCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer independentCancel()
	if _, err := independent.WaitForReady(independentCtx); err != nil {
		t.Fatalf("independent daemon readiness: %v", err)
	}
}
