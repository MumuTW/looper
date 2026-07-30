package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxTitleRunes bounds a generated Issue title. GitHub accepts far more, but a
// title long enough to wrap is worse than a truncated one with the full text in
// the body.
const maxTitleRunes = 100

// Deps are the effects the dispatcher needs. Every one is injected so the routing
// decisions below are testable without a network or a database.
type Deps struct {
	// LoopByReply maps a reply target (chat id, replied-to message id) to the loop
	// that asked there, or "" when the message is not a reply to one of our asks.
	LoopByReply func(ctx context.Context, chatID, messageID string) string
	// EnqueueMessage hands free text to a loop as a human message. Mirrors the
	// Feishu transport: a typed reply is conversational, not a forced resolution.
	EnqueueMessage func(ctx context.Context, loopID, text string) error
	// CreateIssue opens an Issue in the resolved project and returns its URL.
	CreateIssue func(ctx context.Context, projectID, title, body string) (IssueRef, error)
	// Reply sends text back into the originating chat, threaded under the message
	// that triggered it.
	Reply func(ctx context.Context, chatID string, replyTo int64, text string) error
	// KnownProject reports whether a project id is registered on this daemon.
	KnownProject func(projectID string) bool
	// DefaultProjectID receives messages that name no project.
	DefaultProjectID string
	// AllowedUserIDs is the intake allowlist. An update from anyone else is
	// dropped without a reply.
	AllowedUserIDs []int64
	LogWarn        func(msg string, fields map[string]any)
	LogDebug       func(msg string, fields map[string]any)
}

// IssueRef identifies an Issue that intake just opened.
type IssueRef struct {
	Number int64
	URL    string
}

// Result summarizes one dispatch pass.
type Result struct {
	IssuesOpened     int
	AnswersDelivered int
	Rejected         int
	// MaxUpdateID is the highest update_id seen, including ones that were ignored:
	// re-reading an update we deliberately dropped would drop it again forever.
	MaxUpdateID int64
}

// Dispatch routes a batch of updates. Each update is independent — one failure
// does not abandon the batch — but MaxUpdateID only advances past an update whose
// side effect either succeeded or was deliberately skipped, so a failed Issue
// creation is retried on the next pass rather than silently lost.
func Dispatch(ctx context.Context, updates []Update, deps Deps) Result {
	var result Result
	for _, update := range updates {
		msg := update.Message
		if msg == nil || msg.Chat == nil || msg.From == nil {
			result.MaxUpdateID = maxInt64(result.MaxUpdateID, update.UpdateID)
			continue
		}
		text := strings.TrimSpace(msg.Text)
		chatID := strconv.FormatInt(msg.Chat.ID, 10)
		if text == "" {
			result.MaxUpdateID = maxInt64(result.MaxUpdateID, update.UpdateID)
			continue
		}
		if !allowed(msg.From.ID, deps.AllowedUserIDs) {
			// Silent drop: replying would confirm the bot is live to anyone who
			// guesses its handle.
			result.Rejected++
			result.MaxUpdateID = maxInt64(result.MaxUpdateID, update.UpdateID)
			if deps.LogDebug != nil {
				deps.LogDebug("telegram intake: dropped update from user outside allowlist", map[string]any{"userId": msg.From.ID, "chatId": chatID})
			}
			continue
		}

		if err := dispatchOne(ctx, chatID, text, msg.MessageID, replyTargetID(msg), deps, &result); err != nil {
			if deps.LogWarn != nil {
				deps.LogWarn("telegram intake: update failed", map[string]any{"updateId": update.UpdateID, "chatId": chatID, "error": err.Error()})
			}
			// Leave the cursor behind this update so the next pass retries it.
			continue
		}
		result.MaxUpdateID = maxInt64(result.MaxUpdateID, update.UpdateID)
	}
	return result
}

