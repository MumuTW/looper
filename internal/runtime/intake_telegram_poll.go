package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/intake/telegram"
	"github.com/nexu-io/looper/internal/outboundguard"
)

// telegramIntakeHTTPTimeout bounds a Bot API call.
const telegramIntakeHTTPTimeout = 30 * time.Second

// telegramIntakeOffset is the next update id to fetch. It is in memory on
// purpose: it is an optimization, not an authority. Telegram redelivers anything
// unacknowledged, and the duplicate check in createIntakeIssue is what actually
// prevents a second Issue, so losing this on restart costs one wasted lookup per
// replayed update and nothing else.
var telegramIntakeOffset struct {
	mu sync.Mutex
	v  int64
}

// runTelegramIntakePoll drains one batch of Telegram updates, opening an Issue
// for each new request.
//
// Intake stops at the Issue. Whether that Issue is specific enough, and which
// Role should pick it up, stays with Triager — this lane makes no routing or
// classification decision.
//
// The lane runs inside the serial scheduler tick, so getUpdates does not
// long-poll: waiting on Telegram would hold every discovery lane hostage for the
// duration of the wait. A message is therefore picked up within one scheduler
// poll interval.
func runTelegramIntakePoll(ctx context.Context, input defaultSchedulerTickInput) {
	if input.Config == nil || input.Repos == nil {
		return
	}
	cfg := input.Config.Intake.Telegram
	if cfg == nil || !cfg.Enabled {
		return
	}
	if input.GitHubGateway == nil {
		if input.Logger != nil {
			input.Logger.Warn("telegram intake: no github gateway; intake cannot open issues", nil)
		}
		return
	}
	token := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.BotTokenEnv)))
	if token == "" {
		if input.Logger != nil {
			input.Logger.Warn("telegram intake: bot token env is empty", map[string]any{"env": cfg.BotTokenEnv})
		}
		return
	}

	telegramIntakeOffset.mu.Lock()
	offset := telegramIntakeOffset.v
	telegramIntakeOffset.mu.Unlock()

	client := telegram.NewClient(token, telegramIntakeHTTPTimeout)
	updates, err := client.GetUpdates(ctx, offset)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("telegram intake: getUpdates failed", map[string]any{"error": err.Error()})
		}
		return
	}
	if len(updates) == 0 {
		return
	}

	deps := telegram.Deps{
		DefaultProjectID: strings.TrimSpace(cfg.DefaultProjectID),
		AllowedUserIDs:   append([]int64(nil), cfg.AllowedUserIDs...),
		ResolveProject: func(ctx context.Context, projectID string) (telegram.Target, error) {
			return resolveIntakeTarget(ctx, input, projectID)
		},
		FindIssueBySourceStamp: func(ctx context.Context, target telegram.Target, stamp string) (int64, error) {
			return input.GitHubGateway.FindIssueBySourceStamp(ctx, target.Repo, stamp, target.RepoPath)
		},
		CreateIssue: func(ctx context.Context, target telegram.Target, title, body string) (telegram.IssueRef, error) {
			return createIntakeIssue(ctx, input, target, title, body)
		},
		Reply: func(ctx context.Context, chatID, text string) error {
			return client.SendMessage(ctx, chatID, text)
		},
	}
	if input.Logger != nil {
		deps.LogWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
		deps.LogDebug = func(msg string, fields map[string]any) { input.Logger.Debug(msg, fields) }
	}

	result := telegram.Dispatch(ctx, updates, deps)
	if result.AckedUpdateID > 0 {
		telegramIntakeOffset.mu.Lock()
		if next := result.AckedUpdateID + 1; next > telegramIntakeOffset.v {
			telegramIntakeOffset.v = next
		}
		telegramIntakeOffset.mu.Unlock()
	}
	if (result.IssuesOpened > 0 || result.Rejected > 0 || result.Duplicates > 0) && input.Logger != nil {
		input.Logger.Info("telegram intake: processed updates", map[string]any{
			"issuesOpened": result.IssuesOpened, "duplicates": result.Duplicates, "rejected": result.Rejected,
		})
	}
}

// resolveIntakeTarget reports where a project's Issues live, or why intake
// cannot use it. A storage failure is returned as an error so the request is
// retried; only a genuinely unusable project is reported as unroutable, because
// that answer is told to the sender and consumes their message.
func resolveIntakeTarget(ctx context.Context, input defaultSchedulerTickInput, projectID string) (telegram.Target, error) {
	project, err := input.Repos.Projects.GetByID(ctx, projectID)
	if err != nil {
		return telegram.Target{}, err
	}
	if project == nil {
		return telegram.Target{Unroutable: "沒有這個 project"}, nil
	}
	if project.Archived {
		return telegram.Target{Unroutable: "project 已封存"}, nil
	}
	repo, inCatalog := schedulerProjectRepo(input, *project)
	if !inCatalog {
		return telegram.Target{Unroutable: "project 不在目前的設定檔裡"}, nil
	}
	if strings.TrimSpace(repo) == "" {
		return telegram.Target{Unroutable: "project 沒有繫結 repo"}, nil
	}
	return telegram.Target{ProjectID: project.ID, Repo: repo, RepoPath: project.RepoPath}, nil
}

func createIntakeIssue(ctx context.Context, input defaultSchedulerTickInput, target telegram.Target, title, body string) (telegram.IssueRef, error) {
	created, err := input.GitHubGateway.CreateIssue(ctx, githubinfra.CreateIssueInput{
		Repo: target.Repo, Title: title, Body: body, CWD: target.RepoPath,
	})
	if err != nil {
		return telegram.IssueRef{}, classifyIntakeCreateError(err)
	}
	return telegram.IssueRef{Number: created.Number, URL: created.URL}, nil
}

// classifyIntakeCreateError marks an outbound-guard rejection as permanent. The
// guard rejects content it will always reject — a credential in the message, a
// private key — so retrying that message forever would wedge the lane, and every
// later message behind it, on one bad request.
func classifyIntakeCreateError(err error) error {
	var rejection *outboundguard.Rejection
	if errors.As(err, &rejection) {
		return telegram.Permanent(fmt.Errorf("message cannot be posted to the forge: %w", err))
	}
	return err
}
