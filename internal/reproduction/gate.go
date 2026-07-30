package reproduction

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/validationcmd"
)

// Record is the persisted Reproduction Record: the Authority for "this bug is
// reproduced". It is written by Reproducer to the event log before Planner is
// reached, and it — not the worktree copy, and not any agent's claim to have
// written a test — is what Worker's and Fixer's gate is checked against.
type Record struct {
	Version         int             `json:"version"`
	ProjectID       string          `json:"projectId"`
	Repo            string          `json:"repo"`
	IssueNumber     int64           `json:"issueNumber"`
	Branch          string          `json:"branch"`
	Command         string          `json:"command"`
	Files           []FileHash      `json:"files"`
	CommitSHA       string          `json:"commitSha"`
	BaseSHA         string          `json:"baseSha,omitempty"`
	ExpectedFailure ExpectedFailure `json:"expectedFailure"`
	IdempotencyKey  string          `json:"idempotencyKey"`
	RecordedAt      string          `json:"recordedAt"`
}

// Reason is a stable, non-generic identifier for why a reproduction check
// failed. "Validation failed" is not an acceptable answer for a tampered
// reproduction: the whole point of the Role is that this specific failure is
// distinguishable from an ordinary red suite.
type Reason string

const (
	// ReasonTestMissing means a recorded reproduction file is gone.
	ReasonTestMissing Reason = "reproduction_test_missing"
	// ReasonTestModified means a recorded reproduction file's content changed.
	ReasonTestModified Reason = "reproduction_test_modified"
	// ReasonCommandFailed means the reproduction still fails: the bug is not fixed.
	ReasonCommandFailed Reason = "reproduction_command_failed"
	// ReasonCommandError means the command could not be executed at all.
	ReasonCommandError Reason = "reproduction_command_error"
	// ReasonCommandPassedOnBase means a candidate reproduction passed immediately.
	// A command that passes before any fix is not a reproduction.
	ReasonCommandPassedOnBase Reason = "reproduction_command_passed_on_base"
	// ReasonUnexpectedFailure means the command failed, but the failure did not
	// match the expected failure signature recorded for this reproduction. A
	// nonzero exit caused by a syntax error, a setup failure, or an unrelated
	// already-failing test is not proof of the reported bug, so it is rejected
	// rather than elevated into the reproduction authority.
	ReasonUnexpectedFailure Reason = "reproduction_unexpected_failure"
)

// Result is the outcome of one reproduction check.
type Result struct {
	Passed  bool
	Reason  Reason
	Summary string
	Output  string
}

// Input describes one reproduction check against a worktree. The command comes
// from the repository under test, so it runs through the same sandbox as
// repository validation commands rather than in the daemon's environment.
type Input struct {
	WorktreePath string
	Record       Record
	Timeout      time.Duration
	CodexCommand string
	Tracker      processcontainment.LiveTracker

	// Run is an in-package test seam. Production callers leave it nil so
	// repository-controlled commands stay sandboxed.
	Run func(context.Context, validationcmd.Options) (shell.Result, error)
}

// Verify is the completion half of the gate: the recorded reproduction files
// must still exist with their recorded content, and the recorded command must
// now pass. It is additional to — never a replacement for — the repository's
// own validation suite: red→green *and* no regression.
func Verify(ctx context.Context, input Input) (Result, error) {
	if result, ok := checkIntegrity(input); !ok {
		return result, nil
	}
	output, err := runCommand(ctx, input)
	if err != nil {
		var commandErr *shell.CommandExecutionError
		if errors.As(err, &commandErr) {
			// Only an ordinary nonzero exit means the bug is still failing. A
			// timeout, cancellation, or infrastructure failure is a transient
			// execution problem, not a reproduction verdict: mislabelling it as
			// "still failing" would force manual intervention for a condition
			// that retries on its own.
			if commandErr.Category == shell.FailureNonZeroExit {
				return Result{
					Reason:  ReasonCommandFailed,
					Summary: fmt.Sprintf("Reproduction still fails: %s", input.Record.Command),
					Output:  output,
				}, nil
			}
			return Result{
				Reason:  ReasonCommandError,
				Summary: fmt.Sprintf("Reproduction command did not complete: %s", input.Record.Command),
				Output:  output,
			}, nil
		}
		return Result{
			Reason:  ReasonCommandError,
			Summary: fmt.Sprintf("Reproduction command could not be run: %s", input.Record.Command),
			Output:  err.Error(),
		}, nil
	}
	return Result{Passed: true, Summary: "Reproduction passes", Output: output}, nil
}

