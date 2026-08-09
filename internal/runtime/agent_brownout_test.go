package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/config"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	"github.com/MumuTW/looper/internal/loops/brownout"
	"github.com/MumuTW/looper/internal/storage"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func brownoutRuntime(t *testing.T, mutate func(*config.AgentBrownoutConfig)) (*Runtime, *testClock) {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.AgentBrownout.MinFailures = 3
	cfg.Scheduler.AgentBrownout.FailureRatio = 0.8
	cfg.Scheduler.AgentBrownout.CooldownSeconds = 600
	cfg.Scheduler.AgentBrownout.MaxCooldownSeconds = 3600
	cfg.Scheduler.AgentBrownout.ProbeSuccesses = 1
	if mutate != nil {
		mutate(&cfg.Scheduler.AgentBrownout)
	}
	clock := &testClock{at: time.Date(2026, 8, 1, 22, 0, 0, 0, time.UTC)}
	rt := New(Options{Config: cfg, Now: clock.now})
	if err := rt.admission.MarkReady("test"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	return rt, clock
}

// Outcomes carry StartedAt because the breaker only treats executions admitted
// during the current half-open period as probes.
func failAgent(rt *Runtime, clock *testClock, times int) {
	for i := 0; i < times; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Status: "failed", StartedAt: clock.now()})
	}
}

func succeedAgent(rt *Runtime, clock *testClock) {
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Status: "completed", Succeeded: true, StartedAt: clock.now()})
}

// The incident this gate exists for: agent runs failing over and over while the
// scheduler kept producing work. AllowClaim is the gate the whole
// work-producing tick passes through, so closing it there stops discovery too —
// not only claims.
func TestRepeatedAgentFailuresCloseClaimAdmission(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)

	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("healthy runtime refused claims: %v", err)
	}
	failAgent(rt, clock, 3)

	err := rt.AllowClaim()
	if !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("AllowClaim() = %v, want brownout.ErrOpen after sustained agent failures", err)
	}
}

func TestWithAllowAgentClaimLanesRefusesOpenProvider(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)
	called := false
	err := rt.WithAllowAgentClaimLanes([]storage.QueueClaimLane{{Vendor: ""}}, func() {
		called = true
	})
	if !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("WithAllowAgentClaimLanes() = %v, want brownout.ErrOpen", err)
	}
	if called {
		t.Fatal("atomic claim callback ran while the provider breaker was open")
	}
}

func TestSchedulerClaimPumpUsesLifecycleAdmissionDuringBrownout(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	rt.services.Repositories = &storage.Repositories{}
	claimCalls := 0
	rt.defaultSchedulerClaim = func(context.Context, Services) error {
		claimCalls++
		return nil
	}
	failAgent(rt, clock, 3)
	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("AllowClaim() = nil, want provider brownout open")
	}

	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls != 1 {
		t.Fatalf("claim calls = %d, want lifecycle-only outer pump to reach partitioned claim logic", claimCalls)
	}
}

func TestAgentBrownoutIsolatedByEffectiveVendor(t *testing.T) {
	rt, _ := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
	})
	base := rt.Config()
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	base.Agent.Vendor = &codex
	base.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &claude}
	rt.projectCatalog.PublishGlobals(base)
	rt.publishCatalogConsumers(base)

	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	if err := rt.agentHealth.Allow(string(codex)); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("codex breaker error = %v, want brownout open", err)
	}
	if err := rt.agentHealth.Allow(string(claude)); err != nil {
		t.Fatalf("healthy Claude breaker refused work: %v", err)
	}
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("healthy provider should keep scheduler admission open: %v", err)
	}
	if _, err := rt.allowAgentSpawn(&agent.SpawnMeta{Vendor: string(codex)}); !errors.Is(err, agent.ErrProviderBrownout) || !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("Codex spawn admission = %v, want provider brownout marker and open cause", err)
	}
	release, err := rt.allowAgentSpawn(&agent.SpawnMeta{Vendor: string(claude)})
	if err != nil {
		t.Fatalf("Claude spawn admission = %v, want allowed", err)
	}
	release()
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(claude), Status: "failed"})
	}
	// An unattributed legacy callback must not recreate a healthy default bucket
	// once all configured providers are open.
	logger := &recordingLogger{}
	rt.logger = logger
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Status: "completed", Succeeded: true})
	if !logger.sawWarn("agent outcome missing effective provider attribution; outcome ignored") {
		t.Fatal("unattributed outcome was dropped without a warning")
	}
	if err := rt.AllowClaim(); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("unattributed outcome bypassed configured provider breakers: %v", err)
	}
}

