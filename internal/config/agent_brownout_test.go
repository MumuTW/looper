package config

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDefaultAgentBrownoutIsEnabledAndCoherent(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	brownout := cfg.Scheduler.AgentBrownout
	if !brownout.Enabled {
		t.Fatal("agent brownout is off by default; a daemon that hammers an exhausted provider is the default failure mode")
	}
	if !brownout.Notify {
		t.Fatal("agent brownout notification is off by default; silent failure is the reported symptom")
	}
	if brownout.MaxCooldownSeconds < brownout.CooldownSeconds {
		t.Fatalf("default maxCooldownSeconds %d is below cooldownSeconds %d", brownout.MaxCooldownSeconds, brownout.CooldownSeconds)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() on defaults error = %v", err)
	}
}

// The default must be inert on a daemon that merely fails sometimes, or nobody
// will leave it on.
func TestDefaultAgentBrownoutRatioIsNotMerelyACount(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if ratio := cfg.Scheduler.AgentBrownout.FailureRatio; ratio <= 0.5 {
		t.Fatalf("default failureRatio %v would trip on a daemon that is half working", ratio)
	}
}

func TestValidateAgentBrownout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*AgentBrownoutConfig)
		wantPath string
		wantMsg  string
	}{
		{name: "defaults accepted", mutate: func(*AgentBrownoutConfig) {}},
		{
			name:     "zero window rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.WindowSeconds = 0 },
			wantPath: "scheduler.agentBrownout.windowSeconds",
			wantMsg:  "must be a positive integer",
		},
		{
			name:     "zero minFailures rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.MinFailures = 0 },
			wantPath: "scheduler.agentBrownout.minFailures",
			wantMsg:  "must be a positive integer",
		},
		{
			name:     "ratio above one rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.FailureRatio = 1.5 },
			wantPath: "scheduler.agentBrownout.failureRatio",
			wantMsg:  "must be a number in (0, 1]",
		},
		{
			name:     "zero ratio rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.FailureRatio = 0 },
			wantPath: "scheduler.agentBrownout.failureRatio",
			wantMsg:  "must be a number in (0, 1]",
		},
		{
			name:     "zero cooldown rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.CooldownSeconds = 0 },
			wantPath: "scheduler.agentBrownout.cooldownSeconds",
			wantMsg:  "must be a positive integer",
		},
		{
			name:     "max below base rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.MaxCooldownSeconds = b.CooldownSeconds - 1 },
			wantPath: "scheduler.agentBrownout.maxCooldownSeconds",
			wantMsg:  "must be an integer >= scheduler.agentBrownout.cooldownSeconds",
		},
		{
			name:     "zero probeSuccesses rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.ProbeSuccesses = 0 },
			wantPath: "scheduler.agentBrownout.probeSuccesses",
			wantMsg:  "must be a positive integer",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			tt.mutate(&cfg.Scheduler.AgentBrownout)
			err = Validate(cfg)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			var validationErr *ConfigValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
			}
			assertValidationIssue(t, validationErr, tt.wantPath, tt.wantMsg)
		})
	}
}

// Turning the gate off must not require keeping its numbers coherent, or
// disabling it becomes a multi-field edit during an incident.
func TestDisabledAgentBrownoutSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.AgentBrownout = AgentBrownoutConfig{Enabled: false}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() on a disabled brownout error = %v", err)
	}
}

func TestPartialAgentBrownoutMergesFieldwise(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	baseline := cfg.Scheduler.AgentBrownout

	var partial PartialSchedulerConfig
	if err := json.Unmarshal([]byte(`{"agentBrownout":{"cooldownSeconds":120}}`), &partial); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	mergeSchedulerConfig(&cfg.Scheduler, partial)

	got := cfg.Scheduler.AgentBrownout
	if got.CooldownSeconds != 120 {
		t.Fatalf("cooldownSeconds = %d, want 120", got.CooldownSeconds)
	}
	if got.FailureRatio != baseline.FailureRatio || got.WindowSeconds != baseline.WindowSeconds || got.Enabled != baseline.Enabled {
		t.Fatalf("unset fields were clobbered: %+v", got)
	}
}
