// Package brownout gates work production on looper's own agent failure rate.
//
// Providers report exhaustion in shapes looper cannot enumerate: an HTTP 429, a
// 503, a "Permission denied: Reached overall message rate limit" string, a
// silent hang. Recognizing each of them is a losing game, and getting it wrong
// means retrying an exhausted quota every few seconds for hours.
//
// This package refuses to guess. It observes only what looper always knows
// first-hand — whether its own agent runs are succeeding — and stops producing
// work when enough of them are not. The provider's identity, error format, and
// stated reset window are all outside the model on purpose.
//
// The breaker is global rather than per-loop because provider quota is
// account-wide: a per-loop breaker sees one loop's failures and lets the other
// nine keep spending the same exhausted budget.
package brownout

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrOpen is returned by Allow while work production is suspended.
var ErrOpen = errors.New("agent brownout: work production suspended")

// State is the breaker's admission posture.
type State string

const (
	// StateClosed admits work: agent outcomes are healthy enough.
	StateClosed State = "closed"
	// StateOpen refuses all work production until the cooldown elapses.
	StateOpen State = "open"
	// StateHalfOpen admits work again to find out whether the provider
	// recovered. A single failure sends it back to open with a longer cooldown.
	StateHalfOpen State = "half_open"
)

// Config is the operator-facing policy. Zero values are not defaults — callers
// pass a config already resolved from config.SchedulerConfig.
type Config struct {
	// Enabled turns the whole gate off when false. Allow then never refuses.
	Enabled bool
	// Window is the rolling period over which outcomes are counted.
	Window time.Duration
	// MinFailures is the absolute floor below which the ratio is not
	// meaningful. Three failures out of three is a 100% failure rate and says
	// almost nothing on a daemon that has run four agents all day.
	MinFailures int
	// FailureRatio is the share of in-window outcomes that must be failures.
	// Expressed as a ratio rather than a count so the same policy holds for a
	// one-project laptop and a fleet, which is why there is no absolute
	// "failures per minute" knob anywhere in this package.
	FailureRatio float64
	// Cooldown is the operator's safe interval: how long to stay open before
	// the first probe. This is the "wait it out" number, and it is deliberately
	// the operator's to choose — looper cannot read the provider's reset clock.
	Cooldown time.Duration
	// MaxCooldown caps the doubling that follows probes which keep failing.
	MaxCooldown time.Duration
	// ProbeSuccesses is how many consecutive successful agent outcomes in
	// half-open are needed to close the breaker.
	ProbeSuccesses int
}

// Transition describes a state change for logging and notification.
type Transition struct {
	From State
	To   State
	// Failures and Total describe the window that produced the transition.
	Failures int
	Total    int
	// Cooldown is how long the breaker will stay open, when To is StateOpen.
	Cooldown time.Duration
	// Reason is a short machine-readable cause.
	Reason string
}

// Summary reports the breaker's live posture for status endpoints.
type Summary struct {
	State     State         `json:"state"`
	Failures  int           `json:"failures"`
	Total     int           `json:"total"`
	OpenUntil *time.Time    `json:"openUntil,omitempty"`
	Cooldown  time.Duration `json:"-"`
	Trips     int           `json:"trips"`
}

type outcome struct {
	at time.Time
	ok bool
}

// Breaker is safe for concurrent use.
type Breaker struct {
	mu       sync.Mutex
	cfg      Config
	now      func() time.Time
	onChange func(Transition)

	state    State
	outcomes []outcome

	// openUntil is when the current open period ends. Zero unless state is open.
	openUntil time.Time
	// cooldown is the current backoff, doubled each time a probe round fails.
	cooldown time.Duration
	// probeSuccesses counts consecutive successes observed in half-open.
	probeSuccesses int
	// trips counts open transitions for the lifetime of the process.
	trips int
	// halfOpenAt is when the current half-open period began. Outcomes from
	// executions that started before it are not probes: a worker admitted
	// before the gate opened can outlive a 15-minute cooldown, and letting its
	// result decide recovery would close the gate without ever testing whether
	// the provider recovered.
	halfOpenAt time.Time
	// tripFailures and tripTotal retain the evidence that opened the current
	// outage. The rolling window is cleared on open so it cannot re-trip from
	// stale failures, but status surfaces still need to explain why the gate is
	// open (or waiting for its first probe).
	tripFailures int
	tripTotal    int
}