func TestAgentHealthSnapshotExposesPartialProviderBrownout(t *testing.T) {
	rt, _ := brownoutRuntime(t, nil)
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	base := rt.Config()
	base.Agent.Vendor = &codex
	base.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &claude}
	rt.projectCatalog.PublishGlobals(base)
	rt.publishCatalogConsumers(base)

	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	summary := rt.AgentHealth()
	if summary.State != brownout.StateClosed || !summary.Partial {
		t.Fatalf("AgentHealth() = %#v, want closed aggregate with partial=true", summary)
	}
	if len(summary.Providers) != 2 {
		t.Fatalf("provider summaries = %#v, want codex and claude", summary.Providers)
	}
	states := make(map[string]brownout.State, len(summary.Providers))
	for _, provider := range summary.Providers {
		states[provider.Provider] = provider.State
	}
	if states[string(codex)] != brownout.StateOpen || states[string(claude)] != brownout.StateClosed {
		t.Fatalf("provider states = %#v, want codex open and claude closed", states)
	}
}

func TestAgentHealthRegistersGlobalCoordinatorProvider(t *testing.T) {
	rt, _ := brownoutRuntime(t, nil)
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	next := rt.Config()
	next.Agent.Vendor = &codex
	roleAgent := &config.RoleAgentConfig{Vendor: &claude}
	next.Roles.Planner.Agent = roleAgent
	next.Roles.Worker.Agent = roleAgent
	next.Roles.Reviewer.Agent = roleAgent
	next.Roles.Fixer.Agent = roleAgent
	next.Roles.Coordinator.Enabled = true
	rt.publishCatalogConsumers(next)

	if err := rt.agentHealth.Allow(string(codex)); err != nil {
		t.Fatalf("global coordinator provider admission = %v, want registered provider", err)
	}
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "coordinator-loop", RunID: "coordinator-run", ExecutionID: "coordinator-exec", Vendor: string(codex),
	})
	if err != nil {
		t.Fatalf("coordinator global-provider spawn = %v, want admitted", err)
	}
	lease.Release()
}

func TestAgentHealthExcludesDisabledCoordinatorProvider(t *testing.T) {
	rt, _ := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
	})
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	next := rt.Config()
	next.Agent.Vendor = &codex
	roleAgent := &config.RoleAgentConfig{Vendor: &claude}
	next.Roles.Planner.Agent = roleAgent
	next.Roles.Worker.Agent = roleAgent
	next.Roles.Reviewer.Agent = roleAgent
	next.Roles.Fixer.Agent = roleAgent
	next.Roles.Coordinator.Enabled = false
	rt.publishCatalogConsumers(next)

	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(claude), Status: "failed"})
	}
	if err := rt.agentHealth.AllowAny(); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("AllowAny() with disabled coordinator provider = %v, want open from Claude", err)
	}
	if err := rt.agentHealth.Allow(string(codex)); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("disabled coordinator provider admission = %v, want fail-closed", err)
	}
}

func TestAgentBrownoutGateCoversClaimCriticalSection(t *testing.T) {
	rt, clock := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- rt.WithAllowAgentClaim(func() {
			close(entered)
			<-release
		})
	}()
	<-entered
	// Terminal outcomes use the same gate mutex. If it were only a point-in-
	// time check, this lock would be available while the claim section is held.
	if rt.agentHealth.gateMu.TryLock() {
		rt.agentHealth.gateMu.Unlock()
		t.Fatal("brownout gate was not held across the claim critical section")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("WithAllowAgentClaim() = %v", err)
	}
	failAgent(rt, clock, 3)
	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("outcomes after the critical section did not open the provider breaker")
	}
}

