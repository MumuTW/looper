package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGatewayPullRequestLookupRequiresDaemonCredentialBeforeGHStart(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ghPath := "gh-must-not-start-without-daemon-credential"
	cfg.Tools.GHPath = &ghPath

	_, err = NewGatewayPullRequestLookup(cfg, time.Now)(context.Background(), "acme/repo", 1, t.TempDir())
	if !errors.Is(err, github.ErrCredentialUnavailable) {
		t.Fatalf("lookup error = %v, want ErrCredentialUnavailable before gh starts", err)
	}
}

func TestManualHoldPreflightRequiresDaemonCredentialBeforeGHStart(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	ghPath := "gh-must-not-start-without-daemon-credential"
	cfg.Tools.GHPath = &ghPath
	projectID := "project_manual_hold"
	now := "2026-07-30T00:00:00.000Z"
	if err := rt.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Manual hold", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: cfg, Runtime: rt})
	err := h.validateManualHoldBypassForLoopTarget(context.Background(), projectID, domain.LoopTypeWorker, domain.LoopTarget{
		TargetType:  domain.LoopTargetTypeIssue,
		Repo:        "acme/repo",
		IssueNumber: 1,
	}, false)
	if err == nil || !strings.Contains(err.Error(), github.ErrCredentialUnavailable.Error()) {
		t.Fatalf("manual hold preflight error = %v, want credential gate before gh starts", err)
	}
}