// New builds a breaker. now and onChange may be nil.
func New(cfg Config, now func() time.Time, onChange func(Transition)) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{cfg: cfg, now: now, onChange: onChange, state: StateClosed, cooldown: cfg.Cooldown}
}

// Allow reports whether work production may proceed. It is the only method the
// scheduler calls, and it advances open -> half-open when the cooldown expires,
// so no separate timer owns recovery.
func (b *Breaker) Allow() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	// cfg is read under the mutex because SetConfig writes it from the config
	// reload path while the scheduler is calling this from its own goroutines.
	if !b.cfg.Enabled {
		b.mu.Unlock()
		return nil
	}
	transition, ok := b.refreshLocked()
	state := b.state
	remaining := b.openUntil.Sub(b.now())
	b.mu.Unlock()

	if ok {
		b.emit(transition)
	}
	if state == StateOpen {
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Errorf("%w: retrying in %s", ErrOpen, remaining.Round(time.Second))
	}
	return nil
}

// Record observes one agent outcome. Callers pass only outcomes attributable to
// the agent boundary; a failed git push or a rejected policy check is looper's
// own problem and must not open a gate meant for provider trouble.
func (b *Breaker) Record(startedAt time.Time, ok bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if !b.cfg.Enabled {
		b.mu.Unlock()
		return
	}
	now := b.now()
	b.outcomes = append(b.outcomes, outcome{at: now, ok: ok})
	b.pruneLocked(now)

	transition, changed := b.evaluateLocked(now, startedAt, ok)
	b.mu.Unlock()

	if changed {
		b.emit(transition)
	}
}

// SetConfig republishes policy on config reload. It deliberately preserves
// state: an operator who edits config while the gate is open must not thereby
// resume the hammering it stopped. The one exception is disabling the gate,
// which is an explicit instruction to stop refusing work.
func (b *Breaker) SetConfig(cfg Config) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	previous := b.cfg
	b.cfg = cfg
	if !cfg.Enabled {
		b.state = StateClosed
		b.outcomes = nil
		b.probeSuccesses = 0
		b.cooldown = cfg.Cooldown
		b.tripFailures = 0
		b.tripTotal = 0
		return
	}
	// A cooldown the operator shortened should take effect on the next round,
	// not retroactively extend or truncate the one already being served, except
	// where the new maximum is lower than the backoff currently in force.
	if previous.Cooldown != cfg.Cooldown && b.state == StateClosed {
		b.cooldown = cfg.Cooldown
	}
	if cfg.MaxCooldown > 0 && b.cooldown > cfg.MaxCooldown {
		// Move the deadline with the duration. Lowering the maximum while an
		// exponentially backed-off interval is being served must actually
		// shorten the wait; leaving openUntil alone would keep refusing work
		// for the old hour after the operator lowered the ceiling to fifteen
		// minutes, which reads as the reload having been ignored.
		if b.state == StateOpen {
			shortened := b.openUntil.Add(cfg.MaxCooldown - b.cooldown)
			if shortened.Before(b.openUntil) {
				b.openUntil = shortened
			}
		}
		b.cooldown = cfg.MaxCooldown
	}
	if b.cooldown <= 0 {
		b.cooldown = cfg.Cooldown
	}
}

