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
	b, _, _ := newTestBreaker(t, cfg)
	for i := 0; i < 100; i++ {
		b.Record(false)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("disabled breaker refused work: %v", err)
	}
}

func TestTripsOnSustainedFailureRate(t *testing.T) {
	b, _, transitions := newTestBreaker(t, testConfig())
	if err := b.Allow(); err != nil {
		t.Fatalf("healthy breaker refused work: %v", err)
	}
	b.Record(false)
	b.Record(false)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker opened below MinFailures: %v", err)
	}
	b.Record(false)
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
	b, _, _ := newTestBreaker(t, testConfig())
	for i := 0; i < 10; i++ {
		b.Record(true)
	}
	b.Record(false)
	b.Record(false)
	b.Record(false)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker opened on a 3/13 failure rate: %v", err)
	}
}

func TestStaleFailuresLeaveTheWindow(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(false)
	b.Record(false)
	c.add(6 * time.Minute)
	b.Record(false)
	if err := b.Allow(); err != nil {
		t.Fatalf("breaker counted failures older than the window: %v", err)
	}
}

func TestCooldownElapsesIntoHalfOpenThenCloses(t *testing.T) {
	b, c, transitions := newTestBreaker(t, testConfig())
	b.Record(false)
	b.Record(false)
	b.Record(false)
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

	b.Record(true)
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
	b.Record(false)
	b.Record(false)
	b.Record(false)

	c.add(10 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("expected half_open: %v", err)
	}
	b.Record(false)

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
	b.Record(false)
	b.Record(false)
	b.Record(false)
	for i := 0; i < 6; i++ {
		c.add(b.Snapshot().Cooldown)
		if err := b.Allow(); err != nil {
			t.Fatalf("round %d: expected half_open: %v", i, err)
		}
		b.Record(false)
	}
	if got := b.Snapshot().Cooldown; got != 40*time.Minute {
		t.Fatalf("cooldown grew past MaxCooldown: %s", got)
	}
}

func TestRecoveryResetsCooldownToConfigured(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	b.Record(false)
	b.Record(false)
	b.Record(false)
	c.add(10 * time.Minute)
	_ = b.Allow()
	b.Record(false) // cooldown -> 20m

	c.add(20 * time.Minute)
	_ = b.Allow()
	b.Record(true)

	if got := b.Snapshot().Cooldown; got != 10*time.Minute {
		t.Fatalf("recovery left the cooldown backed off at %s", got)
	}
}

// The cooldown must be observable: entering half-open discards the window that
// tripped the breaker, so the first Allow after a cooldown cannot be refused by
// failures the operator already waited out.
func TestHalfOpenDoesNotInheritTheTrippingWindow(t *testing.T) {
	b, c, _ := newTestBreaker(t, testConfig())
	for i := 0; i < 20; i++ {
		b.Record(false)
	}
	c.add(10 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("half_open inherited the tripping window: %v", err)
	}
	if got := b.Snapshot().Failures; got != 0 {
		t.Fatalf("expected an empty window in half_open, got %d failures", got)
	}
}

func TestMultipleProbeSuccessesRequired(t *testing.T) {
	cfg := testConfig()
	cfg.ProbeSuccesses = 2
	b, c, _ := newTestBreaker(t, cfg)
	b.Record(false)
	b.Record(false)
	b.Record(false)
	c.add(10 * time.Minute)
	_ = b.Allow()

	b.Record(true)
	if got := b.Snapshot().State; got != StateHalfOpen {
		t.Fatalf("closed after one probe when two were required, got %s", got)
	}
	b.Record(true)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("expected closed after two probes, got %s", got)
	}
}

func TestSnapshotReportsOpenUntilAndTrips(t *testing.T) {
	b, _, _ := newTestBreaker(t, testConfig())
	if snapshot := b.Snapshot(); snapshot.OpenUntil != nil {
		t.Fatalf("closed breaker reported openUntil %v", snapshot.OpenUntil)
	}
	b.Record(false)
	b.Record(false)
	b.Record(false)
	snapshot := b.Snapshot()
	if snapshot.OpenUntil == nil {
		t.Fatal("open breaker did not report openUntil")
	}
	if snapshot.Trips != 1 {
		t.Fatalf("expected 1 trip, got %d", snapshot.Trips)
	}
}

// The scheduler calls Allow from its own goroutines while a config reload calls
// SetConfig from another. Under -race this fails if any field of cfg is read
// outside the mutex.
func TestConcurrentAllowRecordAndSetConfigAreRaceFree(t *testing.T) {
	cfg := testConfig()
	b := New(cfg, time.Now, func(Transition) {})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				_ = b.Allow()
			}
		}()
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				b.Record(n%2 == 0)
			}
		}()
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				next := cfg
				next.Enabled = n%2 == 0
				b.SetConfig(next)
				_ = b.Snapshot()
			}
		}()
	}
	wg.Wait()
}

func TestNilBreakerIsInert(t *testing.T) {
	var b *Breaker
	if err := b.Allow(); err != nil {
		t.Fatalf("nil breaker refused work: %v", err)
	}
	b.Record(false)
	if got := b.Snapshot().State; got != StateClosed {
		t.Fatalf("nil breaker reported %s", got)
	}
}