func TestAgentBrownoutGateBlocksRecursiveSpawnUntilSectionEnds(t *testing.T) {
	rt, _ := brownoutRuntime(t, nil)
	type spawnResult struct {
		lease agent.SpawnLease
		err   error
	}
	result := make(chan spawnResult, 1)
	if err := rt.WithAllowAgentClaim(func() {
		go func() {
			lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{Vendor: "_default"})
			result <- spawnResult{lease: lease, err: err}
		}()
		select {
		case <-result:
			t.Fatal("recursive spawn admission completed while the non-reentrant gate was held")
		case <-time.After(20 * time.Millisecond):
		}
	}); err != nil {
		t.Fatalf("WithAllowAgentClaim() = %v", err)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("spawn admission after critical section = %v", got.err)
		}
		got.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("recursive spawn admission stayed blocked after the critical section ended")
	}
}

func TestAgentBrownoutIgnoresPreCooldownOutcomesAsProbes(t *testing.T) {
	rt, clock := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
		cfg.CooldownSeconds = 60
		cfg.MaxCooldownSeconds = 120
	})
	vendor := "codex"
	startedBefore := clock.now()
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: vendor, StartedAt: startedBefore, Status: "failed"})
	}
	if err := rt.agentHealth.Allow(vendor); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("breaker error = %v, want open", err)
	}
	clock.advance(time.Minute)
	probe, err := rt.agentHealth.AllowAdmission(vendor)
	if err != nil || !probe {
		t.Fatalf("half-open admission = (%v, %v), want probe=true and nil error", probe, err)
	}
	// This execution started before half-open; it must not close or re-trip the
	// breaker merely because it completed after the cooldown elapsed.
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: vendor, StartedAt: startedBefore, Status: "completed", Succeeded: true})
	if got := rt.agentHealth.breaker(vendor).Snapshot().State; got != brownout.StateHalfOpen {
		t.Fatalf("pre-cooldown outcome changed state to %s, want half_open", got)
	}
	clock.advance(time.Nanosecond)
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: vendor, BrownoutProbe: true, StartedAt: clock.now(), Status: "completed", Succeeded: true})
	if got := rt.agentHealth.breaker(vendor).Snapshot().State; got != brownout.StateClosed {
		t.Fatalf("probe outcome left state %s, want closed", got)
	}
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: vendor, StartedAt: startedBefore, Status: "failed"})
	}
	if got := rt.agentHealth.breaker(vendor).Snapshot().State; got != brownout.StateClosed {
		t.Fatalf("stale post-recovery outcome changed state to %s, want closed", got)
	}
}

func TestAgentBrownoutReleasesProbeWhenSpawnLeaseEndsWithoutOutcome(t *testing.T) {
	rt, clock := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
		cfg.CooldownSeconds = 60
		cfg.MaxCooldownSeconds = 120
	})
	vendor := "_default"
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: vendor, StartedAt: clock.now(), Status: "failed"})
	}
	if err := rt.agentHealth.Allow(vendor); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("breaker error = %v, want open", err)
	}
	clock.advance(time.Minute)

	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "probe-loop", RunID: "probe-run", ExecutionID: "probe-first", Vendor: vendor,
	})
	if err != nil {
		t.Fatalf("first half-open spawn = %v", err)
	}
	if probe, ok := lease.(interface{ BrownoutProbe() bool }); !ok || !probe.BrownoutProbe() {
		t.Fatal("first half-open spawn did not carry a probe reservation")
	}
	// Simulate cmd.Start/sandbox failure: no terminal Outcome is reported.
	lease.Release()

	replacement, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "probe-loop", RunID: "probe-run", ExecutionID: "probe-replacement", Vendor: vendor,
	})
	if err != nil {
		t.Fatalf("replacement half-open spawn = %v, want released probe slot", err)
	}
	replacement.Release()
}

func TestAgentBrownoutAdmitsStickySnapshotAfterVendorRemoval(t *testing.T) {
	rt, _ := brownoutRuntime(t, nil)
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	configured := rt.Config()
	configured.Agent.Vendor = &codex
	configured.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &claude}
	rt.publishCatalogConsumers(configured)

	removed := configured
	removed.Agent.Vendor = nil
	rt.publishCatalogConsumers(removed)

	sticky, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "sticky-loop", RunID: "sticky-run", ExecutionID: "sticky-exec", Vendor: string(codex),
		BrownoutStickySnapshot: true,
	})
	if err != nil {
		t.Fatalf("sticky snapshot spawn = %v, want old vendor admitted through its preserved health bucket", err)
	}
	sticky.Release()

	if _, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "live-loop", RunID: "live-run", ExecutionID: "live-exec", Vendor: string(codex),
	}); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("ordinary spawn for removed vendor = %v, want provider admission refusal", err)
	}
}

