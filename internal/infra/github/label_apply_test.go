package github

import (
	"context"
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
			return shell.Result{Stdout: `[{"name":"looper:hold","color":"b60205","description":"Human veto, hand-worded"}]`}, nil
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
