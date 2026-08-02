package runtime

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops/brownout"
)

const defaultAgentHealthVendor = "_default"

// agentHealthRegistry keeps provider health independent while retaining one
// lifecycle gate for scheduler work. The gateMu is held across a durable claim
// or enqueue section so an outcome cannot open a provider breaker between its
// admission check and the mutation it authorizes; removing it reintroduces the
// exact point-in-time race the brownout gate is meant to close. It is deliberately
// non-reentrant: callbacks must not spawn or consult provider health while the
// section is held, and operation leases use the outer scheduler admission.
type agentHealthRegistry struct {
	mu                sync.Mutex
	gateMu            sync.Mutex
	now               func() time.Time
	onChange          func(string, brownout.Transition)
	onWarning         func(string)
	cfg               brownout.Config
	configured        bool
	configuredVendors map[string]struct{}
	breakers          map[string]*brownout.Breaker
}

func newAgentHealthRegistry(cfg brownout.Config, daemonCfg config.Config, now func() time.Time, onChange func(string, brownout.Transition), onWarning func(string)) *agentHealthRegistry {
	if now == nil {
		now = time.Now
	}
	r := &agentHealthRegistry{now: now, onChange: onChange, onWarning: onWarning, cfg: cfg, configuredVendors: make(map[string]struct{}), breakers: make(map[string]*brownout.Breaker)}
	r.ensureConfiguredVendorsLocked(daemonCfg)
	if len(r.breakers) == 0 {
		r.ensureBreakerLocked(defaultAgentHealthVendor)
	}
	return r
}

func (r *agentHealthRegistry) ensureConfiguredVendorsLocked(cfg config.Config) {
	configured := false
	activeVendors := make(map[string]struct{})
	for _, role := range []string{config.CodingRolePlanner, config.CodingRoleWorker, config.CodingRoleReviewer, config.CodingRoleFixer} {
		if resolved, ok := config.ResolveAgent(cfg, "", role); ok {
			configured = true
			activeVendors[string(resolved.Vendor)] = struct{}{}
			r.ensureBreakerLocked(string(resolved.Vendor))
		}
	}
	for vendor := range r.breakers {
		if vendor == defaultAgentHealthVendor {
			continue
		}
		if _, ok := activeVendors[vendor]; !ok {
			delete(r.breakers, vendor)
		}
	}
	r.configured = configured
	r.configuredVendors = activeVendors
	if configured {
		// The default bucket is only for embedders/legacy configs with no
		// effective vendor. Once a real provider is configured it must not keep
		// AllowAny healthy and bypass all provider-specific breakers.
		delete(r.breakers, defaultAgentHealthVendor)
	}
}

func (r *agentHealthRegistry) ensureBreakerLocked(vendor string) *brownout.Breaker {
	key := strings.TrimSpace(vendor)
	if key == "" {
		key = defaultAgentHealthVendor
	}
	if breaker := r.breakers[key]; breaker != nil {
		return breaker
	}
	keyCopy := key
	breaker := brownout.New(r.cfg, r.now, func(transition brownout.Transition) {
		if r.onChange != nil {
			r.onChange(keyCopy, transition)
		}
	})
	r.breakers[key] = breaker
	return breaker
}

func (r *agentHealthRegistry) breaker(vendor string) *brownout.Breaker {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureBreakerLocked(vendor)
}

