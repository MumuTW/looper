package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/config"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	"github.com/MumuTW/looper/internal/loops/brownout"
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
	if err := rt.allowAgentSpawn(&agent.SpawnMeta{Vendor: string(codex)}); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("Codex spawn admission = %v, want brownout open", err)
	}
	if err := rt.allowAgentSpawn(&agent.SpawnMeta{Vendor: string(claude)}); err != nil {
		t.Fatalf("Claude spawn admission = %v, want allowed", err)
	}
	for i := 0; i < 3; i++ {
		rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: string(claude), Status: "failed"})
	}
	// An unattributed legacy callback must not recreate a healthy default bucket
	// once all configured providers are open.
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Status: "completed", Succeeded: true})
	if err := rt.AllowClaim(); !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("unattributed outcome bypassed configured provider breakers: %v", err)
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
	rt.activeExecutions.ReportAgentOutcome(agent.Outcome{Vendor: vendor, BrownoutProbe: true, StartedAt: clock.now(), Status: "completed", Succeeded: true})
	if got := rt.agentHealth.breaker(vendor).Snapshot().State; got != brownout.StateClosed {
		t.Fatalf("probe outcome left state %s, want closed", got)
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

// Webhook acceptance spends agent budget downstream, so it must see the gate.
func TestWebhookAcceptanceIsHeldBackByTheBrownoutGate(t *testing.T) {
	rt, clock := brownoutRuntime(t, nil)
	failAgent(rt, clock, 3)

	ran := false
	err := rt.WithAllowClaim(func() { ran = true })
	if !errors.Is(err, brownout.ErrOpen) {
		t.Fatalf("WithAllowClaim() = %v, want brownout.ErrOpen", err)
	}
	if ran {
		t.Fatal("WithAllowClaim() ran its critical section while the gate was open")
	}
}
