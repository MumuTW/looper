package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// Contract for #150: settlement is asymmetric. Absence of the recorded process
// is conclusive; presence never is.
func TestRecordedProcessVerifiablyGoneIsAsymmetric(t *testing.T) {
	t.Parallel()

	livePID := int64(4242)
	identity := stringPtr(`{"processIdentity":{"startTime":2201,"bootId":"boot-a"}}`)

	for _, tc := range []struct {
		name      string
		execution storage.AgentExecutionRecord
		command   func(context.Context, int) (string, error)
		start     func(context.Context, int) (int64, error)
		bootID    func(context.Context, int) (string, error)
		want      processAbsenceReason
	}{
		{
			name:      "no process holds the pid",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "", nil },
			want:      processAbsenceRecordedProcessAbsent,
		},
		{
			// Rows written before process identity existed still settle when the
			// PID holds nothing: there is no other process to confuse ours with.
			name:      "no recorded identity and no process holds the pid",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`)},
			command:   func(context.Context, int) (string, error) { return "", nil },
			want:      processAbsenceRecordedProcessAbsent,
		},
		{
			name:      "pid reused by a different birth",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			start:     func(context.Context, int) (int64, error) { return 9999, nil },
			want:      processAbsenceRecordedProcessReplaced,
		},
		{
			name:      "recorded birth still matches the live process",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			start:     func(context.Context, int) (int64, error) { return 2201, nil },
			want:      processAbsenceRecordedProcessLive,
		},
		{
			// Present but unidentifiable: settling this would settle something we
			// cannot identify, which is exactly what the asymmetry forbids.
			name:      "no recorded identity while the pid is live",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`)},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			want:      processAbsenceIdentityUnavailable,
		},
		{
			name:      "probe failure",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "", errors.New("ps unavailable") },
			want:      processAbsenceIdentityUnavailable,
		},
		{
			name:      "no recorded pid",
			execution: storage.AgentExecutionRecord{CommandJSON: stringPtr(`{"command":"codex"}`)},
			command:   func(context.Context, int) (string, error) { return "", nil },
			want:      processAbsenceNoRecordedPID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			start := tc.start
			if start == nil {
				start = func(context.Context, int) (int64, error) { return 0, errors.New("unexpected start read") }
			}
			bootID := tc.bootID
			if bootID == nil {
				bootID = func(context.Context, int) (string, error) { return "boot-a", nil }
			}
			rt := &Runtime{readProcessCommand: tc.command, readProcessStart: start, readProcessBootID: bootID}
			got := rt.recordedProcessVerifiablyGone(context.Background(), tc.execution)
			if got != tc.want {
				t.Fatalf("recordedProcessVerifiablyGone() = %q, want %q", got, tc.want)
			}
			if got.provesAbsence() != (tc.want == processAbsenceRecordedProcessAbsent || tc.want == processAbsenceRecordedProcessReplaced) {
				t.Fatalf("provesAbsence() disagrees with reason %q", got)
			}
		})
	}
}

// Terminal rows (including the status='timeout' rows seen in #150) are already
// outside the active set, so settlement has nothing to do and must not rewrite
// an immutable terminal observation.
func TestSettleQuarantinedExecutionSkipsTerminalRows(t *testing.T) {
	t.Parallel()

	pid := int64(4242)
	rt := &Runtime{
		readProcessCommand: func(context.Context, int) (string, error) {
			t.Fatal("probed a terminal row")
			return "", nil
		},
	}
	for _, status := range []string{"timeout", "failed", "completed", "killed"} {
		settled, events, err := rt.settleQuarantinedExecution(context.Background(), nil, storage.AgentExecutionRecord{
			ID: "execution_terminal", Status: status, PID: &pid,
		}, "2026-07-30T12:00:00.000Z")
		if settled || events != 0 || err != nil {
			t.Fatalf("settleQuarantinedExecution(%q) = (%v, %d, %v), want no-op", status, settled, events, err)
		}
	}
}