func (r *agentHealthRegistry) Allow(vendor string) error {
	if r == nil {
		return nil
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	breaker, err := r.admissionBreaker(vendor)
	if err != nil {
		return err
	}
	return breaker.Allow()
}

// With runs a provider-scoped durable mutation only while that provider is
// admitted. It is intentionally separate from WithAny: a healthy provider
// must not authorize a queue claim that will later be routed to another
// provider whose breaker is open.
func (r *agentHealthRegistry) With(vendor string, fn func()) error {
	if r == nil {
		if fn != nil {
			fn()
		}
		return nil
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	breaker, err := r.admissionBreaker(vendor)
	if err != nil {
		return err
	}
	if err := breaker.Allow(); err != nil {
		return err
	}
	if fn != nil {
		fn()
	}
	return nil
}

func (r *agentHealthRegistry) AllowAdmission(vendor string) (bool, error) {
	if r == nil {
		return false, nil
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	breaker, err := r.admissionBreaker(vendor)
	if err != nil {
		return false, err
	}
	return breaker.AllowAdmission()
}

// AllowAny admits work when at least one configured provider is healthy. A
// single exhausted provider therefore cannot suspend independent providers;
// provider-specific spawn admission still refuses work routed to the open one.
func (r *agentHealthRegistry) AllowAny() error {
	if r == nil {
		return nil
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	breakers := r.snapshotBreakers()
	var firstErr error
	for _, breaker := range breakers {
		if err := breaker.Allow(); err == nil {
			return nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *agentHealthRegistry) WithAny(fn func()) error {
	if r == nil {
		if fn != nil {
			fn()
		}
		return nil
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	breakers := r.snapshotBreakers()
	var firstErr error
	for _, breaker := range breakers {
		if err := breaker.Allow(); err == nil {
			if fn != nil {
				fn()
			}
			return nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *agentHealthRegistry) Record(vendor string, startedAt time.Time, ok, probe bool) {
	if r == nil {
		return
	}
	r.gateMu.Lock()
	// Once the daemon has real role bindings, an unattributed outcome is not a
	// safe default-provider signal: creating a healthy fallback bucket would let
	// it bypass AllowAny even when every configured provider is open.
	if strings.TrimSpace(vendor) == "" {
		r.mu.Lock()
		configured := r.configured
		r.mu.Unlock()
		if configured {
			r.gateMu.Unlock()
			if r.onWarning != nil {
				r.onWarning("agent outcome missing effective provider attribution; outcome ignored")
			}
			return
		}
	}
	if !r.vendorActive(vendor) {
		r.gateMu.Unlock()
		if r.onWarning != nil {
			r.onWarning("agent outcome used a provider no longer configured; outcome ignored")
		}
		return
	}
	breaker := r.breaker(vendor)
	// Legacy/default outcomes may carry a timestamp but no spawn token. Let the
	// breaker validate that timestamp against its half-open boundary; configured
	// providers must carry the explicit token from the common spawn lease.
	if !probe && strings.TrimSpace(vendor) == "" && !startedAt.IsZero() {
		probe = true
	}
	breaker.RecordAdmission(startedAt, ok, probe)
	r.gateMu.Unlock()
}

func (r *agentHealthRegistry) SetConfig(cfg brownout.Config, daemonCfg config.Config) {
	if r == nil {
		return
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	r.mu.Lock()
	r.cfg = cfg
	r.ensureConfiguredVendorsLocked(daemonCfg)
	if len(r.breakers) == 0 {
		r.ensureBreakerLocked(defaultAgentHealthVendor)
	}
	breakers := make([]*brownout.Breaker, 0, len(r.breakers))
	for _, breaker := range r.breakers {
		breakers = append(breakers, breaker)
	}
	r.mu.Unlock()
	for _, breaker := range breakers {
		breaker.SetConfig(cfg)
	}
}

func (r *agentHealthRegistry) snapshotBreakers() []*brownout.Breaker {
	r.mu.Lock()
	keys := make([]string, 0, len(r.breakers))
	for key := range r.breakers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	breakers := make([]*brownout.Breaker, 0, len(keys))
	for _, key := range keys {
		breakers = append(breakers, r.breakers[key])
	}
	r.mu.Unlock()
	return breakers
}

func (r *agentHealthRegistry) vendorActive(vendor string) bool {
	key := strings.TrimSpace(vendor)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured {
		return true
	}
	_, ok := r.configuredVendors[key]
	return ok
}

// admissionBreaker resolves legacy empty/default spawn metadata only when the
// daemon has exactly one active provider. That keeps older embedders and
// lifecycle tests working without creating a fallback bucket that could bypass
// a multi-provider brownout; ambiguous metadata remains fail-closed.
func (r *agentHealthRegistry) admissionBreaker(vendor string) (*brownout.Breaker, error) {
	key := strings.TrimSpace(vendor)
	r.mu.Lock()
	if r.configured {
		if _, ok := r.configuredVendors[key]; !ok && (key == "" || key == defaultAgentHealthVendor) && len(r.configuredVendors) == 1 {
			for active := range r.configuredVendors {
				key = active
			}
		}
		if _, ok := r.configuredVendors[key]; !ok {
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: provider %q is no longer configured", brownout.ErrOpen, strings.TrimSpace(vendor))
		}
	}
	r.mu.Unlock()
	return r.breaker(key), nil
}

func (r *agentHealthRegistry) Snapshot() brownout.Summary {
	if r == nil {
		return brownout.Summary{State: brownout.StateClosed}
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	summary := brownout.Summary{State: brownout.StateClosed}
	breakers := r.snapshotBreakers()
	allOpen := len(breakers) > 0
	hasHalfOpen := false
	hasClosed := false
	for _, breaker := range breakers {
		current := breaker.Snapshot()
		summary.Failures += current.Failures
		summary.Total += current.Total
		summary.Trips += current.Trips
		if current.State == brownout.StateOpen {
			if current.OpenUntil != nil && (summary.OpenUntil == nil || current.OpenUntil.Before(*summary.OpenUntil)) {
				until := *current.OpenUntil
				summary.OpenUntil = &until
			}
			summary.Cooldown = maxDuration(summary.Cooldown, current.Cooldown)
			continue
		}
		allOpen = false
		switch current.State {
		case brownout.StateHalfOpen:
			hasHalfOpen = true
			summary.Cooldown = maxDuration(summary.Cooldown, current.Cooldown)
		case brownout.StateClosed:
			hasClosed = true
		}
	}
	// The aggregate status describes the scheduler's AllowAny authority: one
	// open provider does not mean all work is paused while another is healthy.
	// Keep openUntil only when every known provider is open; otherwise it would
	// misleadingly suggest a global suspension that the scheduler does not have.
	if allOpen {
		summary.State = brownout.StateOpen
	} else if hasHalfOpen && !hasClosed {
		summary.State = brownout.StateHalfOpen
		summary.OpenUntil = nil
	} else {
		summary.State = brownout.StateClosed
		summary.OpenUntil = nil
	}
	return summary
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