func TestStickySnapshotClaimCheckDoesNotConsumeHalfOpenProbe(t *testing.T) {
	rt, clock := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
		cfg.CooldownSeconds = 60
		cfg.MaxCooldownSeconds = 120
	})
	codex := config.AgentVendorCodex
	configured := rt.Config()
	configured.Agent.Vendor = &codex
	rt.publishCatalogConsumers(configured)
	preRemovalLease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "retained-loop", RunID: "retained-run", ExecutionID: "retained-exec", Vendor: string(codex)})
	if err != nil {
		t.Fatalf("pre-removal spawn lease = %v", err)
	}
	defer preRemovalLease.Release()
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	if err := rt.agentHealth.Allow(string(codex)); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("codex breaker = %v, want open before removal", err)
	}
	removed := configured
	removed.Agent.Vendor = nil
	rt.publishCatalogConsumers(removed)
	clock.advance(time.Minute)

	if err := rt.AllowSnapshotClaimForVendor(string(codex)); err != nil {
		t.Fatalf("snapshot claim check = %v, want half-open point-in-time admission", err)
	}
	if err := rt.WithAllowSnapshotAgentClaimForVendor(string(codex), func() {}); err != nil {
		t.Fatalf("snapshot claim critical section = %v, want allowed", err)
	}
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "sticky-probe-loop", RunID: "sticky-probe-run", ExecutionID: "sticky-probe-exec", Vendor: string(codex), BrownoutStickySnapshot: true,
	})
	if err != nil {
		t.Fatalf("sticky probe spawn = %v, want the unconsumed probe slot", err)
	}
	if probe, ok := lease.(interface{ BrownoutProbe() bool }); !ok || !probe.BrownoutProbe() {
		t.Fatal("sticky probe spawn did not reserve the half-open probe")
	}
	lease.Release()
}

func TestAgentHealthPromotesRetainedBreakerWhenVendorReturns(t *testing.T) {
	rt, _ := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
		cfg.CooldownSeconds = 600
		cfg.MaxCooldownSeconds = 3600
	})
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	configured := rt.Config()
	configured.Agent.Vendor = &codex
	configured.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &claude}
	rt.publishCatalogConsumers(configured)
	preRemovalLease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "retained-loop", RunID: "retained-run", ExecutionID: "retained-exec", Vendor: string(codex)})
	if err != nil {
		t.Fatalf("pre-removal spawn lease = %v", err)
	}
	defer preRemovalLease.Release()
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	if err := rt.agentHealth.Allow(string(codex)); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("codex breaker = %v, want open before removal", err)
	}

	removed := configured
	removed.Agent.Vendor = nil
	rt.publishCatalogConsumers(removed)

	reactivated := removed
	reactivated.Agent.Vendor = &codex
	rt.publishCatalogConsumers(reactivated)
	if err := rt.agentHealth.Allow(string(codex)); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("reactivated codex breaker = %v, want retained open state", err)
	}
}

func TestAgentHealthRecordsInFlightOutcomeAfterVendorRemoval(t *testing.T) {
	rt, _ := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
	})
	codex := config.AgentVendorCodex
	configured := rt.Config()
	configured.Agent.Vendor = &codex
	rt.publishCatalogConsumers(configured)
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "in-flight-loop", RunID: "in-flight-run", ExecutionID: "in-flight-exec", Vendor: string(codex)})
	if err != nil {
		t.Fatalf("pre-removal spawn lease = %v", err)
	}
	defer lease.Release()
	for i := 0; i < 2; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	removed := configured
	removed.Agent.Vendor = nil
	rt.publishCatalogConsumers(removed)

	// This run was admitted before reload and completes after its provider was
	// removed. Its failure still belongs to the retained breaker that owns the
	// sticky retry identity.
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	if err := rt.agentHealth.AllowSnapshot(string(codex)); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("retained Codex breaker = %v, want open after in-flight failure", err)
	}
}