// ProveFailing is the authoring half: a candidate reproduction is only accepted
// when the command is observed to fail on the current base. Passed reports
// whether the proof succeeded, so a command that exits zero is rejected here
// rather than being taken on the agent's word.
func ProveFailing(ctx context.Context, input Input) (Result, error) {
	if result, ok := checkIntegrity(input); !ok {
		return result, nil
	}
	output, err := runCommand(ctx, input)
	if err != nil {
		var commandErr *shell.CommandExecutionError
		if errors.As(err, &commandErr) {
			if commandErr.Category != shell.FailureNonZeroExit {
				return Result{
					Reason:  ReasonCommandError,
					Summary: fmt.Sprintf("Reproduction command did not complete: %s", input.Record.Command),
					Output:  output,
				}, nil
			}
			// A non-zero exit alone is not proof of the reported bug: a syntax
			// error, a setup failure, or an unrelated already-failing test all
			// exit non-zero, and repairing any of those would turn the command
			// green without fixing anything. The recorded signature must hold
			// against both the declared files and the observed output.
			return proveSignature(input, output), nil
		}
		return Result{
			Reason:  ReasonCommandError,
			Summary: fmt.Sprintf("Reproduction command could not be run: %s", input.Record.Command),
			Output:  err.Error(),
		}, nil
	}
	return Result{
		Reason:  ReasonCommandPassedOnBase,
		Summary: fmt.Sprintf("Candidate reproduction passed on the current base, so it does not reproduce the bug: %s", input.Record.Command),
		Output:  output,
	}, nil
}

// proveSignature decides whether an observed non-zero exit is proof of *this*
// bug rather than of some other breakage.
//
// The two checks are deliberately against different artifacts. The declared
// reproduction files are hashed into the record, so requiring the test
// identifier to appear in one of them binds the signature to content that
// cannot later change without failing the integrity check. Requiring both
// halves in the command output then proves the command actually reached that
// test and failed there.
func proveSignature(input Input, output string) Result {
	expected := input.Record.ExpectedFailure.Normalize()
	if err := expected.Validate(); err != nil {
		return Result{
			Reason:  ReasonUnexpectedFailure,
			Summary: fmt.Sprintf("Reproduction command failed with no usable expected failure signature (%v): %s", err, input.Record.Command),
			Output:  output,
		}
	}
	declared, err := expected.declaredByFiles(input.WorktreePath, input.Record.Files)
	if err != nil {
		return Result{
			Reason:  ReasonCommandError,
			Summary: fmt.Sprintf("The declared reproduction files could not be read to confirm the expected failure signature: %v", err),
			Output:  output,
		}
	}
	if !declared {
		return Result{
			Reason: ReasonUnexpectedFailure,
			Summary: fmt.Sprintf(
				"The expected failure signature names test %q, which no declared reproduction file contains: %s",
				expected.Test, input.Record.Command),
			Output: output,
		}
	}
	if !expected.matchesOutput(output) {
		return Result{
			Reason: ReasonUnexpectedFailure,
			Summary: fmt.Sprintf(
				"Reproduction command failed, but not with the expected failure of test %q: %s",
				expected.Test, input.Record.Command),
			Output: output,
		}
	}
	return Result{
		Passed:  true,
		Summary: fmt.Sprintf("Reproduction observed failing in test %q: %s", expected.Test, input.Record.Command),
		Output:  output,
	}
}

// checkIntegrity re-checks the recorded files against their recorded hashes.
// This is the mechanical half of tamper detection: a prompt telling Worker not
// to weaken the reproduction is exactly the self-policing this Role removes.
func checkIntegrity(input Input) (Result, bool) {
	for _, file := range input.Record.Files {
		absolute, err := resolveInsideWorktree(input.WorktreePath, file.Path)
		if err != nil {
			return Result{
				Reason:  ReasonTestMissing,
				Summary: fmt.Sprintf("Reproduction test %s is not readable inside the worktree: %v", file.Path, err),
			}, false
		}
		if _, statErr := os.Stat(absolute); statErr != nil {
			return Result{
				Reason:  ReasonTestMissing,
				Summary: fmt.Sprintf("Reproduction test %s was deleted after the reproduction commit; the reproduction can no longer be evaluated", file.Path),
			}, false
		}
		sum, err := hashFile(absolute)
		if err != nil {
			return Result{
				Reason:  ReasonTestMissing,
				Summary: fmt.Sprintf("Reproduction test %s could not be read: %v", file.Path, err),
			}, false
		}
		if !strings.EqualFold(sum, file.SHA256) {
			return Result{
				Reason: ReasonTestModified,
				Summary: fmt.Sprintf(
					"Reproduction test %s was modified after the reproduction commit (recorded sha256 %s, found %s); the reproduction no longer proves the reported bug",
					file.Path, shortHash(file.SHA256), shortHash(sum)),
			}, false
		}
	}
	return Result{}, true
}

func runCommand(ctx context.Context, input Input) (string, error) {
	run := input.Run
	if run == nil {
		run = validationcmd.Run
	}
	result, err := run(ctx, validationcmd.Options{
		CWD:          input.WorktreePath,
		Command:      input.Record.Command,
		Timeout:      input.Timeout,
		CodexCommand: input.CodexCommand,
		Tracker:      input.Tracker,
	})
	return combineOutput(result), err
}

func combineOutput(result shell.Result) string {
	stdout := strings.TrimSpace(result.Stdout)
	stderr := strings.TrimSpace(result.Stderr)
	switch {
	case stdout != "" && stderr != "":
		return stdout + "\n" + stderr
	case stderr != "":
		return stderr
	default:
		return stdout
	}
}

func shortHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
