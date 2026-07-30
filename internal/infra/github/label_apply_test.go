package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/labels"
)

// Applying a label used to create it with `gh label create --force`, which
// updates an existing label in place. Every time Looper applied looper:hold to
// an issue it therefore rewrote that label's color and description in the
// repository from its own table — silently replacing wording a maintainer had
// chosen in the forge UI. Looper needs the label to exist; it has no claim on
// how an existing one reads.
func TestApplyingLabelsNeverRewritesAnExistingLabel(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "label list --repo acme/looper --limit 1000 --json name,color,description":
			// looper:hold exists with wording that differs from labels.Standard.
			return shell.Result{Stdout: fmt.Sprintf(`[{"name":%q,"color":"b60205","description":"Human veto, hand-worded"}]`, labels.HoldGlobal)}, nil
		case strings.HasPrefix(args, "label create looper:plan "):
			return shell.Result{Stdout: "{}"}, nil
		case strings.HasPrefix(args, "api repos/acme/looper/issues/7/labels"):
			return shell.Result{Stdout: "[]"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.AddIssueLabels(context.Background(), IssueLabelsInput{
		Repo:        "acme/looper",
		IssueNumber: 7,
		Labels:      []string{labels.HoldGlobal, labels.DefaultPlanTrigger},
	})
	if err != nil {
		t.Fatalf("AddIssueLabels() error = %v", err)
	}

	log := strings.Join(runner.calls, "\n")
	if strings.Contains(log, "--force") {
		t.Errorf("gh was called with --force, which rewrites existing labels:\n%s", log)
	}
	if strings.Contains(log, "label create "+labels.HoldGlobal) {
		t.Errorf("existing label %s was recreated:\n%s", labels.HoldGlobal, log)
	}
	if !strings.Contains(log, "label create "+labels.DefaultPlanTrigger) {
		t.Errorf("missing label %s was not created:\n%s", labels.DefaultPlanTrigger, log)
	}
}

// Two daemon actions can concurrently target the same missing label: both list
// before either creates it, the first create succeeds, and the second no-force
// `gh label create` then fails with "already exists". The label is present,
// which is all ensureLabelsExist needs, so that one outcome must be tolerated
// and the issue-label POST must still run.
func TestApplyingLabelsToleratesLabelCreatedAfterList(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "label list --repo acme/looper --limit 1000 --json name,color,description":
			// The list ran before the race created the label, so it is missing.
			return shell.Result{Stdout: `[]`}, nil
		case strings.HasPrefix(args, "label create "+labels.DefaultPlanTrigger+" "):
			// Another action created it between our list and our create.
			return shell.Result{Stderr: "HTTP 422: Label already exists"}, &shell.CommandExecutionError{Message: "Command exited with code 1: HTTP 422: Label already exists", Category: shell.FailureNonZeroExit}
		case strings.HasPrefix(args, "api repos/acme/looper/issues/7/labels"):
			return shell.Result{Stdout: "[]"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.AddIssueLabels(context.Background(), IssueLabelsInput{
		Repo:        "acme/looper",
		IssueNumber: 7,
		Labels:      []string{labels.DefaultPlanTrigger},
	})
	if err != nil {
		t.Fatalf("AddIssueLabels() error = %v, want the duplicate-create race tolerated", err)
	}

	log := strings.Join(runner.calls, "\n")
	if !strings.Contains(log, "api repos/acme/looper/issues/7/labels") {
		t.Errorf("issue-label POST did not run after the tolerated create race:\n%s", log)
	}
}

// A create failure that is NOT the duplicate-label race must still surface, so
// the tolerance does not mask real errors (permissions, network, etc.).
func TestApplyingLabelsSurfacesNonDuplicateCreateFailure(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "label list --repo acme/looper --limit 1000 --json name,color,description":
			return shell.Result{Stdout: `[]`}, nil
		case strings.HasPrefix(args, "label create "+labels.DefaultPlanTrigger+" "):
			return shell.Result{Stderr: "HTTP 403: Resource not accessible by integration"}, &shell.CommandExecutionError{Message: "Command exited with code 1: HTTP 403: Resource not accessible by integration", Category: shell.FailureNonZeroExit}
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.AddIssueLabels(context.Background(), IssueLabelsInput{
		Repo:        "acme/looper",
		IssueNumber: 7,
		Labels:      []string{labels.DefaultPlanTrigger},
	})
	if err == nil {
		t.Fatal("AddIssueLabels() error = nil, want the non-duplicate create failure to surface")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("AddIssueLabels() error = %v, want the original 403 failure propagated", err)
	}
}

// The requested labels are the authority: a caller asking for a label has
// already decided it belongs. Listing is drift detection — it only skips
// creates that would fail anyway — so a transient list failure must not block
// the mutation the caller asked for. Before the read-before-write change there
// was no list to fail; turning it into a gate would have been a new way for
// label application to stop working.
func TestApplyingLabelsSurvivesAFailedLabelList(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.HasPrefix(args, "label list "):
			return shell.Result{}, errors.New("HTTP 502: Bad Gateway")
		case strings.HasPrefix(args, "label create "+labels.DefaultPlanTrigger):
			return shell.Result{Stdout: "{}"}, nil
		case strings.HasPrefix(args, "api repos/acme/looper/issues/11/labels"):
			return shell.Result{Stdout: "[]"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.AddIssueLabels(context.Background(), IssueLabelsInput{
		Repo:        "acme/looper",
		IssueNumber: 11,
		Labels:      []string{labels.DefaultPlanTrigger},
	}); err != nil {
		t.Fatalf("AddIssueLabels() error = %v, want a failed list to be survivable", err)
	}

	log := strings.Join(runner.calls, "\n")
	if !strings.Contains(log, "api repos/acme/looper/issues/11/labels") {
		t.Errorf("a failed label list blocked the label application:\n%s", log)
	}
	if strings.Contains(log, "--force") {
		t.Errorf("recovered with --force, which is the rewrite this path avoids:\n%s", log)
	}
}

// Once a create fails, the caller's action will not happen: ensureLabelsExist
// returns the error and the issue-label mutation is never sent. Creating the
// remaining labels anyway would leave the repository changed for an action
// that did not take place.
func TestApplyingLabelsStopsCreatingAfterAFailure(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.HasPrefix(args, "label list "):
			return shell.Result{Stdout: "[]"}, nil
		case strings.HasPrefix(args, "label create "+labels.DefaultPlanTrigger+" "):
			return shell.Result{}, &shell.CommandExecutionError{Message: "HTTP 403: Resource not accessible by integration", Category: shell.FailureNonZeroExit}
		default:
			t.Fatalf("unexpected gh args after the failing create: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.AddIssueLabels(context.Background(), IssueLabelsInput{
		Repo:        "acme/looper",
		IssueNumber: 12,
		Labels:      []string{labels.DefaultPlanTrigger, labels.HoldGlobal},
	})
	if err == nil {
		t.Fatal("AddIssueLabels() error = nil, want the create failure to surface")
	}
	if strings.Contains(strings.Join(runner.calls, "\n"), "label create "+labels.HoldGlobal) {
		t.Errorf("kept creating labels after the action was known to fail:\n%s", strings.Join(runner.calls, "\n"))
	}
}

// A dry run's whole output is the plan, and no create follows to correct a
// wrong guess about what already exists. A failed listing therefore has to be
// reported rather than reported as "everything will be created".
func TestInitializeLabelsDryRunRequiresAListing(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.HasPrefix(strings.Join(options.Args, " "), "label list ") {
			return shell.Result{}, &shell.CommandExecutionError{Message: "HTTP 502: Bad Gateway", Category: shell.FailureNonZeroExit}
		}
		t.Fatalf("a dry run must not mutate: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	result, err := gateway.InitializeLabels(context.Background(), InitializeLabelsInput{Repo: "acme/looper", DryRun: true})
	if err == nil {
		t.Fatalf("InitializeLabels(dry run) error = nil, want the failed listing reported; got plan %#v", result.Summary)
	}
}
