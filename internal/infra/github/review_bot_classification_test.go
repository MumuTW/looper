package github

import (
	"context"
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
