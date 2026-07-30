package triager

import (
	"testing"
)

func TestRunnerMaxPendingReadsPerTickDefault(t *testing.T) {
	r := New(Options{})
	if r.maxPendingReadsPerTick != defaultMaxPendingReadsPerTick {
		t.Fatalf("maxPendingReadsPerTick = %d, want %d", r.maxPendingReadsPerTick, defaultMaxPendingReadsPerTick)
	}
}

func TestRunnerMaxPendingReadsPerTickCustom(t *testing.T) {
	r := New(Options{MaxPendingReadsPerTick: 50})
	if r.maxPendingReadsPerTick != 50 {
		t.Fatalf("maxPendingReadsPerTick = %d, want 50", r.maxPendingReadsPerTick)
	}
}

func TestOptionsHasBudgetField(t *testing.T) {
	o := Options{MaxPendingReadsPerTick: 99}
	if o.MaxPendingReadsPerTick != 99 {
		t.Fatal("Options.MaxPendingReadsPerTick field not accessible")
	}
}

func TestDefaultBudgetValue(t *testing.T) {
	if defaultMaxPendingReadsPerTick != 25 {
		t.Fatalf("defaultMaxPendingReadsPerTick = %d, want 25", defaultMaxPendingReadsPerTick)
	}
}

func TestRunnerHasBudgetField(t *testing.T) {
	r := &Runner{maxPendingReadsPerTick: 42}
	if r.maxPendingReadsPerTick != 42 {
		t.Fatal("Runner.maxPendingReadsPerTick field not accessible")
	}
}

func TestDiscoveryResultStructIncludesPendingReadsExhausted(t *testing.T) {
	var result DiscoveryResult
	result.PendingReadsExhausted = 5
	if result.PendingReadsExhausted != 5 {
		t.Fatal("PendingReadsExhausted field not accessible")
	}
}
