package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

func TestDetectGitHubHITLAnswer(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 100, Author: "lefarcen", Body: "<!-- looper:hitl:ask v=1 --> which one?"}, // the ask (bot marker), == askCommentID
		{ID: 101, Author: "lefarcen", Body: "<!-- looper:stamp --> still working"},     // bot marker, ignored even if same login
		{ID: 105, Author: "lefarcen", Body: "用 A,改 resize handle"},                     // human reply, no marker -> first answer
		{ID: 110, Author: "someoneelse", Body: "later comment"},
	}
	// The first explicitly allowlisted non-looper comment after the ask wins —
	// even though the bot and human share the "lefarcen" account, the marker
	// distinguishes them.
	if got := detectGitHubHITLAnswer(comments, 100, []string{"lefarcen"}); got != "用 A,改 resize handle" {
		t.Fatalf("answer = %q, want the first human reply", got)
	}
	if got := detectGitHubHITLAnswer(comments, 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty without explicit or repository authority", got)
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
		answerAuthors: []string{"lefarcen"},
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

func TestPollGitHubHITLAnswersOnceEmptyAllowlistRejectsUnauthorizedComment(t *testing.T) {
	delivered := 0
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 500, Author: "looper", Body: "<!-- looper:hitl:ask --> ask"},
				{ID: 501, Author: "external-contributor", Body: "publish it"},
			}, nil
		},
		deliverAnswer: func(_ contextType, _, _ string) error {
			delivered++
			return nil
		},
		authorizeAuthor: func(_ contextType, repo, author, cwd string) (bool, error) {
			if repo != "acme/x" || author != "external-contributor" || cwd != "/tmp/repo" {
				t.Fatalf("authorizeAuthor(%q, %q, %q), want acme/x, external-contributor, /tmp/repo", repo, author, cwd)
			}
			return false, nil
		},
		projectCWD: func(string) string { return "/tmp/repo" },
	}

	got := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500,
	}}, deps)

	if got != 0 || delivered != 0 {
		t.Fatalf("delivered = (%d, %d), want unauthorized comment ignored", got, delivered)
	}
}

func TestPollGitHubHITLAnswersOnceEmptyAllowlistAcceptsRepositoryWriter(t *testing.T) {
	delivered := 0
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{{ID: 501, Author: "maintainer", Body: "publish it"}}, nil
		},
		deliverAnswer: func(_ contextType, _, _ string) error {
			delivered++
			return nil
		},
		authorizeAuthor: func(_ contextType, _, author, _ string) (bool, error) {
			return author == "maintainer", nil
		},
		projectCWD: func(string) string { return "/tmp/repo" },
	}

	got := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500,
	}}, deps)

	if got != 1 || delivered != 1 {
		t.Fatalf("delivered = (%d, %d), want repository writer accepted", got, delivered)
	}
}

func TestPollGitHubHITLAnswersOnceSkipsUnauthorizedCommentBeforeRepositoryWriter(t *testing.T) {
	var checked []string
	var deliveredAnswer string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 502, Author: "maintainer", Body: "approved answer"},
				{ID: 501, Author: "external-contributor", Body: "malicious answer"},
			}, nil
		},
		deliverAnswer: func(_ contextType, _, answer string) error {
			deliveredAnswer = answer
			return nil
		},
		authorizeAuthor: func(_ contextType, _, author, _ string) (bool, error) {
			checked = append(checked, author)
			return author == "maintainer", nil
		},
		projectCWD: func(string) string { return "/tmp/repo" },
	}

	got := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500,
	}}, deps)

	if got != 1 || deliveredAnswer != "approved answer" {
		t.Fatalf("delivered = (%d, %q), want later authorized answer", got, deliveredAnswer)
	}
	if want := []string{"external-contributor", "maintainer"}; strings.Join(checked, ",") != strings.Join(want, ",") {
		t.Fatalf("checked authors = %v, want %v in comment order", checked, want)
	}
}

func TestPollGitHubHITLAnswersOnceWhitespaceOnlyAllowlistUsesRepositoryAuthority(t *testing.T) {
	delivered := 0
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{{ID: 501, Author: "maintainer", Body: "publish it"}}, nil
		},
		deliverAnswer: func(_ contextType, _, _ string) error {
			delivered++
			return nil
		},
		authorizeAuthor: func(_ contextType, _, _, _ string) (bool, error) { return true, nil },
		projectCWD:      func(string) string { return "/tmp/repo" },
		answerAuthors:   []string{"  "},
	}

	got := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500,
	}}, deps)
	if got != 1 || delivered != 1 {
		t.Fatalf("delivered = (%d, %d), want normalized-empty allowlist to use repository authority", got, delivered)
	}
}