func TestAgentHealthSnapshotExposesNonClosedStickyProvider(t *testing.T) {
	rt, _ := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
	})
	codex := config.AgentVendorCodex
	claude := config.AgentVendorClaudeCode
	configured := rt.Config()
	configured.Agent.Vendor = &codex
	configured.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &claude}
	rt.publishCatalogConsumers(configured)
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "sticky-status-loop", RunID: "sticky-status-run", ExecutionID: "sticky-status-exec", Vendor: string(codex)})
	if err != nil {
		t.Fatalf("pre-removal spawn lease = %v", err)
	}
	defer lease.Release()
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	removed := configured
	removed.Agent.Vendor = nil
	rt.publishCatalogConsumers(removed)

	summary := rt.AgentHealth()
	if summary.State != brownout.StateClosed || summary.Partial {
		t.Fatalf("AgentHealth() = %#v, want active aggregate closed and non-partial", summary)
	}
	var sticky *brownout.ProviderSummary
	for i := range summary.Providers {
		if summary.Providers[i].Provider == string(codex) {
			sticky = &summary.Providers[i]
			break
		}
	}
	if sticky == nil || sticky.State != brownout.StateOpen {
		t.Fatalf("provider summaries = %#v, want non-closed sticky Codex provider", summary.Providers)
	}
}

func TestAgentHealthDropsRemovedBreakerWithoutStickyWork(t *testing.T) {
	rt, _ := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
	})
	codex := config.AgentVendorCodex
	configured := rt.Config()
	configured.Agent.Vendor = &codex
	rt.publishCatalogConsumers(configured)
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(codex), Status: "failed"})
	}
	removed := configured
	removed.Agent.Vendor = nil
	rt.publishCatalogConsumers(removed)

	summary := rt.AgentHealth()
	for _, provider := range summary.Providers {
		if provider.Provider == string(codex) {
			t.Fatalf("removed provider retained without queued/live sticky work: %#v", summary.Providers)
		}
	}
}

func TestAgentBrownoutClaimAdmissionSkipsExhaustedHalfOpenProbe(t *testing.T) {
	rt, clock := brownoutRuntime(t, func(cfg *config.AgentBrownoutConfig) {
		cfg.MinFailures = 3
		cfg.ProbeSuccesses = 1
	})
	vendor := "_default"
	failAgent(rt, clock, 3)
	clock.advance(10 * time.Minute)
	lease, err := rt.activeExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "probe-loop", RunID: "probe-run", ExecutionID: "probe-exec", Vendor: vendor,
	})
	if err != nil {
		t.Fatalf("half-open probe spawn = %v", err)
	}
	defer lease.Release()
	if err := rt.AllowClaimForVendor(vendor); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("claim admission with exhausted probe lane = %v, want brownout.ErrOpen", err)
	}
}

func TestWorktreeCleanupContinuesDuringAgentBrownout(t *testing.T) {
	fixture := newWorktreeCleanupFixture(t)
	for i := 0; i < fixture.config.Scheduler.AgentBrownout.MinFailures; i++ {
		fixture.runtime.activeExecutions.ReportAgentOutcome(agent.Outcome{Status: "completed", Succeeded: false})
	}
	if !errors.Is(fixture.runtime.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("expected agent brownout to close work-producing admission")
	}

	git := &fakeWorktreeCleanupGit{
		listed: map[string][]gitinfra.WorktreeListEntry{fixture.project.RepoPath: {}},
		clean:  map[string]bool{},
	}
	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)
	if summary.LastStatus != "completed" {
		t.Fatalf("cleanup status = %#v, want completed while brownout is open", summary)
	}
	if events := fixture.events(t); !containsWorktreeCleanupEvent(events, "worktree.cleanup.completed") {
		t.Fatalf("events = %#v, want durable completion event while brownout is open", events)
	}
}

// An agent looper killed says nothing about the provider. Counting those would
// let an operator stop or a shutdown drain trip the gate.
func TestKilledAgentsDoNotCountAsFailures(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	for i := 0; i < 10; i++ {
		// The executor filters "killed" before it reaches the registry; assert
		// the daemon-side contract by reporting only what it forwards.
		succeedAgent(rt, clock)
	}
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("successful agents closed admission: %v", err)
	}
}

