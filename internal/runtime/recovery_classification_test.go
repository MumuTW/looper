package runtime

import (
	"context"
	"os/exec"
	"testing"

	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

// The classifier has two outcomes and one live input. This table is the whole
// surface; the 3-class × 5-probe-reason matrix it replaces is gone because PID
// probes no longer participate in recovery at all.
func TestClassifyDurableExecution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		execution storage.AgentExecutionRecord
		ownsLive  bool
		want      ContainmentClass
		reason    string
	}{
		{
			name:      "durable terminal status",
			execution: storage.AgentExecutionRecord{ID: "e1", Status: "killed", PID: int64Ptr(99)},
			want:      ContainmentConfirmedDead,
			reason:    "durable_terminal_finalization",
		},
		{
			name:      "running row this daemon supervises",
			execution: storage.AgentExecutionRecord{ID: "e2", Status: "running", PID: int64Ptr(99)},
			ownsLive:  true,
			want:      ContainmentCurrentDaemonOwned,
			reason:    "current_daemon_supervisor_handle",
		},
		{
			name:      "running row from a previous daemon",
			execution: storage.AgentExecutionRecord{ID: "e3", Status: "running", PID: int64Ptr(99)},
			want:      ContainmentConfirmedDead,
			reason:    "stale_generation_retired",
		},
		{
			name:      "running row with no pid at all",
			execution: storage.AgentExecutionRecord{ID: "e4", Status: "running"},
			want:      ContainmentConfirmedDead,
			reason:    "stale_generation_retired",
		},
		{
			name:      "a live handle outranks a terminal-looking cancel",
			execution: storage.AgentExecutionRecord{ID: "e5", Status: "cancelling", PID: int64Ptr(99)},
			ownsLive:  true,
			want:      ContainmentCurrentDaemonOwned,
			reason:    "current_daemon_supervisor_handle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyDurableExecution(tc.execution, nil, tc.ownsLive)
			if got.Class != tc.want || got.Reason != tc.reason {
				t.Fatalf("classify = %#v, want class=%s reason=%s", got, tc.want, tc.reason)
			}
			if classificationAllowsTerminalOrRequeue(got.Class) != (tc.want == ContainmentConfirmedDead) {
				t.Fatalf("terminal authority for %s = %v", got.Class, classificationAllowsTerminalOrRequeue(got.Class))
			}
		})
	}
}

func TestClassifyConfirmedDeadFromCurrentDaemonDrainedHandle(t *testing.T) {
	t.Parallel()

	active := storage.AgentExecutionRecord{ID: "e2", Status: "running", PID: int64Ptr(99)}
	if _, ok := classifyFromDurableStatusAndHandle(active, nil); ok {
		t.Fatal("active status without handle must not be confirmed_dead by status alone")
	}

	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true binary not available")
	}
	cmd := exec.Command(truePath)
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if err := handle.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle not ConfirmedDead after Drain")
	}
	class, ok := classifyFromDurableStatusAndHandle(active, handle)
	if !ok || class.Class != ContainmentConfirmedDead || class.Reason != "current_daemon_confirmed_drain" {
		t.Fatalf("drained handle classification = %#v ok=%v", class, ok)
	}
}
