package runtime

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDetectGitHubHITLAnswer(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 100, Author: "lefarcen", Body: "<!-- looper:hitl:ask v=1 --> which one?"}, // the ask (bot marker), == askCommentID
		{ID: 101, Author: "lefarcen", Body: "<!-- looper:stamp --> still working"},     // bot marker, ignored even if same login
		{ID: 105, Author: "lefarcen", Body: "用 A,改 resize handle"},                     // human reply, no marker -> first answer
		{ID: 110, Author: "someoneelse", Body: "later comment"},
	}
	// First non-looper comment after the ask wins — even though the bot and human
	// share the "lefarcen" account, the marker distinguishes them.
	if got := detectGitHubHITLAnswer(comments, 100, nil); got != "用 A,改 resize handle" {
		t.Fatalf("answer = %q, want the first human reply", got)
	}
	// Only the ask + a marked bot note -> no answer yet.
	if got := detectGitHubHITLAnswer(comments[:2], 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty (no human reply yet)", got)
	}
	// Allowlist excludes lefarcen -> the next allowed author answers.
	if got := detectGitHubHITLAnswer(comments, 100, []string{"someoneelse"}); got != "later comment" {
		t.Fatalf("answer = %q, want the allowlisted author's comment", got)
	}
	// A looper-marked comment after the ask is never an answer.
	marked := []githubAnswerComment{{ID: 200, Author: "lefarcen", Body: "<!-- looper:decision-log --> recorded"}}
	if got := detectGitHubHITLAnswer(marked, 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty (looper's own comment)", got)
	}
}

func TestDetectGitHubHITLAnswer_RejectsBotAuthors(t *testing.T) {
	// Forgejo/GitHub default empty answerAuthors must not accept the first
	// unmarked CI/app/service-account comment after the ask as the answer.
	comments := []githubAnswerComment{
		{ID: 10, Author: "looper", Body: "<!-- looper:hitl:ask --> q"},
		{ID: 11, Author: "actions-bot", Body: "Build passed", IsBot: true},
		{ID: 12, Author: "dependabot[bot]", Body: "Bump lodash"},
		{ID: 13, Author: "human-op", Body: "keep RollingUpdate"},
	}
	if got := detectGitHubHITLAnswer(comments, 10, nil); got != "keep RollingUpdate" {
		t.Fatalf("answer = %q, want human reply after skipping bots", got)
	}
	// Only bots after the ask -> no answer yet.
	if got := detectGitHubHITLAnswer(comments[:3], 10, nil); got != "" {
		t.Fatalf("answer = %q, want empty when only bots replied", got)
	}
	// Explicit allowlist may accept a bot login (operator choice).
	if got := detectGitHubHITLAnswer(comments, 10, []string{"actions-bot"}); got != "Build passed" {
		t.Fatalf("answer = %q, want allowlisted bot comment", got)
	}
}

func TestPollGitHubHITLAnswersOnce(t *testing.T) {
	commentsByPR := map[int64][]githubAnswerComment{
		42: {{ID: 500, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}, {ID: 501, Author: "lefarcen", Body: "go with A"}},
		43: {{ID: 600, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}}, // no human reply yet
	}
	var deliveredTo []string
	var cleared []int64
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, pr int64, _ string) ([]githubAnswerComment, error) {
			return commentsByPR[pr], nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			deliveredTo = append(deliveredTo, loopID+"="+answer)
			return nil
		},
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

func TestHITLPRCommentProvider_UsesAskThenProject(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	tokenEnv := "FORGEJO_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.test", TokenEnv: &tokenEnv}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "p-fj", Name: "FJ", Provider: "fj", Repo: "acme/fj", RepoPath: t.TempDir()}}
	project := storage.ProjectRecord{ID: "p-fj"}

	if got := hitlPRCommentProvider(&cfg, project, "forgejo"); got != "forgejo" {
		t.Fatalf("explicit forgejo provider = %q", got)
	}
	if got := hitlPRCommentProvider(&cfg, project, "github"); got != "github" {
		t.Fatalf("explicit github provider = %q", got)
	}
	// Legacy asks without Provider fall back to project binding.
	if got := hitlPRCommentProvider(&cfg, project, ""); got != "forgejo" {
		t.Fatalf("legacy empty provider on forgejo project = %q, want forgejo", got)
	}
	githubProject := storage.ProjectRecord{ID: "unknown"}
	if got := hitlPRCommentProvider(&cfg, githubProject, ""); got != "github" {
		t.Fatalf("unknown project defaults to %q, want github", got)
	}
}