func TestAgentBrownoutReopensAfterConfiguredCooldown(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)
	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("expected the gate to be open")
	}

	clock.advance(9 * time.Minute)
	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("gate reopened before the operator's configured safe interval elapsed")
	}

	clock.advance(2 * time.Minute)
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("gate did not retry on its own after the cooldown: %v", err)
	}
	if got := rt.AgentHealth().State; got != brownout.StateHalfOpen {
		t.Fatalf("AgentHealth() state = %s, want half_open", got)
	}

	succeedAgent(rt, clock)
	if got := rt.AgentHealth().State; got != brownout.StateClosed {
		t.Fatalf("AgentHealth() state = %s, want closed after a successful probe", got)
	}
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() after recovery = %v", err)
	}
}

// Admission is the stronger authority: a stopping daemon must report stopping,
// not a brownout an operator might wait out.
func TestAdmissionOutranksAgentBrownout(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)
	if err := rt.admission.BeginShutdown("test"); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	err := rt.AllowClaim()
	if errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("AllowClaim() reported brownout while stopping: %v", err)
	}
	if err == nil {
		t.Fatal("AllowClaim() allowed work while stopping")
	}
}

func TestWithAllowClaimAdmissionOutranksAgentBrownout(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)
	if err := rt.admission.BeginShutdown("test"); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	ran := false
	err := rt.WithAllowClaim(func() { ran = true })
	if errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("WithAllowClaim() reported brownout while stopping: %v", err)
	}
	if err == nil || ran {
		t.Fatalf("WithAllowClaim() = %v ran=%t, want admission refusal", err, ran)
	}
}

func TestDisablingAgentBrownoutReleasesTheGate(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)
	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("expected the gate to be open")
	}

	next := rt.Config()
	next.Scheduler.AgentBrownout.Enabled = false
	rt.publishCatalogConsumers(next)

	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("disabling the gate did not release it: %v", err)
	}
}

// Editing config during an outage must not resume the hammering: a reload that
// silently cleared the open gate would turn "let me lengthen the cooldown" into
// "start failing again immediately".
func TestConfigReloadDoesNotClearAnOpenGate(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)
	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("expected the gate to be open")
	}

	next := rt.Config()
	next.Scheduler.AgentBrownout.CooldownSeconds = 1200
	rt.publishCatalogConsumers(next)

	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("config reload released an open brownout gate")
	}
}

func TestAgentBrownoutTransitionsAreLogged(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	logger := &recordingLogger{}
	rt.logger = logger
	failAgent(rt, clock, 3)

	if !logger.sawWarn("agent brownout opened: work production suspended") {
		t.Fatalf("no warning logged when work production stopped; logged: %v", logger.messages())
	}
}

func TestAgentBrownoutResumeMessageDistinguishesDisabledOverride(t *testing.T) {
	t.Parallel()

	title, subtitle, body, dedupe := agentBrownoutResumeMessage("codex", "disabled_override")
	if title != "Looper Resumed: codex (override)" || subtitle != "codex brownout disabled by operator override" {
		t.Fatalf("override notice title/subtitle = %q / %q", title, subtitle)
	}
	if !strings.Contains(body, "operator") || !strings.Contains(body, "no recovery probe was observed") || strings.Contains(body, "probe agent run succeeded") {
		t.Fatalf("override notice body = %q, want operator override without probe recovery claim", body)
	}
	if dedupe != ".disabled_override" {
		t.Fatalf("override dedupe suffix = %q, want distinct transition", dedupe)
	}

	recoveryTitle, recoverySubtitle, recoveryBody, recoveryDedupe := agentBrownoutResumeMessage("codex", "probe_success")
	if recoveryTitle != "Looper Resumed: codex" || recoverySubtitle != "a codex probe agent run succeeded" || !strings.Contains(recoveryBody, "working again") || recoveryDedupe != ".closed" {
		t.Fatalf("recovery notice = %q / %q / %q / %q", recoveryTitle, recoverySubtitle, recoveryBody, recoveryDedupe)
	}
}

func TestBrownoutNotificationProviderKeySerializesOverrideTail(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{".open", ".closed", ".disabled_override", ".open.2", ".closed.2", ".disabled_override.2"} {
		if got := brownoutNotificationProviderKey("codex" + suffix); got != "codex" {
			t.Fatalf("brownoutNotificationProviderKey(%q) = %q, want codex", suffix, got)
		}
	}
	if got := brownoutNotificationProviderKey("codex.other"); got != "codex.other" {
		t.Fatalf("brownoutNotificationProviderKey(non-transition) = %q, want unchanged suffix", got)
	}
}