// dispatchOne applies a single message. A reply to one of our ask messages goes
// to the loop that asked; anything else opens an Issue.
func dispatchOne(ctx context.Context, chatID, text string, messageID, replyTo int64, deps Deps, result *Result) error {
	if replyTo > 0 && deps.LoopByReply != nil && deps.EnqueueMessage != nil {
		loopID := deps.LoopByReply(ctx, chatID, strconv.FormatInt(replyTo, 10))
		if loopID != "" {
			if err := deps.EnqueueMessage(ctx, loopID, text); err != nil {
				return fmt.Errorf("enqueue human message for loop %s: %w", loopID, err)
			}
			result.AnswersDelivered++
			return nil
		}
		// A reply to something else in the chat is treated as new work, not lost.
	}

	projectID, body := resolveProject(text, deps)
	if projectID == "" {
		// An unroutable message is a configuration or typing mistake, not a
		// failure to retry: tell the sender and move on.
		result.Rejected++
		reply(ctx, deps, chatID, messageID, unroutableMessage(text, deps))
		return nil
	}
	if deps.CreateIssue == nil {
		return fmt.Errorf("telegram intake: CreateIssue is not wired")
	}
	issue, err := deps.CreateIssue(ctx, projectID, issueTitle(body), issueBody(body, chatID))
	if err != nil {
		return fmt.Errorf("create issue in %s: %w", projectID, err)
	}
	result.IssuesOpened++
	reply(ctx, deps, chatID, messageID, fmt.Sprintf("已開 issue #%d — %s\n(%s)", issue.Number, issue.URL, projectID))
	return nil
}

// resolveProject reads an optional leading "#<projectId>" token and returns the
// project the Issue belongs to plus the remaining text. An unknown explicit
// project resolves to "" rather than silently falling back to the default, so a
// typo cannot file work against the wrong repository.
func resolveProject(text string, deps Deps) (projectID, body string) {
	if strings.HasPrefix(text, "#") {
		token, rest, _ := strings.Cut(text, " ")
		candidate := strings.TrimSpace(strings.TrimPrefix(token, "#"))
		if candidate != "" {
			if deps.KnownProject != nil && !deps.KnownProject(candidate) {
				return "", strings.TrimSpace(rest)
			}
			return candidate, strings.TrimSpace(rest)
		}
	}
	if deps.KnownProject != nil && deps.DefaultProjectID != "" && !deps.KnownProject(deps.DefaultProjectID) {
		return "", text
	}
	return deps.DefaultProjectID, text
}

func unroutableMessage(text string, deps Deps) string {
	if strings.HasPrefix(text, "#") {
		token, _, _ := strings.Cut(text, " ")
		return fmt.Sprintf("找不到 project %q。用 #<projectId> 指定,或省略前綴送到預設 project。", strings.TrimPrefix(token, "#"))
	}
	if strings.TrimSpace(deps.DefaultProjectID) == "" {
		return "沒有設定 intake.telegram.defaultProjectId,請用 #<projectId> 指定 project。"
	}
	return fmt.Sprintf("預設 project %q 沒有註冊在這個 daemon 上。", deps.DefaultProjectID)
}

// issueTitle takes the first line, truncated on a rune boundary. The untruncated
// text always survives in the body.
func issueTitle(text string) string {
	first := strings.TrimSpace(text)
	if idx := strings.IndexAny(first, "\r\n"); idx >= 0 {
		first = strings.TrimSpace(first[:idx])
	}
	if first == "" {
		return "Untitled request from Telegram"
	}
	if utf8.RuneCountInString(first) <= maxTitleRunes {
		return first
	}
	runes := []rune(first)
	return strings.TrimSpace(string(runes[:maxTitleRunes])) + "…"
}

// issueBody keeps the message verbatim and records where it came from. Intake does
// not expand, rewrite, or classify the request: judging whether it is specific
// enough is Triager's job, and rewriting here would hide the original wording from
// that judgement.
func issueBody(text, chatID string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n\n---\n")
	b.WriteString(fmt.Sprintf("_Opened by looper Telegram intake (chat `%s`)._\n", chatID))
	return b.String()
}

func replyTargetID(msg *Message) int64 {
	if msg.ReplyToMessage == nil {
		return 0
	}
	return msg.ReplyToMessage.MessageID
}

func reply(ctx context.Context, deps Deps, chatID string, replyTo int64, text string) {
	if deps.Reply == nil {
		return
	}
	if err := deps.Reply(ctx, chatID, replyTo, text); err != nil && deps.LogWarn != nil {
		deps.LogWarn("telegram intake: reply failed", map[string]any{"chatId": chatID, "error": err.Error()})
	}
}

func allowed(userID int64, allowlist []int64) bool {
	for _, id := range allowlist {
		if id == userID {
			return true
		}
	}
	return false
}

func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}