func TestPollGitHubHITLAnswersOncePermissionLookupFailureFailsClosed(t *testing.T) {
	delivered := 0
	var warning string
	var warningFields map[string]any
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{{ID: 501, Author: "maintainer", Body: "publish it"}}, nil
		},
		deliverAnswer: func(_ contextType, _, _ string) error {
			delivered++
			return nil
		},
		authorizeAuthor: func(_ contextType, _, _, _ string) (bool, error) {
			return false, errors.New("permission API unavailable")
		},
		projectCWD: func(string) string { return "/tmp/repo" },
		logWarn: func(msg string, fields map[string]any) {
			warning = msg
			warningFields = fields
		},
	}

	got := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500,
	}}, deps)

	if got != 0 || delivered != 0 {
		t.Fatalf("delivered = (%d, %d), want permission lookup failure to fail closed", got, delivered)
	}
	if warning != "hitl github poll: authorize answer author failed" {
		t.Fatalf("warning = %q, want permission lookup diagnostic", warning)
	}
	if got := warningFields["error"]; got != "permission API unavailable" {
		t.Fatalf("warning error = %v, want permission API unavailable", got)
	}
}

func TestRunGitHubHITLPollUsesHostScopedRepositoryPermissionAndIsReplaySafe(t *testing.T) {
	now := time.Date(2026, time.July, 30, 6, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project-hitl-authority"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "HITL authority", RepoPath: "/tmp/repo", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "Publish?", Status: "awaiting", Transport: "github", PRNumber: 42, AskCommentID: 500,
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	repo := "ghe.example.com/acme/x"
	prNumber := int64(42)
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop-hitl-authority", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "pull_request", TargetID: &repo,
		Repo: &repo, PRNumber: &prNumber, Status: "awaiting_human", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	var commentCalls, permissionCalls int
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "issues/42/comments"):
			commentCalls++
			return shell.Result{Stdout: `[[{"id":501,"body":"publish it","user":{"login":"maintainer"}}]]`}, nil
		case strings.Contains(args, "collaborators/maintainer/permission"):
			permissionCalls++
			if !strings.Contains(args, "--hostname ghe.example.com") {
				t.Fatalf("permission args = %q, want project hostname", args)
			}
			return shell.Result{Stdout: `{"permission":"write"}`}, nil
		default:
			return shell.Result{Stdout: `{}`}, nil
		}
	}})
	cfg := config.Config{HITL: config.HITLConfig{Enabled: true, AnswerTransport: "github"}}
	input := defaultSchedulerTickInput{Repos: repos, GitHubGateway: gateway, Config: &cfg, Now: func() time.Time { return now }}
	project := storage.ProjectRecord{ID: projectID, RepoPath: "/tmp/repo"}

	runGitHubHITLPoll(context.Background(), input, project)
	runGitHubHITLPoll(context.Background(), input, project)

	if commentCalls != 1 || permissionCalls != 1 {
		t.Fatalf("GitHub calls = comments:%d permission:%d, want one each across replay", commentCalls, permissionCalls)
	}
	loop, err := repos.Loops.GetByID(context.Background(), "loop-hitl-authority")
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop after authorized answer = %#v, %v, want running", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "answered" || ask.Answer != "publish it" {
		t.Fatalf("ask after authorized answer = %#v, %v", ask, ok)
	}
}

// TestPollGitHubHITLAnswersOnceDeduplicatesPermissionChecksByAuthor verifies the
// per-pass author dedup: one untrusted contributor who posts many comments before
// a maintainer answers triggers a single permission request, not one per comment.
func TestPollGitHubHITLAnswersOnceDeduplicatesPermissionChecksByAuthor(t *testing.T) {
	var checked []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 500, Author: "looper", Body: "<!-- looper:hitl:ask --> ask"},
				{ID: 501, Author: "spammer", Body: "do x"},
				{ID: 502, Author: "spammer", Body: "do y"},
				{ID: 503, Author: "spammer", Body: "do z"},
				{ID: 504, Author: "maintainer", Body: "approved"},
			}, nil
		},
		deliverAnswer: func(_ contextType, _, _ string) error { return nil },
		authorizeAuthor: func(_ contextType, _, author, _ string) (bool, error) {
			checked = append(checked, author)
			return author == "maintainer", nil
		},
		projectCWD: func(string) string { return "/tmp/repo" },
	}

	got := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500,
	}}, deps)

	if got != 1 {
		t.Fatalf("delivered = %d, want 1 (maintainer answer delivered)", got)
	}
	if want := []string{"spammer", "maintainer"}; strings.Join(checked, ",") != strings.Join(want, ",") {
		t.Fatalf("checked authors = %v, want %v (spammer deduplicated to one check)", checked, want)
	}
}
