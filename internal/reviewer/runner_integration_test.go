package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/disclosure"
	"github.com/MumuTW/looper/internal/e2e/harness"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/reviewer/resolution"
)

func TestThreadResolutionReplyPassesStampedGitHubPublicationBoundary(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{})
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	const (
		repo     = "acme/looper"
		threadID = "MDQ6UHVsbFJlcXVlc3RSZXZpZXdUaHJlYWQxMjM0NTY="
		headSHA  = "0dd6a5019812fc422f9f20626530758ad67ad66e"
	)
	fakeGH.WriteState(t, harness.GHState{PullRequests: map[string]harness.GHPullRequest{
		repo + "#42": {Number: 42, Repo: repo, Threads: []harness.GHThread{{ID: threadID}}},
	}})

	policy := config.ReviewerThreadResolutionConfig{Mode: config.ReviewerThreadResolutionModeResolveObjective}
	body := resolution.Reply(threadID, headSHA, threadResolutionAgentDecision{
		Decision: "objectively_fixed", Evidence: "the nil check is present", Confidence: "high",
	}, policy)
	stamper := disclosure.Stamper{Config: config.DisclosureConfig{
		Enabled:  true,
		Channels: config.DisclosureChannelsConfig{ReviewComment: true},
	}}
	body = stamper.ReviewComment(body, "reviewer")

	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path})
	if err := gateway.AddReviewThreadReply(context.Background(), githubinfra.AddReviewThreadReplyInput{Repo: repo, ThreadID: threadID, Body: body}); err != nil {
		t.Fatalf("AddReviewThreadReply() error = %v", err)
	}
	if err := gateway.ResolveReviewThread(context.Background(), githubinfra.ResolveReviewThreadInput{Repo: repo, ThreadID: threadID}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}

	stateBytes, err := os.ReadFile(fakeGH.StatePath)
	if err != nil {
		t.Fatalf("ReadFile(fake gh state) error = %v", err)
	}
	var state harness.GHState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("Unmarshal(fake gh state) error = %v", err)
	}
	thread := state.PullRequests[repo+"#42"].Threads[0]
	if !thread.IsResolved {
		t.Fatal("review thread remained unresolved")
	}
	if len(thread.Comments) != 1 || thread.Comments[0].Body != body {
		t.Fatalf("thread comments = %#v, want stamped audit reply", thread.Comments)
	}
}
 (refactor(gatekeeper): retire reviewer merge authority)