// Lifecycle: both poll delivery and dashboard /respond must clear the
// awaiting-human label. Poll path is covered by TestPollGitHubHITLAnswersOnce;
// this covers clearHITLAwaitingLabel's no-op/fail-soft contract used by /respond.
func TestClearHITLAwaitingLabel_NoopWithoutPR(t *testing.T) {
	if err := clearHITLAwaitingLabel(context.Background(), nil, nil, "", 0, "", ""); err != nil {
		t.Fatalf("empty repo/pr must no-op, got %v", err)
	}
	if err := clearHITLAwaitingLabel(context.Background(), nil, nil, "acme/x", 42, "", "looper:awaiting-human"); err != nil {
		// No forgejo binding and no github gateway → no-op, not hard error.
		t.Fatalf("nil gateway with no forgejo binding must no-op, got %v", err)
	}
}

func TestPollGitHubHITLAnswersOnce_ForgejoProviderLoop(t *testing.T) {
	// Ensures forgejo-labeled asks still go through the shared poll detector and
	// clearAwaiting path (provider routing is wired in runGitHubHITLPoll).
	commentsByPR := map[int64][]githubAnswerComment{
		7: {
			{ID: 10, Author: "bot", Body: "<!-- looper:hitl:ask --> q"},
			{ID: 11, Author: "human", Body: "keep RollingUpdate"},
		},
	}
	var delivered []string
	var cleared []int64
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, pr int64, _ string) ([]githubAnswerComment, error) {
			return commentsByPR[pr], nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return nil
		},
		clearAwaiting: func(_ contextType, _ string, pr int64, _ string) { cleared = append(cleared, pr) },
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		{ID: "loop-fj", Repo: "acme/fj", Transport: "github", Provider: "forgejo", AskStatus: "awaiting", PRNumber: 7, AskCommentID: 10},
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != "loop-fj=keep RollingUpdate" {
		t.Fatalf("delivered = %v n=%d, want forgejo reply observed", delivered, n)
	}
	if len(cleared) != 1 || cleared[0] != 7 {
		t.Fatalf("cleared = %v, want [7] (awaiting label cleanup after answer)", cleared)
	}
}

// Park-to-poll boundary: after durable park stamps transport+PR but before the
// ask comment id is persisted, pre-existing human issue comments must not be
// treated as the answer (AskCommentID==0 would make every id>0 eligible).
func TestPollGitHubHITLAnswersOnce_SkipsZeroAskCommentID(t *testing.T) {
	listCalls := 0
	var delivered []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			listCalls++
			return []githubAnswerComment{
				{ID: 50, Author: "human", Body: "unrelated prior comment on the PR"},
				{ID: 51, Author: "human", Body: "another pre-ask comment"},
			}, nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return nil
		},
	}
	// Incomplete park window: transport+PR published, comment not yet durable.
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		{ID: "loop-mid-delivery", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 99, AskCommentID: 0},
	}, deps)
	if n != 0 || listCalls != 0 || len(delivered) != 0 {
		t.Fatalf("zero AskCommentID must be poll-ineligible: n=%d listCalls=%d delivered=%v", n, listCalls, delivered)
	}

	// Once the ask comment id is durable, the same pre-existing comments (ids < ask)
	// still must not answer; only a reply after the ask does.
	deps.listComments = func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
		listCalls++
		return []githubAnswerComment{
			{ID: 50, Author: "human", Body: "unrelated prior comment on the PR"},
			{ID: 100, Author: "bot", Body: "<!-- looper:hitl:ask --> q"},
			{ID: 101, Author: "human", Body: "keep RollingUpdate"},
		}, nil
	}
	n = pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		{ID: "loop-ready", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 99, AskCommentID: 100},
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != "loop-ready=keep RollingUpdate" {
		t.Fatalf("after AskCommentID durable: delivered=%v n=%d, want post-ask answer only", delivered, n)
	}
}
