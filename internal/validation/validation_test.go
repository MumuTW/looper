package validation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/validationcmd"
)

func localValidationRunner(ctx context.Context, options validationcmd.Options) (shell.Result, error) {
	return shell.Run(ctx, shell.Options{
		Command: "/bin/sh",
		Args:    []string{"-c", options.Command},
		CWD:     options.CWD,
		Timeout: options.Timeout,
		Tracker: options.Tracker,
	})
}

func TestRunCommandsKeepsPolicyWordsDiagnosticForNonZeroExit(t *testing.T) {
	t.Parallel()

	result, err := RunCommands(context.Background(), Input{
		Commands: []string{`printf 'TestTimeoutPolicy: head changed'; printf 'connection refused' >&2; exit 1`},
	}, &Options{CWD: t.TempDir(), runValidation: localValidationRunner})
	if err != nil {
		t.Fatalf("RunCommands() error = %v", err)
	}
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if result.FailureCategory != FailureNonZeroExit {
		t.Fatalf("FailureCategory = %q, want %q", result.FailureCategory, FailureNonZeroExit)
	}
	for _, diagnostic := range []string{"TestTimeoutPolicy", "head changed", "connection refused"} {
		if !strings.Contains(result.Output, diagnostic) {
			t.Fatalf("Output = %q, want diagnostic %q", result.Output, diagnostic)
		}
	}

	policy := PolicyFor(result.FailureCategory)
	if policy.FailureKind != FailureKindManualIntervention || policy.ResumePolicy != loops.ResumePolicyManualIntervention {
		t.Fatalf("PolicyFor() = %#v, want deterministic manual policy", policy)
	}
}

func TestRunCommandsDistinguishesSupervisorTimeoutFromContextDeadline(t *testing.T) {
	t.Parallel()

	timeoutResult, err := RunCommands(context.Background(), Input{
		Commands:       []string{`sleep 1`},
		CommandTimeout: 20 * time.Millisecond,
	}, &Options{CWD: t.TempDir(), runValidation: localValidationRunner})
	if err != nil {
		t.Fatalf("RunCommands(timeout) error = %v", err)
	}
	if timeoutResult.FailureCategory != FailureSupervisorTimeout {
		t.Fatalf("timeout FailureCategory = %q, want %q", timeoutResult.FailureCategory, FailureSupervisorTimeout)
	}
	if timeoutResult.Summary != "Validation timed out: sleep 1" {
		t.Fatalf("timeout Summary = %q, want timeout-specific summary", timeoutResult.Summary)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	contextResult, err := RunCommands(ctx, Input{Commands: []string{`sleep 1`}}, &Options{CWD: t.TempDir(), runValidation: localValidationRunner})
	if err != nil {
		t.Fatalf("RunCommands(context deadline) error = %v", err)
	}
	if contextResult.FailureCategory != FailureContextCanceled {
		t.Fatalf("context FailureCategory = %q, want %q", contextResult.FailureCategory, FailureContextCanceled)
	}
}

func TestRunCommandsPreservesDiagnosticsWhenCommandProducesNoOutput(t *testing.T) {
	t.Parallel()

	result, err := RunCommands(context.Background(), Input{Commands: []string{"go test ./..."}}, &Options{
		CWD:          t.TempDir(),
		CodexCommand: "/path/that/does/not/exist/codex",
	})
	if err != nil {
		t.Fatalf("RunCommands() error = %v", err)
	}
	if result.Passed || result.FailureCategory != FailureInfrastructure {
		t.Fatalf("RunCommands() = %#v, want infrastructure failure", result)
	}
	if !strings.Contains(result.Output, "no such file or directory") {
		t.Fatalf("Output = %q, want missing executable diagnostic", result.Output)
	}
}

func TestRunCommandsFailsClosedWithoutSandboxConfiguration(t *testing.T) {
	t.Parallel()

	result, err := RunCommands(context.Background(), Input{Commands: []string{"printf exposed"}}, nil)
	if err != nil {
		t.Fatalf("RunCommands() error = %v", err)
	}
	if result.Passed || result.FailureCategory != FailureInfrastructure {
		t.Fatalf("RunCommands() = %#v, want fail-closed infrastructure result", result)
	}
	if !strings.Contains(result.Output, "cwd is required") {
		t.Fatalf("Output = %q, want sandbox configuration diagnostic", result.Output)
	}
}

func TestPolicyForOperationalFailureCategory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		category FailureCategory
		failure  string
		resume   string
	}{
		{name: "context cancellation remains resumable", category: FailureContextCanceled, failure: FailureKindRetryableAfterResume, resume: loops.ResumePolicyReplayStep},
		{name: "supervisor timeout retries", category: FailureSupervisorTimeout, failure: FailureKindRetryableTransient, resume: loops.ResumePolicyReplayStep},
		{name: "infrastructure retries", category: FailureInfrastructure, failure: FailureKindRetryableTransient, resume: loops.ResumePolicyReplayStep},
		{name: "ordinary exit is manual", category: FailureNonZeroExit, failure: FailureKindManualIntervention, resume: loops.ResumePolicyManualIntervention},
		{name: "missing category fails closed", category: "", failure: FailureKindManualIntervention, resume: loops.ResumePolicyManualIntervention},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := PolicyFor(test.category)
			if policy.FailureKind != test.failure || policy.ResumePolicy != test.resume {
				t.Fatalf("PolicyFor(%q) = %#v, want failure=%q resume=%q", test.category, policy, test.failure, test.resume)
			}
		})
	}
}
