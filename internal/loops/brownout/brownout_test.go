package brownout

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time      { return c.at }
func (c *clock) add(d time.Duration) { c.at = c.at.Add(d) }

func testConfig() Config {
	return Config{
		Enabled:        true,
		Window:         5 * time.Minute,
		MinFailures:    3,
		FailureRatio:   0.5,
		Cooldown:       10 * time.Minute,
		MaxCooldown:    40 * time.Minute,
		ProbeSuccesses: 1,
	}
}

func newTestBreaker(t *testing.T, cfg Config) (*Breaker, *clock, *[]Transition) {
	t.Helper()
	c := &clock{at: time.Date(2026, 8, 1, 22, 0, 0, 0, time.UTC)}
	transitions := make([]Transition, 0)
	b := New(cfg, c.now, func(tr Transition) { transitions = append(transitions, tr) })
	return b, c, &transitions
}

func TestDisabledBreakerNeverRefuses(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	b, c, _ := newTestBreaker(t, cfg)
	for i := 0; i < 100; i++ {
		b.Record(c.now(), false)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("disabled breaker refused work: %v", err)
	}
}

func TestTripsOnSustainedFailureRate(t *testing.T) {
	b, c, transitions := newTestBreaker(t, testConfig())
	if err := b.Allow(); err != nil {
		t.Fatalf("healthy breaker refused work: %v", err)
	}
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker opened below MinFailures: %v", err)
	}
	b.Record(c.now(), false)
	err := b.Allow()
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("breaker did not open after sustained failures: %v", err)
	}
	if len(*transitions) != 1 || (*transitions)[0].To != StateOpen {
		t.Fatalf("expected one open transition, got %+v", *transitions)
	}
	if (*transitions)[0].Reason != "failure_rate" {
		t.Fatalf("unexpected trip reason: %q", (*transitions)[0].Reason)
	}
}

// A mostly-healthy daemon that fails occasionally must keep working. This is
// what makes the gate safe to enable by default: it is a rate, not a count.
func TestDoesNotTripWhenFailuresAreDilutedBySuccess(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	for i := 0; i < 10; i++ {
		b.Record(c.now(), true)
	}
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker opened on a 3/13 failure rate: %v", err)
	}
}

func TestStaleFailuresLeaveTheWindow(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(6 * time.Minute)
	b.Record(c.now(), false)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker counted failures older than the window: %v", err)
	}
}

func TestCooldownElapsesIntoHalfOpenThenCloses(t *testing.T) {
	b, c, transitions := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("expected breaker to be open")
	}

	c.add(9 * time.Minute)
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("breaker reopened admission before the cooldown elapsed")
	}

	c.add(2 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker stayed shut past its cooldown: %v", err)
	}
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("expected half_open after cooldown, got %s", got)
	}

	b.Record(c.now(), true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("expected closed after a successful probe, got %s", got)
	}
	last := (*transitions)[len(*transitions)-1]
	if last.To != StateClosed || last.Reason != "probes_succeeded" {
		t.Fatalf("unexpected recovery transition: %+v", last)
	}
}

func TestFailedProbeReopensWithDoubledCooldown(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)

	c.add(20 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("expected half_open: %v", err)
	}
	b.Record(c.now(), false)

	snapshot := b.Snapshot()
	if snapshot.State != StateOpen {
		t.Fatalf("expected open after a failed probe, got %s", snapshot.State)
	}
	if snapshot.Cooldown != 20*time.Minute {
		t.Fatalf("expected cooldown to double to 20m, got %s", snapshot.Cooldown)
	}

	// A single failed probe must not be enough to close it again early.
	c.add(19 * time.Minute)
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("breaker admitted work before the doubled cooldown elapsed")
	}
	c.add(2 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker did not retry after the doubled cooldown: %v", err)
	}
}

func TestCooldownIsCappedAtMax(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	for i := 0; i < 6; i++ {
		c.add(b.Snapshot().Cooldown)
		if err := b.Allow(); err != nil {
			t.Fatalf("round %d: expected half_open: %v", i, err)
		}
		b.Record(c.now(), false)
	}
	if got := b.Snapshot().Cooldown; got != 40*time.Minute {
		t.Fatalf("cooldown grew past MaxCooldown: %s", got)
	}
}

