package config

import (
	"encoding/json"
	"errors"
	"math"
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
			name:     "window duration overflow rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.WindowSeconds = int(maxAgentBrownoutDurationSeconds + 1) },
			wantPath: "scheduler.agentBrownout.windowSeconds",
			wantMsg:  "must not exceed time.Duration maximum in seconds",
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
			name:     "NaN ratio rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.FailureRatio = math.NaN() },
			wantPath: "scheduler.agentBrownout.failureRatio",
			wantMsg:  "must be a number in (0, 1]",
		},
		{
			name:     "Inf ratio rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.FailureRatio = math.Inf(1) },
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
			name:     "cooldown duration overflow rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.CooldownSeconds = int(maxAgentBrownoutDurationSeconds + 1) },
			wantPath: "scheduler.agentBrownout.cooldownSeconds",
			wantMsg:  "must not exceed time.Duration maximum in seconds",
		},
		{
			name:     "max below base rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.MaxCooldownSeconds = b.CooldownSeconds - 1 },
			wantPath: "scheduler.agentBrownout.maxCooldownSeconds",
			wantMsg:  "must be an integer >= scheduler.agentBrownout.cooldownSeconds",
		},
		{
			name:     "max cooldown duration overflow rejected",
			mutate:   func(b *AgentBrownoutConfig) { b.MaxCooldownSeconds = int(maxAgentBrownoutDurationSeconds + 1) },
			wantPath: "scheduler.agentBrownout.maxCooldownSeconds",
			wantMsg:  "must not exceed time.Duration maximum in seconds",
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

func TestAgentBrownoutEnvironmentAndCLIPrecedence(t *testing.T) {
	t.Parallel()

	file := PartialConfig{Scheduler: &PartialSchedulerConfig{AgentBrownout: &PartialAgentBrownoutConfig{
		Enabled:            boolPtr(false),
		WindowSeconds:      intPtr(30),
		MinFailures:        intPtr(2),
		FailureRatio:       floatPtr(0.4),
		CooldownSeconds:    intPtr(60),
		MaxCooldownSeconds: intPtr(120),
		ProbeSuccesses:     intPtr(1),
		Notify:             boolPtr(false),
	}}}
	env, err := buildEnvOverrides(mapEnvLookup(map[string]string{
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_ENABLED":              "true",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_WINDOW_SECONDS":       "300",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_MIN_FAILURES":         "4",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_FAILURE_RATIO":        "0.7",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_COOLDOWN_SECONDS":     "90",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_MAX_COOLDOWN_SECONDS": "600",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_PROBE_SUCCESSES":      "2",
		"LOOPER_SCHEDULER_AGENT_BROWNOUT_NOTIFY":               "true",
	}))
	if err != nil {
		t.Fatalf("buildEnvOverrides() error = %v", err)
	}
	fromEnv, err := Normalize(t.TempDir(), file, env)
	if err != nil {
		t.Fatalf("Normalize(file, env) error = %v", err)
	}
	if got := fromEnv.Scheduler.AgentBrownout; got.WindowSeconds != 300 || got.MinFailures != 4 || got.FailureRatio != 0.7 || got.CooldownSeconds != 90 || got.MaxCooldownSeconds != 600 || got.ProbeSuccesses != 2 || !got.Enabled || !got.Notify {
		t.Fatalf("environment overrides did not beat file values: %+v", got)
	}

	parsed, err := parseCLIArgs([]string{
		"--scheduler-agent-brownout-enabled=false",
		"--scheduler-agent-brownout-window-seconds=900",
		"--scheduler-agent-brownout-min-failures=7",
		"--scheduler-agent-brownout-failure-ratio=0.9",
		"--scheduler-agent-brownout-cooldown-seconds=120",
		"--scheduler-agent-brownout-max-cooldown-seconds=900",
		"--scheduler-agent-brownout-probe-successes=3",
		"--scheduler-agent-brownout-notify=false",
	})
	if err != nil {
		t.Fatalf("parseCLIArgs() error = %v", err)
	}
	fromCLI, err := Normalize(t.TempDir(), file, env, parsed.overrides)
	if err != nil {
		t.Fatalf("Normalize(file, env, cli) error = %v", err)
	}
	got := fromCLI.Scheduler.AgentBrownout
	if got.Enabled || got.Notify || got.WindowSeconds != 900 || got.MinFailures != 7 || got.FailureRatio != 0.9 || got.CooldownSeconds != 120 || got.MaxCooldownSeconds != 900 || got.ProbeSuccesses != 3 {
		t.Fatalf("CLI overrides did not beat environment/file values: %+v", got)
	}
}

func TestAgentBrownoutEnvironmentRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	_, err := buildEnvOverrides(mapEnvLookup(map[string]string{"LOOPER_SCHEDULER_AGENT_BROWNOUT_FAILURE_RATIO": "not-a-number"}))
	if err == nil {
		t.Fatal("buildEnvOverrides() accepted invalid failure ratio")
	}
	if got := err.Error(); got != `invalid value for LOOPER_SCHEDULER_AGENT_BROWNOUT_FAILURE_RATIO: "not-a-number" is not a number` {
		t.Fatalf("error = %q, want precise environment variable error", got)
	}
}

func TestAgentBrownoutCLIRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	_, err := parseCLIArgs([]string{"--scheduler-agent-brownout-failure-ratio=oops"})
	if err == nil {
		t.Fatal("parseCLIArgs() accepted invalid failure ratio")
	}
	if got := err.Error(); got != `invalid value for --scheduler-agent-brownout-failure-ratio: "oops" is not a number` {
		t.Fatalf("error = %q, want precise CLI flag error", got)
	}
}

func TestAgentBrownoutRejectsNonFiniteRatiosFromEnvironmentAndCLI(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"NaN", "Inf", "-Inf", "Infinity"} {
		value := value
		t.Run("env_"+value, func(t *testing.T) {
			t.Parallel()
			if _, err := buildEnvOverrides(mapEnvLookup(map[string]string{
				"LOOPER_SCHEDULER_AGENT_BROWNOUT_FAILURE_RATIO": value,
			})); err == nil {
				t.Fatalf("buildEnvOverrides() accepted non-finite ratio %q", value)
			}
		})
		t.Run("cli_"+value, func(t *testing.T) {
			t.Parallel()
			if _, err := parseCLIArgs([]string{"--scheduler-agent-brownout-failure-ratio=" + value}); err == nil {
				t.Fatalf("parseCLIArgs() accepted non-finite ratio %q", value)
			}
		})
	}
}

func floatPtr(value float64) *float64 { return &value }