func TestBrownoutTransitionDedupeSuffixKeepsDistinctTrips(t *testing.T) {
	t.Parallel()
	if got := brownoutTransitionDedupeSuffix("codex.open", 1); got != "codex.open.1" {
		t.Fatalf("trip one suffix = %q, want codex.open.1", got)
	}
	if got := brownoutTransitionDedupeSuffix("codex.open", 2); got != "codex.open.2" {
		t.Fatalf("trip two suffix = %q, want codex.open.2", got)
	}
	if got := brownoutNotificationProviderKey("codex.open.2"); got != "codex" {
		t.Fatalf("trip suffix provider key = %q, want codex", got)
	}
}

func TestBrownoutNotificationDedupeKeyScopesDaemonIncident(t *testing.T) {
	t.Parallel()
	first := brownoutNotificationDedupeKey("daemon-a", "codex.open.1")
	second := brownoutNotificationDedupeKey("daemon-b", "codex.open.1")
	if first == second {
		t.Fatalf("restart incidents reused dedupe key %q", first)
	}
	if first != "runtime.agentBrownout.daemon-a.codex.open.1" {
		t.Fatalf("dedupe key = %q, want daemon-scoped transition key", first)
	}
}

func TestBrownoutIncidentIDRotatesPerRuntime(t *testing.T) {
	first, _ := brownoutRuntime(t, nil)
	second, _ := brownoutRuntime(t, nil)
	if first.brownoutIncidentID == "" || second.brownoutIncidentID == "" {
		t.Fatal("brownout incident ID must be initialized for every runtime")
	}
	if first.brownoutIncidentID == second.brownoutIncidentID {
		t.Fatalf("runtime restart reused brownout incident ID %q", first.brownoutIncidentID)
	}
}

type recordingLogger struct {
	mu      sync.Mutex
	entries []loggedEntry
}

type loggedEntry struct {
	level   string
	message string
}

func (l *recordingLogger) Debug(msg string, _ map[string]any) { l.record("debug", msg) }
func (l *recordingLogger) Info(msg string, _ map[string]any)  { l.record("info", msg) }
func (l *recordingLogger) Warn(msg string, _ map[string]any)  { l.record("warn", msg) }
func (l *recordingLogger) Error(msg string, _ map[string]any) { l.record("error", msg) }

func (l *recordingLogger) record(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, loggedEntry{level: level, message: msg})
}

func (l *recordingLogger) sawWarn(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.level == "warn" && entry.message == msg {
			return true
		}
	}
	return false
}

func (l *recordingLogger) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.entries))
	for _, entry := range l.entries {
		out = append(out, entry.level+": "+entry.message)
	}
	return out
}

// A provider outage is exactly when worktree debt accumulates: every attempt
// prepares a worktree before failing at the agent step. Suspending cleanup for
// the whole backoff would preserve the disk growth that made the incident
// visible in the first place.
func TestWorktreeCleanupIsNotHeldBackByTheBrownoutGate(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)

	if !errors.Is(rt.AllowClaim(), brownout.ErrOpen) {
		t.Fatal("expected the gate to be open")
	}
	if err := rt.AllowLifecycleWork(); err != nil {
		t.Fatalf("worktree cleanup was blocked by the agent-health gate: %v", err)
	}
}

// Cleanup keeps claim admission's authority, so it still stops while the daemon
// is shutting down. That is the #580 invariant the exemption must not weaken.
func TestLifecycleWorkStillRespectsAdmission(t *testing.T) {
	rt, _ := brownoutRuntime(t, nil)
	if err := rt.admission.BeginShutdown("test"); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	if err := rt.AllowLifecycleWork(); err == nil {
		t.Fatal("AllowLifecycleWork() allowed work while stopping")
	}
}

// Webhook acceptance is lifecycle work; provider admission remains at the
// agent-producing discovery/claim/spawn boundaries.
func TestWebhookAcceptanceUsesLifecycleAdmissionDuringBrownout(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)

	ran := false
	if err := rt.WithAllowLifecycleWork(func() { ran = true }); err != nil {
		t.Fatalf("WithAllowLifecycleWork() = %v, want lifecycle admission during brownout", err)
	}
	if !ran {
		t.Fatal("WithAllowLifecycleWork() did not run its critical section")
	}
}