func TestRecoveryResetsCooldownToConfigured(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)
	_ = b.Allow()
	b.Record(c.now(), false) // cooldown -> 20m

	c.add(20 * time.Minute)
	_ = b.Allow()
	b.Record(c.now(), true)

	if got := b.Snapshot().Cooldown; got != 10*time.Minute {
		t.Fatalf("recovery left the cooldown backed off at %s", got)
	}
}

// The cooldown must be observable: entering half-open discards the window that
// tripped the breaker, so the first Allow after a cooldown cannot be refused by
// failures the operator already waited out. Status still reports the last trip
// evidence while the new evaluation window is empty.
func TestHalfOpenDoesNotInheritTheTrippingWindow(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	for i := 0; i < 20; i++ {
		b.Record(c.now(), false)
	}
	c.add(10 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("half_open inherited the tripping window: %v", err)
	}
	snapshot := b.Snapshot()
	if snapshot.Failures != 3 || snapshot.Total != 3 {
		t.Fatalf("expected last trip evidence in half_open, got failures=%d total=%d", snapshot.Failures, snapshot.Total)
	}
}

func TestMultipleProbeSuccessesRequired(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 2
	b, c, _ := newTestBreaker(t, cfg)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)
	_ = b.Allow()

	b.Record(c.now(), true)
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("closed after one probe when two were required, got %s", got)
	}
	b.Record(c.now(), true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("expected closed after two probes, got %s", got)
	}
}

func TestHalfOpenAdmissionReservesProbeSlots(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 2
	b, c, _ := newTestBreaker(t, cfg)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)

	probe, err := b.AllowAdmission()
	if err != nil || !probe {
		t.Fatalf("first half-open admission = (%t, %v), want probe", probe, err)
	}
	probe, err = b.AllowAdmission()
	if err != nil || !probe {
		t.Fatalf("second half-open admission = (%t, %v), want probe", probe, err)
	}
	if _, err := b.AllowAdmission(); !errors.Is(err, ErrOpen) {
		t.Fatalf("third half-open admission = %v, want probe-capacity refusal", err)
	}

	b.Record(c.now(), true)
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("state after first reserved probe = %s, want half_open", got)
	}
	b.Record(c.now(), true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("state after all reserved probes = %s, want closed", got)
	}
}

func TestHalfOpenAdmissionReleaseReturnsProbeSlot(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)

	probe, err := b.AllowAdmission()
	if err != nil || !probe {
		t.Fatalf("first half-open admission = (%t, %v), want probe", probe, err)
	}
	b.ReleaseProbe()

	probe, err = b.AllowAdmission()
	if err != nil || !probe {
		t.Fatalf("admission after release = (%t, %v), want probe", probe, err)
	}
	b.RecordAdmission(c.now(), true, true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("state after replacement probe = %s, want closed", got)
	}
}

func TestHalfOpenAdmissionCountsCompletedProbesAgainstCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 2
	b, c, _ := newTestBreaker(t, cfg)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)

	for i := 0; i < 2; i++ {
		probe, err := b.AllowAdmission()
		if err != nil || !probe {
			t.Fatalf("probe admission %d = (%t, %v), want reserved probe", i+1, probe, err)
		}
	}
	b.RecordAdmission(c.now(), true, true)
	if _, err := b.AllowAdmission(); !errors.Is(err, ErrOpen) {
		t.Fatalf("admission after one completed and one in-flight probe = %v, want capacity refusal", err)
	}
	b.RecordAdmission(c.now(), true, true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("state after configured probe successes = %s, want closed", got)
	}
}

