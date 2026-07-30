package runtime

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/intake/telegram"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// telegramIntakeHTTPTimeout bounds a Bot API call.
const telegramIntakeHTTPTimeout = 30 * time.Second

// telegramPollTimeoutSeconds is the getUpdates long-poll window, deliberately 0.
// This lane runs inside the scheduler tick, which is serial: a long poll would
// hold every discovery lane hostage for its full duration on every tick. With 0,
// Telegram returns pending updates immediately and returns empty immediately when
// there are none, so the tick cost is one round trip and intake latency is at
// most one scheduler poll interval.
const telegramPollTimeoutSeconds = 0

// runTelegramIntakePoll drains one batch of Telegram updates: a plain message
// opens an Issue in the resolved project, and a reply to a loop's ask hands the
// text to that loop. Gated by intake.telegram.enabled; a no-op otherwise.
//
// Intake stops at the Issue. Whether that Issue is specific enough, and which
// Role should pick it up, stays with Triager — this lane makes no routing or
// classification decision.
func runTelegramIntakePoll(ctx context.Context, input defaultSchedulerTickInput) {
	if input.Config == nil || input.Repos == nil || input.GitHubGateway == nil {
		return
	}
	cfg := input.Config.Intake.Telegram
	if cfg == nil || !cfg.Enabled {
		return
	}
	token := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.BotTokenEnv)))
	if token == "" {
		if input.Logger != nil {
			input.Logger.Warn("telegram intake: bot token env is empty", map[string]any{"env": cfg.BotTokenEnv})
		}
		return
	}
	if input.Repos.TelegramIntake == nil || input.Repos.TelegramThreads == nil {
		return
	}

	lastUpdateID, err := input.Repos.TelegramIntake.LastUpdateID(ctx)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("telegram intake: read cursor failed", map[string]any{"error": err.Error()})
		}
		return
	}

	client := telegram.NewClient(token, telegramIntakeHTTPTimeout)
	updates, err := client.GetUpdates(ctx, lastUpdateID+1, telegramPollTimeoutSeconds)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("telegram intake: getUpdates failed", map[string]any{"error": err.Error()})
		}
		return
	}
	if len(updates) == 0 {
		return
	}

	nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
	deps := telegram.Deps{
		DefaultProjectID: strings.TrimSpace(cfg.DefaultProjectID),
		AllowedUserIDs:   append([]int64(nil), cfg.AllowedUserIDs...),
		KnownProject: func(projectID string) bool {
			project, err := input.Repos.Projects.GetByID(ctx, projectID)
			return err == nil && project != nil && !project.Archived
		},
		LoopByReply: func(ctx context.Context, chatID, messageID string) string {
			loopID, err := input.Repos.TelegramThreads.LoopByMessage(ctx, chatID, messageID)
			if err != nil {
				return ""
			}
			return loopID
		},
		EnqueueMessage: func(ctx context.Context, loopID, text string) error {
			return enqueueHumanMessageToLoop(ctx, input.Repos, nowISO, loopID, text)
		},
		CreateIssue: func(ctx context.Context, projectID, title, body string) (telegram.IssueRef, error) {
			return createIntakeIssue(ctx, input, projectID, title, body)
		},
		Reply: func(ctx context.Context, chatID string, replyTo int64, text string) error {
			_, err := client.SendMessage(ctx, chatID, text, replyTo)
			return err
		},
	}
	if input.Logger != nil {
		deps.LogWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
		deps.LogDebug = func(msg string, fields map[string]any) { input.Logger.Debug(msg, fields) }
	}

	result := telegram.Dispatch(ctx, updates, deps)
	if result.MaxUpdateID > lastUpdateID {
		if err := input.Repos.TelegramIntake.AdvanceUpdateID(ctx, result.MaxUpdateID, nowISO); err != nil && input.Logger != nil {
			// The cursor is the only thing standing between a restart and duplicate
			// Issues, so a failure here is worth a warning even though the batch
			// itself succeeded.
			input.Logger.Warn("telegram intake: advance cursor failed", map[string]any{"error": err.Error(), "updateId": result.MaxUpdateID})
		}
	}
	if (result.IssuesOpened > 0 || result.AnswersDelivered > 0) && input.Logger != nil {
		input.Logger.Info("telegram intake: processed updates", map[string]any{
			"issuesOpened": result.IssuesOpened, "answersDelivered": result.AnswersDelivered, "rejected": result.Rejected,
		})
	}
}

