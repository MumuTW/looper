package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/escalator"
)

type fakeScheduledEscalator struct {
	calls  int
	err    error
	result escalator.RunResult
}

func (f *fakeScheduledEscalator) Run(context.Context) (escalator.RunResult, error) {
	f.calls++
	return f.result, f.err
}

func TestRunEscalatorIfDueHonorsCadenceAcrossSnapshots(t *testing.T) {
	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	state := &schedulerEscalatorCadence{}
	runner := &fakeScheduledEscalator{}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Escalator.Enabled = true
	cfg.Roles.Escalator.CadenceSeconds = 3600
	input := defaultSchedulerTickInput{Config: &cfg, Escalator: runner, EscalatorCadence: state}

	if err := runEscalatorIfDue(context.Background(), input, now); err != nil {
		t.Fatalf("first run error = %v", err)
	}
	// A newly built catalog snapshot carries a new runner but shares the cadence
	// state, so config reload cannot multiply global digests.
	second := &fakeScheduledEscalator{}
	input.Escalator = second
	if err := runEscalatorIfDue(context.Background(), input, now.Add(30*time.Minute)); err != nil {
		t.Fatalf("early run error = %v", err)
	}
	if runner.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = first:%d second:%d, want 1/0", runner.calls, second.calls)
	}
	if err := runEscalatorIfDue(context.Background(), input, now.Add(time.Hour)); err != nil {
		t.Fatalf("due run error = %v", err)
	}
	if second.calls != 1 {
		t.Fatalf("second calls = %d, want 1 when due", second.calls)
	}
}

func TestRunEscalatorIfDueRetriesFailedAttemptNextTick(t *testing.T) {
	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	wantErr := errors.New("census unavailable")
	runner := &fakeScheduledEscalator{err: wantErr}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Escalator.Enabled = true
	cfg.Roles.Escalator.CadenceSeconds = 3600
	input := defaultSchedulerTickInput{Config: &cfg, Escalator: runner, EscalatorCadence: &schedulerEscalatorCadence{}}

	if err := runEscalatorIfDue(context.Background(), input, now); !errors.Is(err, wantErr) {
		t.Fatalf("first error = %v, want %v", err, wantErr)
	}
	runner.err = nil
	if err := runEscalatorIfDue(context.Background(), input, now.Add(time.Minute)); err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if runner.calls != 2 {
		t.Fatalf("calls = %d, want failed attempt retried next tick", runner.calls)
	}
}

func TestRunEscalatorIfDueRetriesPartialCensusNextTick(t *testing.T) {
	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	runner := &fakeScheduledEscalator{result: escalator.RunResult{
		Snapshot: escalator.Snapshot{Partial: true}, Suppressed: true,
	}}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Escalator.Enabled = true
	cfg.Roles.Escalator.CadenceSeconds = 3600
	input := defaultSchedulerTickInput{Config: &cfg, Escalator: runner, EscalatorCadence: &schedulerEscalatorCadence{}}

	if err := runEscalatorIfDue(context.Background(), input, now); err != nil {
		t.Fatalf("partial run error = %v", err)
	}
	runner.result = escalator.RunResult{}
	if err := runEscalatorIfDue(context.Background(), input, now.Add(time.Minute)); err != nil {
		t.Fatalf("retry run error = %v", err)
	}
	if runner.calls != 2 {
		t.Fatalf("calls = %d, want partial census retried on next tick", runner.calls)
	}
}

func TestRuntimeEscalatorLinks(t *testing.T) {
	base := "https://looper.example.test/root/"
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.BaseURL = &base
	links := newRuntimeEscalatorLinker(cfg)
	if got := links.Issue("project", "owner/repo", 42); got != "https://github.com/owner/repo/issues/42" {
		t.Fatalf("Issue() = %q", got)
	}
	if got := links.PullRequest("project", "github.example.test/owner/repo", 7); got != "https://github.example.test/owner/repo/pull/7" {
		t.Fatalf("PullRequest() = %q", got)
	}
	if got := links.Loop("project", 19); got != "https://looper.example.test/root/dashboard/loops/19" {
		t.Fatalf("Loop() = %q", got)
	}
}

func TestRuntimeEscalatorLinksUseConfiguredProvider(t *testing.T) {
	provider := config.ProviderConfig{ID: "enterprise", Kind: config.ProviderKindGitHub, BaseURL: "https://ghe.example.test/api/v3"}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []config.ProviderConfig{provider}
	cfg.Projects = []config.ProjectRefConfig{{ID: "enterprise-project", Provider: provider.ID, Repo: "acme/looper", RepoPath: t.TempDir()}}

	links := newRuntimeEscalatorLinker(cfg)
	if got := links.PullRequest("enterprise-project", "acme/looper", 42); got != "https://ghe.example.test/acme/looper/pull/42" {
		t.Fatalf("PullRequest() = %q, want configured provider browser URL", got)
	}
	if got := links.Issue("enterprise-project", "acme/looper", 7); got != "https://ghe.example.test/acme/looper/issues/7" {
		t.Fatalf("Issue() = %q, want configured provider browser URL", got)
	}
}

func TestRuntimeEscalatorLinksNormalizeGitHubAPIProvider(t *testing.T) {
	provider := config.ProviderConfig{ID: "github-api", Kind: config.ProviderKindGitHub, BaseURL: "https://api.github.com"}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []config.ProviderConfig{provider}
	cfg.Projects = []config.ProjectRefConfig{{ID: "cloud-project", Provider: provider.ID, Repo: "acme/looper", RepoPath: t.TempDir()}}

	links := newRuntimeEscalatorLinker(cfg)
	if got := links.PullRequest("cloud-project", "acme/looper", 42); got != "https://github.com/acme/looper/pull/42" {
		t.Fatalf("PullRequest() = %q, want public GitHub browser URL", got)
	}
}

func TestBuildDefaultSchedulerWiresEscalatorOnlyWhenEnabled(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disabled := buildValidationCommandHandlers(t, cfg, &capturingSchedulerLogger{}).input(Services{})
	if disabled.Escalator != nil {
		t.Fatal("disabled config wired an Escalator runner")
	}
	cfg.Roles.Escalator.Enabled = true
	enabled := buildValidationCommandHandlers(t, cfg, &capturingSchedulerLogger{}).input(Services{})
	if enabled.Escalator == nil || enabled.EscalatorCadence == nil {
		t.Fatalf("enabled config wiring = runner:%v cadence:%v", enabled.Escalator, enabled.EscalatorCadence)
	}
}