func TestStaleProbeGenerationCannotConsumeCurrentCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 2
	b, c, _ := newTestBreaker(t, cfg)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)

	firstProbe, generationOne, err := b.AllowAdmissionWithGeneration()
	if err != nil || !firstProbe || generationOne == 0 {
		t.Fatalf("first generation admission = (%t, %d, %v), want token", firstProbe, generationOne, err)
	}
	secondProbe, generationOneAgain, err := b.AllowAdmissionWithGeneration()
	if err != nil || !secondProbe || generationOneAgain != generationOne {
		t.Fatalf("sibling generation admission = (%t, %d, %v), want generation %d", secondProbe, generationOneAgain, err, generationOne)
	}
	// One failed probe reopens the breaker while the sibling remains active.
	b.RecordAdmissionGeneration(c.now(), false, true, generationOne)
	c.add(20 * time.Minute)

	probe, generationTwo, err := b.AllowAdmissionWithGeneration()
	if err != nil || !probe || generationTwo == generationOne {
		t.Fatalf("next generation admission = (%t, %d, %v), want a new token", probe, generationTwo, err)
	}
	// The old sibling finishes and is abandoned after the new round begins.
	b.RecordAdmissionGeneration(c.now(), true, true, generationOne)
	b.ReleaseProbeGeneration(generationOne)

	if probe, _, err := b.AllowAdmissionWithGeneration(); err != nil || !probe {
		t.Fatalf("second current-generation admission = (%t, %v), want capacity available", probe, err)
	}
	if _, _, err := b.AllowAdmissionWithGeneration(); !errors.Is(err, ErrOpen) {
		t.Fatalf("third current-generation admission = %v, want stale token not to free capacity", err)
	}
}

func TestNonTokenAdmissionDoesNotConsumeProbeSlot(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 1
	b, c, _ := newTestBreaker(t, cfg)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)

	if err := b.Allow(); err != nil {
		t.Fatalf("non-token half-open admission = %v, want nil", err)
	}
	probe, err := b.AllowAdmission()
	if err != nil || !probe {
		t.Fatalf("token admission after health check = (%t, %v), want reserved probe", probe, err)
	}
	if _, err := b.AllowAdmission(); !errors.Is(err, ErrOpen) {
		t.Fatalf("second token admission = %v, want probe-capacity refusal", err)
	}
	b.RecordAdmission(c.now(), true, true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("state after reserved probe = %s, want closed", got)
	}
}

func TestSnapshotReportsOpenUntilAndTrips(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	if snapshot := b.Snapshot(); snapshot.OpenUntil != nil {
		t.Fatalf("closed breaker reported openUntil %v", snapshot.OpenUntil)
	}
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	snapshot := b.Snapshot()
	if snapshot.OpenUntil == nil {
		t.Fatal("open breaker did not report openUntil")
	}
	if snapshot.Trips != 1 {
		t.Fatalf("expected 1 trip, got %d", snapshot.Trips)
	}
}

func TestSnapshotReportsHalfOpenWhenCooldownElapsed(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if got := b.Snapshot().State; got != StateOpen {
		t.Fatalf("state = %s, want open", got)
	}
	c.add(10 * time.Minute)
	snapshot := b.Snapshot()
	if snapshot.State != StateHalfOpen {
		t.Fatalf("state = %s, want half_open after deadline", snapshot.State)
	}
	if snapshot.OpenUntil != nil {
		t.Fatalf("half_open snapshot reported openUntil %v", snapshot.OpenUntil)
	}
	if snapshot.Failures != 3 || snapshot.Total != 3 {
		t.Fatalf("snapshot lost last trip evidence: %+v", snapshot)
	}
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("snapshot should not mutate breaker state, got %s", got)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() after observed deadline = %v", err)
	}
}

func TestSetConfigPreservesOpenStateAndClampsCooldown(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("expected breaker to be open")
	}
	next := testConfig()
	next.Cooldown = 5 * time.Minute
	next.MaxCooldown = 2 * time.Minute
	b.SetConfig(next)
	snapshot := b.Snapshot()
	if snapshot.State != StateOpen {
		t.Fatalf("SetConfig() changed open state to %s", snapshot.State)
	}
	if snapshot.Cooldown != 2*time.Minute {
		t.Fatalf("cooldown = %s, want max-capped 2m", snapshot.Cooldown)
	}
	if snapshot.Failures != 3 || snapshot.Total != 3 {
		t.Fatalf("SetConfig() lost trip evidence: %+v", snapshot)
	}
}

