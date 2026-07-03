package runtime

import (
	"context"
	"testing"
)

func TestDetectGitHubHITLAnswer(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 100, Author: "looper-bot", Body: "<!-- looper:hitl:ask --> which one?"}, // the ask (== askCommentID)
		{ID: 101, Author: "looper-bot", Body: "some other bot note"},                  // bot, ignored
		{ID: 105, Author: "lefarcen", Body: "用 A,改 resize handle"},                    // first human answer
		{ID: 110, Author: "someoneelse", Body: "later comment"},                        // later, not chosen
	}
	// First non-bot comment after the ask wins.
	if got := detectGitHubHITLAnswer(comments, 100, "looper-bot", nil); got != "用 A,改 resize handle" {
		t.Fatalf("answer = %q, want the first human reply", got)
	}
	// No qualifying comment yet (only the ask + bot notes).
	if got := detectGitHubHITLAnswer(comments[:2], 100, "looper-bot", nil); got != "" {
		t.Fatalf("answer = %q, want empty (no human reply yet)", got)
	}
	// Allowlist excludes lefarcen -> the next allowed author (someoneelse) answers.
	if got := detectGitHubHITLAnswer(comments, 100, "looper-bot", []string{"someoneelse"}); got != "later comment" {
		t.Fatalf("answer = %q, want the allowlisted author's comment", got)
	}
	// The bot's own reply is never an answer even if after the ask.
	self := []githubAnswerComment{{ID: 200, Author: "looper-bot", Body: "still working"}}
	if got := detectGitHubHITLAnswer(self, 100, "looper-bot", nil); got != "" {
		t.Fatalf("answer = %q, want empty (bot's own comment)", got)
	}
}

func TestPollGitHubHITLAnswersOnce(t *testing.T) {
	commentsByPR := map[int64][]githubAnswerComment{
		42: {{ID: 500, Author: "bot", Body: "ask"}, {ID: 501, Author: "human", Body: "go with A"}},
		43: {{ID: 600, Author: "bot", Body: "ask"}}, // no human reply yet
	}
	var deliveredTo []string
	var cleared []int64
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, pr int64, _ string) ([]githubAnswerComment, error) {
			return commentsByPR[pr], nil
		},
		currentUser:   func(_ contextType, _ string) string { return "bot" },
		deliverAnswer: func(_ contextType, loopID, answer string) error { deliveredTo = append(deliveredTo, loopID+"="+answer); return nil },
		clearAwaiting: func(_ contextType, _ string, pr int64, _ string) { cleared = append(cleared, pr) },
		projectCWD:    func(string) string { return "/tmp/repo" },
	}
	loops := []githubHITLAwaitingLoop{
		{ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500},
		{ID: "loop-b", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 43, AskCommentID: 600},
		{ID: "loop-c", Repo: "acme/x", Transport: "feishu", PRNumber: 44}, // non-github, skipped
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), loops, deps)
	if n != 1 {
		t.Fatalf("delivered = %d, want 1", n)
	}
	if len(deliveredTo) != 1 || deliveredTo[0] != "loop-a=go with A" {
		t.Fatalf("deliveredTo = %v, want [loop-a=go with A]", deliveredTo)
	}
	if len(cleared) != 1 || cleared[0] != 42 {
		t.Fatalf("cleared = %v, want [42]", cleared)
	}
}
