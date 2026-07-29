package validation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/validationcmd"
)

type FailureCategory string

const (
	FailureNonZeroExit       FailureCategory = "non_zero_exit"
	FailureSupervisorTimeout FailureCategory = "supervisor_timeout"
	FailureContextCanceled   FailureCategory = "context_canceled"
	FailureInfrastructure    FailureCategory = "infrastructure"
)

const (
	FailureKindManualIntervention   = loops.FailureKindManualIntervention
	FailureKindRetryableAfterResume = loops.FailureKindRetryableAfterResume
	FailureKindRetryableTransient   = "retryable_transient"
)

type Policy struct {
	FailureKind  string
	ResumePolicy string
}

type Input struct {
	Commands       []string
	CommandTimeout time.Duration
}

type Options struct {
	CWD          string
	CodexCommand string
	Tracker      processcontainment.LiveTracker
	// runValidation is an in-package test seam. Production callers always use
	// validationcmd.Run so repository-controlled commands remain sandboxed.
	runValidation func(context.Context, validationcmd.Options) (shell.Result, error)
}

type Result struct {
	Passed          bool
	Summary         string
	Output          string
	FailureCategory FailureCategory
}

func RunCommands(ctx context.Context, input Input, options *Options) (Result, error) {
	if options == nil {
		options = &Options{}
	}
	if len(input.Commands) == 0 {
		return Result{Passed: true, Summary: "No validation commands configured"}, nil
	}

	outputs := make([]string, 0, len(input.Commands)*2)
	for _, command := range input.Commands {
		runValidation := options.runValidation
		if runValidation == nil {
			runValidation = validationcmd.Run
		}
		shellResult, err := runValidation(ctx, validationcmd.Options{
			CWD:          options.CWD,
			Command:      command,
			Timeout:      input.CommandTimeout,
			CodexCommand: options.CodexCommand,
			Tracker:      options.Tracker,
		})

		output := strings.TrimSpace(shellResult.Stdout)
		if shellResult.Stderr != "" {
			stderr := strings.TrimSpace(shellResult.Stderr)
			if output != "" {
				output += "\n" + stderr
			} else {
				output = stderr
			}
		}

		if err != nil {
			var commandErr *shell.CommandExecutionError
			if errors.As(err, &commandErr) {
				if output == "" {
					output = commandErr.Error()
				}
				summary := "Validation failed: " + command
				if commandErr.Category == shell.FailureSupervisorTimeout {
					summary = "Validation timed out: " + command
				}
				return Result{
					Passed:          false,
					Summary:         summary,
					Output:          output,
					FailureCategory: FailureCategory(commandErr.Category),
				}, nil
			}
			return Result{
				Passed:          false,
				Summary:         "Validation failed: " + command,
				Output:          err.Error(),
				FailureCategory: FailureInfrastructure,
			}, nil
		}

		if output != "" {
			outputs = append(outputs, output)
		}
	}

	return Result{Passed: true, Summary: "Validation passed", Output: strings.Join(outputs, "\n")}, nil
}

func PolicyFor(category FailureCategory) Policy {
	switch category {
	case FailureContextCanceled:
		return Policy{FailureKind: FailureKindRetryableAfterResume, ResumePolicy: loops.ResumePolicyReplayStep}
	case FailureSupervisorTimeout, FailureInfrastructure:
		return Policy{FailureKind: FailureKindRetryableTransient, ResumePolicy: loops.ResumePolicyReplayStep}
	case FailureNonZeroExit:
		return Policy{FailureKind: FailureKindManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention}
	default:
		return Policy{FailureKind: FailureKindManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention}
	}
}