func TestSetConfigDisableResetsEvaluationState(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("expected breaker to be open")
	}
	next := testConfig()
	next.Enabled = false
	b.SetConfig(next)
	snapshot := b.Snapshot()
	if snapshot.State != StateClosed || snapshot.Failures != 0 || snapshot.Total != 0 {
		t.Fatalf("disabled breaker retained live state: %+v", snapshot)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("disabled breaker refused work: %v", err)
	}
}

func TestConcurrentUseAndConfigReloadIsRaceFree(t *testing.T) {
	b := New(testConfig(), time.Now, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if worker%2 == 0 {
					b.Record(time.Now(), j%3 != 0)
				} else {
					_ = b.Allow()
					_ = b.Snapshot()
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			cfg := testConfig()
			cfg.Enabled = i%5 != 0
			cfg.MaxCooldown = time.Duration(1+i%7) * time.Minute
			b.SetConfig(cfg)
		}
	}()
	wg.Wait()
}

func TestNilBreakerIsInert(t *testing.T) {
	var b *Breaker
	if err := b.Allow(); err != nil {
		t.Fatalf("nil breaker refused work: %v", err)
	}
	b.Record(time.Now(), false)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("nil breaker reported %s", got)
	}
}

// A worker admitted before the gate opened can outlive the cooldown. Its result
// says nothing about whether the provider recovered, so it must not decide the
// probe in either direction.
func TestOutcomesOlderThanHalfOpenAreNotProbes(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	admittedBeforeOpen := c.now()
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("expected the gate to be open")
	}

	c.add(10 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("expected half_open: %v", err)
	}

	b.Record(admittedBeforeOpen, true)
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("a pre-open execution closed the gate without testing the provider; state = %s", got)
	}

	b.Record(admittedBeforeOpen, false)
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("a pre-open execution reopened the gate and doubled the cooldown; state = %s", got)
	}

	b.Record(c.now(), true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("a genuine probe did not close the gate; state = %s", got)
	}
}

func TestUnattributedOutcomesAreNotProbes(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)
	_ = b.Allow()

	b.Record(time.Time{}, true)
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("an outcome with no start time was treated as a probe; state = %s", got)
	}
}

// Lowering the ceiling during a backed-off outage must actually shorten the
// wait. Clamping only the duration would keep refusing work for the old hour
// and read as the reload having been ignored.
func TestLoweringMaxCooldownShortensTheOpenDeadline(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)
	_ = b.Allow()
	b.Record(c.now(), false) // cooldown -> 20m, openUntil = now + 20m

	lowered := testConfig()
	lowered.MaxCooldown = 12 * time.Minute
	b.SetConfig(lowered)

	if got := b.Snapshot().Cooldown; got != 12*time.Minute {
		t.Fatalf("cooldown = %s, want 12m", got)
	}
	c.add(13 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("gate still refused work past the lowered ceiling: %v", err)
	}
}

func TestLoweringProbeSuccessThresholdClosesCompletedHalfOpenRound(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 3
	b, c, transitions := newTestBreaker(t, cfg)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	b.Record(c.now(), false)
	c.add(10 * time.Minute)

	for i := 0; i < 2; i++ {
		probe, err := b.AllowAdmission()
		if err != nil || !probe {
			t.Fatalf("probe admission %d = (%t, %v), want reserved probe", i+1, probe, err)
		}
		b.RecordAdmission(c.now(), true, true)
	}
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("state before reload = %s, want half-open", got)
	}

	lowered := cfg
	lowered.ProbeSuccesses = 2
	b.SetConfig(lowered)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("state after lowering threshold = %s, want closed", got)
	}
	if len(*transitions) != 3 || (*transitions)[2].Reason != "probe_threshold_reduced" {
		t.Fatalf("transitions after threshold reload = %+v, want explicit recovery transition", *transitions)
	}
	if probe, err := b.AllowAdmission(); err != nil || probe {
		t.Fatalf("admission after reconciled reload = (%t, %v), want ordinary closed admission", probe, err)
	}
}