// Snapshot reports the current posture.
func (b *Breaker) Snapshot() Summary {
	if b == nil {
		return Summary{State: StateClosed}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.pruneLocked(now)
	failures, total := b.countLocked()
	state := b.state
	// Snapshot is intentionally observational: do not advance the breaker or
	// emit a transition from a status read. Still, once the deadline elapsed,
	// reporting open would claim work remains suspended even though the next
	// Allow call may start a probe.
	deadlineElapsed := b.state == StateOpen && !now.Before(b.openUntil)
	if b.state == StateOpen || b.state == StateHalfOpen {
		failures, total = b.tripFailures, b.tripTotal
	}
	if deadlineElapsed {
		state = StateHalfOpen
	}
	summary := Summary{State: state, Failures: failures, Total: total, Cooldown: b.cooldown, Trips: b.trips}
	if b.state == StateOpen && !deadlineElapsed {
		openUntil := b.openUntil
		summary.OpenUntil = &openUntil
	}
	return summary
}

// refreshLocked advances open -> half-open once the cooldown has elapsed.
func (b *Breaker) refreshLocked() (Transition, bool) {
	if b.state != StateOpen {
		return Transition{}, false
	}
	now := b.now()
	if now.Before(b.openUntil) {
		return Transition{}, false
	}
	b.state = StateHalfOpen
	b.probeSuccesses = 0
	b.halfOpenAt = now
	// Drop the window that tripped the breaker. Keeping it would let stale
	// failures re-trip the breaker on the first probe before any new outcome
	// has been observed, which would make the cooldown unobservable.
	b.outcomes = nil
	return Transition{From: StateOpen, To: StateHalfOpen, Reason: "cooldown_elapsed"}, true
}

func (b *Breaker) evaluateLocked(now time.Time, startedAt time.Time, ok bool) (Transition, bool) {
	switch b.state {
	case StateHalfOpen:
		// Only executions admitted during this half-open period are probes. A
		// zero startedAt means the caller could not attribute it, which counts
		// as "not a probe" rather than being guessed at.
		if startedAt.IsZero() || startedAt.Before(b.halfOpenAt) {
			return Transition{}, false
		}
		if !ok {
			// One failed probe is enough: the whole point of half-open is to
			// spend a single round of calls, not to re-derive the failure rate.
			b.cooldown = b.nextCooldown()
			return b.openLocked(now, "probe_failed"), true
		}
		b.probeSuccesses++
		if b.probeSuccesses < b.cfg.ProbeSuccesses {
			return Transition{}, false
		}
		b.state = StateClosed
		b.cooldown = b.cfg.Cooldown
		b.probeSuccesses = 0
		b.outcomes = nil
		return Transition{From: StateHalfOpen, To: StateClosed, Reason: "probes_succeeded"}, true
	case StateClosed:
		failures, total := b.countLocked()
		if failures < b.cfg.MinFailures || total == 0 {
			return Transition{}, false
		}
		if float64(failures)/float64(total) < b.cfg.FailureRatio {
			return Transition{}, false
		}
		transition := b.openLocked(now, "failure_rate")
		transition.Failures = failures
		transition.Total = total
		return transition, true
	default:
		return Transition{}, false
	}
}

func (b *Breaker) openLocked(now time.Time, reason string) Transition {
	from := b.state
	b.state = StateOpen
	b.openUntil = now.Add(b.cooldown)
	b.probeSuccesses = 0
	b.trips++
	failures, total := b.countLocked()
	b.tripFailures = failures
	b.tripTotal = total
	// The window is cleared on the way into open so the breaker measures the
	// recovery, not the outage that is already accounted for.
	b.outcomes = nil
	return Transition{From: from, To: StateOpen, Failures: failures, Total: total, Cooldown: b.cooldown, Reason: reason}
}

func (b *Breaker) nextCooldown() time.Duration {
	next := b.cooldown * 2
	if b.cfg.MaxCooldown > 0 && next > b.cfg.MaxCooldown {
		next = b.cfg.MaxCooldown
	}
	if next <= 0 {
		next = b.cfg.Cooldown
	}
	return next
}

func (b *Breaker) pruneLocked(now time.Time) {
	if b.cfg.Window <= 0 {
		return
	}
	cutoff := now.Add(-b.cfg.Window)
	keep := 0
	for _, o := range b.outcomes {
		if o.at.Before(cutoff) {
			keep++
			continue
		}
		break
	}
	if keep > 0 {
		b.outcomes = append(b.outcomes[:0], b.outcomes[keep:]...)
	}
}

func (b *Breaker) countLocked() (failures, total int) {
	for _, o := range b.outcomes {
		total++
		if !o.ok {
			failures++
		}
	}
	return failures, total
}

func (b *Breaker) emit(transition Transition) {
	if b.onChange == nil {
		return
	}
	b.onChange(transition)
}