// createIntakeIssue opens the Issue for a project the daemon already owns. The
// repo binding comes from the captured catalog, the same authority the discovery
// lanes use, so intake cannot file work against a repo the daemon would not then
// watch.
func createIntakeIssue(ctx context.Context, input defaultSchedulerTickInput, projectID, title, body string) (telegram.IssueRef, error) {
	project, err := input.Repos.Projects.GetByID(ctx, projectID)
	if err != nil {
		return telegram.IssueRef{}, err
	}
	if project == nil || project.Archived {
		return telegram.IssueRef{}, fmt.Errorf("project %s is not an active project on this daemon", projectID)
	}
	repo := telegramIntakeRepo(input, *project)
	if repo == "" {
		return telegram.IssueRef{}, fmt.Errorf("project %s has no repo binding in the captured catalog", projectID)
	}
	created, err := input.GitHubGateway.CreateIssue(ctx, githubinfra.CreateIssueInput{
		Repo:  repo,
		Title: title,
		Body:  body,
		CWD:   project.RepoPath,
	})
	if err != nil {
		return telegram.IssueRef{}, err
	}
	return telegram.IssueRef{Number: created.Number, URL: created.URL}, nil
}

func telegramIntakeRepo(input defaultSchedulerTickInput, project storage.ProjectRecord) string {
	if input.Config != nil {
		if binding, ok := runtimeProjectBinding(*input.Config, project.ID); ok {
			return strings.TrimSpace(binding.Repo)
		}
		return ""
	}
	return repoFromProjectMetadata(project.MetadataJSON)
}

// sendTelegramHITLAsk delivers a mid-run question to the intake chat and records
// which message carried it, so the human's reply routes back to the loop that
// asked. Reusing the intake bot is deliberate: the question arrives in the same
// conversation the work was requested from.
func sendTelegramHITLAsk(ctx context.Context, cfg *config.Config, repos *storage.Repositories, now func() time.Time, ask worker.HITLAskNotification) error {
	if cfg == nil || repos == nil {
		return fmt.Errorf("telegram ask: runtime is not wired")
	}
	intakeCfg := cfg.Intake.Telegram
	if intakeCfg == nil || !intakeCfg.Enabled {
		return fmt.Errorf("telegram ask: intake.telegram is disabled")
	}
	chatID := strings.TrimSpace(intakeCfg.ChatID)
	if chatID == "" {
		return fmt.Errorf("telegram ask: intake.telegram.chatId is required to deliver asks")
	}
	token := strings.TrimSpace(os.Getenv(strings.TrimSpace(intakeCfg.BotTokenEnv)))
	if token == "" {
		return fmt.Errorf("telegram ask: %s is empty", intakeCfg.BotTokenEnv)
	}

	client := telegram.NewClient(token, telegramIntakeHTTPTimeout)
	messageID, err := client.SendMessage(ctx, chatID, telegramAskText(ask), 0)
	if err != nil {
		return err
	}
	if repos.TelegramThreads == nil {
		return nil
	}
	nowISO := eventlog.FormatJavaScriptISOString(now().UTC())
	return repos.TelegramThreads.Upsert(ctx, chatID, strconv.FormatInt(messageID, 10), ask.LoopID, nowISO)
}

// telegramAskText renders the ask as plain text. No inline keyboard: a button
// press would need a separate callback_query lane, and a typed reply already
// carries strictly more than a button can — including "neither, do X instead".
func telegramAskText(ask worker.HITLAskNotification) string {
	var b strings.Builder
	b.WriteString("🤔 looper 需要一個決定才能繼續\n")
	if source := strings.TrimSpace(ask.SourceType + " " + ask.SourceRef); strings.TrimSpace(source) != "" {
		b.WriteString(source)
		if url := strings.TrimSpace(ask.SourceURL); url != "" {
			b.WriteString(" — " + url)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + strings.TrimSpace(ask.Question) + "\n")
	for i, option := range ask.Options {
		b.WriteString(fmt.Sprintf("\n%d. %s", i+1, strings.TrimSpace(option)))
	}
	if len(ask.Options) > 0 {
		b.WriteString("\n")
	}
	if rec := strings.TrimSpace(ask.Recommendation); rec != "" {
		b.WriteString("\n建議:" + rec + "\n")
	}
	b.WriteString("\n直接回覆這則訊息即可(可以選項目,也可以自由描述)。")
	return b.String()
}
