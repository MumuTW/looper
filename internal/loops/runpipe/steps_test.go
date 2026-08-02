package runpipe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

type stepTrace struct {
	events []string
}

func (tr *stepTrace) runner(failOn string, handleFailure bool, stopAfter string) StepRunner[string, int] {
	return StepRunner[string, int]{
		PersistStarted: func(_ context.Context, run storage.RunRecord, step string, c int) (storage.RunRecord, error) {
			tr.events = append(tr.events, "persist-start:"+step)
			return run, nil
		},
		PersistCompleted: func(_ context.Context, run storage.RunRecord, step string, c int) (storage.RunRecord, error) {
			tr.events = append(tr.events, "persist-done:"+step)
			return run, nil
		},
		EmitStepEvent: func(_ context.Context, eventType, step string, _ storage.RunRecord) {
			tr.events = append(tr.events, eventType+":"+step)
		},
		Execute: func(_ context.Context, step string, _ storage.RunRecord, c int) (int, error) {
			tr.events = append(tr.events, "execute:"+step)
			if step == failOn {
				// Steps mutate their checkpoint on error paths too (resume
				// policy, pause state); the engine must adopt it.
				return c + 100, errors.New("boom at " + step)
			}
			return c + 1, nil
		},
		OnFailure: func(_ context.Context, step string, _ storage.RunRecord, c int, stepErr error) (ProcessResult, bool, error) {
			tr.events = append(tr.events, "dispatch:"+step)
			if handleFailure {
				return ProcessResult{Status: "failed", Summary: stepErr.Error()}, true, nil
			}
			return ProcessResult{}, false, nil
		},
		AfterExecuted: func(_ context.Context, step string, c int) {
			tr.events = append(tr.events, "after-executed:"+step)
		},
		AfterCompleted: func(_ context.Context, step string, c int) bool {
			tr.events = append(tr.events, "after:"+step)
			return step == stopAfter
		},
	}
}

// The ordering is the engine's whole reason to exist: persist-started
// before the started event, execution between the events, completion
// persisted before the completed event, bookkeeping last.
func TestStepRunnerOrderingContract(t *testing.T) {
	t.Parallel()
	tr := &stepTrace{}
	run, checkpoint, terminal, err := tr.runner("", false, "").Run(context.Background(), []string{"a", "b"}, storage.RunRecord{}, 0)
	if err != nil || terminal != nil {
		t.Fatalf("Run() = (%v, %v), want clean completion", terminal, err)
	}
	_ = run
	if checkpoint != 2 {
		t.Fatalf("checkpoint = %d, want advanced by both steps", checkpoint)
	}
	// after-executed sits BEFORE persist-done: bookkeeping that must
	// survive a persistence failure (lock rebinding) happens there.
	want := strings.Join([]string{
		"persist-start:a", "loop.step.started:a", "execute:a", "after-executed:a", "persist-done:a", "loop.step.completed:a", "after:a",
		"persist-start:b", "loop.step.started:b", "execute:b", "after-executed:b", "persist-done:b", "loop.step.completed:b", "after:b",
	}, "|")
	if got := strings.Join(tr.events, "|"); got != want {
		t.Fatalf("ordering =\n%s\nwant\n%s", got, want)
	}
}

func TestStepRunnerHandledFailureBecomesTerminal(t *testing.T) {
	t.Parallel()
	tr := &stepTrace{}
	_, checkpoint, terminal, err := tr.runner("b", true, "").Run(context.Background(), []string{"a", "b", "c"}, storage.RunRecord{}, 0)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if terminal == nil || terminal.Status != "failed" || !strings.Contains(terminal.Summary, "boom at b") {
		t.Fatalf("terminal = %+v, want the dispatched failure result", terminal)
	}
	if checkpoint != 101 {
		t.Fatalf("checkpoint = %d, want the failed step's returned checkpoint adopted (error-path mutations must reach the failure dispatch)", checkpoint)
	}
	joined := strings.Join(tr.events, "|")
	if strings.Contains(joined, "persist-done:b") || strings.Contains(joined, "persist-start:c") {
		t.Fatalf("failed step must not persist completion nor continue: %s", joined)
	}
}

func TestStepRunnerUnhandledFailurePropagates(t *testing.T) {
	t.Parallel()
	tr := &stepTrace{}
	_, _, terminal, err := tr.runner("a", false, "").Run(context.Background(), []string{"a"}, storage.RunRecord{}, 0)
	if terminal != nil || err == nil || !strings.Contains(err.Error(), "boom at a") {
		t.Fatalf("Run() = (%v, %v), want the raw step error propagated", terminal, err)
	}
}

func TestStepRunnerAfterCompletedStopsEarly(t *testing.T) {
	t.Parallel()
	tr := &stepTrace{}
	_, checkpoint, terminal, err := tr.runner("", false, "a").Run(context.Background(), []string{"a", "b"}, storage.RunRecord{}, 0)
	if err != nil || terminal != nil {
		t.Fatalf("Run() = (%v, %v), want clean early stop", terminal, err)
	}
	if checkpoint != 1 {
		t.Fatalf("checkpoint = %d, want only the first step applied", checkpoint)
	}
	if strings.Contains(strings.Join(tr.events, "|"), "persist-start:b") {
		t.Fatal("stop=true must prevent the next step entirely")
	}
}
