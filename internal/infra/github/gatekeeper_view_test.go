package github

import (
	"context"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/infra/shell"
)

func TestViewPullRequestForGatekeeperRequestsChangedFilesForDiffBudget(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if !strings.HasPrefix(args, "pr view 42 --repo acme/looper --json ") {
			t.Fatalf("unexpected gh args: %q", args)
		}
		fields := strings.TrimPrefix(args, "pr view 42 --repo acme/looper --json ")
		for _, field := range []string{"additions", "deletions", "changedFiles"} {
			if !strings.Contains(fields, field) {
				t.Fatalf("gatekeeper fields = %q, want %s", fields, field)
			}
		}
		return shell.Result{Stdout: `{"number":42,"state":"OPEN","headRefOid":"head-1","baseRefName":"main","baseRefOid":"base-1","additions":120,"deletions":30,"changedFiles":7}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	detail, err := gateway.ViewPullRequestForGatekeeper(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("ViewPullRequestForGatekeeper() error = %v", err)
	}
	if detail.DiffStats == nil || detail.DiffStats.ChangedFiles != 7 || detail.DiffStats.Deletions != 30 {
		t.Fatalf("DiffStats = %#v, want changedFiles=7 deletions=30", detail.DiffStats)
	}
}
