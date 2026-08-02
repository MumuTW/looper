package github

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/infra/shell"
)

// TestListPullRequestReviewsCarriesForgeBotClassification pins the reason this
// reads REST: GitHub answers `user.type`, and a login test cannot substitute for
// it. The same CodeRabbit account is `coderabbitai[bot]` here and plain
// `coderabbitai` in the GraphQL projection `gh pr view --json reviews` returns,
// so bot-ness has to come from the forge rather than from the spelling.
func TestListPullRequestReviewsCarriesForgeBotClassification(t *testing.T) {
	t.Parallel()
	gateway := New(Options{GHPath: "gh", GHRun: func(_ context.Context, _ shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: `[
			{"id":1,"state":"COMMENTED","body":"nit","user":{"login":"coderabbitai[bot]","type":"Bot"}},
			{"id":2,"state":"APPROVED","body":"lgtm","user":{"login":"octocat","type":"User"}}
		]`}, nil
	}})

	reviews, err := gateway.ListPullRequestReviews(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("ListPullRequestReviews() error = %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("reviews = %#v, want two", reviews)
	}
	if reviews[0].Author != "coderabbitai[bot]" || !reviews[0].IsBot {
		t.Fatalf("review[0] = %#v, want the bot account classified as a bot", reviews[0])
	}
	if reviews[1].Author != "octocat" || reviews[1].IsBot {
		t.Fatalf("review[1] = %#v, want the human account classified as a human", reviews[1])
	}
}

// TestListIssueCommentsContainingProjectsBeforeCapture is the size defect: a
// pull request whose full comment JSON exceeds the shell capture cap makes the
// unprojected reader fail, and a refusal that was present is then recorded as
// nothing at all. The projection has to happen inside gh, before the bounded
// buffer, and it has to keep the account type the detector needs.
func TestListIssueCommentsContainingProjectsBeforeCapture(t *testing.T) {
	t.Parallel()
	var args []string
	gateway := New(Options{GHPath: "gh", GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		args = options.Args
		return shell.Result{Stdout: `{"id":77,"body":"Review limit reached","user":{"login":"coderabbitai[bot]","type":"Bot"}}`}, nil
	}})

	comments, err := gateway.ListIssueCommentsContaining(context.Background(),
		ViewIssueInput{Repo: "acme/looper", IssueNumber: 42}, []string{`Review limit reached`, `He said "no"`})
	if err != nil {
		t.Fatalf("ListIssueCommentsContaining() error = %v", err)
	}
	if len(comments) != 1 || comments[0].ID != 77 || comments[0].Author != "coderabbitai[bot]" || !comments[0].IsBot {
		t.Fatalf("comments = %#v, want the refusal with its account classified", comments)
	}

	filter := ""
	for i, arg := range args {
		if arg == "--jq" && i+1 < len(args) {
			filter = args[i+1]
		}
	}
	if filter == "" {
		t.Fatalf("gh args = %q, want a --jq projection applied before the capture buffer", args)
	}
	for _, want := range []string{
		`contains("Review limit reached")`,
		// Encoded, not concatenated: a marker carrying a quote must not be able to
		// rewrite the filter it is embedded in.
		`contains("He said \"no\"")`,
		`user:{login:.user.login,type:.user.type}`,
	} {
		if !strings.Contains(filter, want) {
			t.Fatalf("jq filter = %q, want it to contain %q", filter, want)
		}
	}
	if strings.Contains(filter, "--slurp") || slices.Contains(args, "--slurp") {
		t.Fatalf("gh args = %q, want pages streamed rather than slurped whole", args)
	}
}
