package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxTitleRunes bounds a generated Issue title. The untruncated text always
// survives in the body.
const maxTitleRunes = 100

// SourceStamp is the marker written into an Issue body identifying the chat
// message that opened it. It is the only durable link between a Telegram update
// and its Issue, and the basis of the duplicate check.
func SourceStamp(chatID string, messageID int64) string {
	return fmt.Sprintf("looper-intake-telegram-%s-%d", chatID, messageID)
}

// Deps are the effects the dispatcher needs. Every one is injected so the
// routing decisions below are testable without a network.
type Deps struct {
	// ResolveProject maps a project id to the repository intake may open an Issue
	// in. It reports Unroutable with a human-readable reason when the project is
	// unknown, archived, or not backed by a provider intake can create Issues in.
	// A transport or storage failure must be returned as an error instead, so a
	// request is never rejected as invalid because a lookup was briefly down.
	ResolveProject func(ctx context.Context, projectID string) (Target, error)
	// FindIssueBySourceStamp reports an Issue already opened for this stamp, or 0.
	FindIssueBySourceStamp func(ctx context.Context, target Target, stamp string) (int64, error)
	// CreateIssue opens the Issue and returns its number and URL.
	CreateIssue func(ctx context.Context, target Target, title, body string) (IssueRef, error)
	// Reply sends text back into the originating chat.
	Reply func(ctx context.Context, chatID, text string) error
	// DefaultProjectID receives messages that name no project.
	DefaultProjectID string
	// AllowedUserIDs is the intake allowlist. An update from anyone else is
	// dropped without a reply.
	AllowedUserIDs []int64
	LogWarn        func(msg string, fields map[string]any)
	LogDebug       func(msg string, fields map[string]any)
}

// Target is a project intake can open an Issue in.
type Target struct {
	ProjectID string
	Repo      string
	RepoPath  string
	// Unroutable, when non-empty, explains why this project cannot take intake
	// work. It is a permanent condition: the sender is told and the update is
	// consumed rather than retried forever.
	Unroutable string
}

// IssueRef identifies an Issue intake opened or found.
type IssueRef struct {
	Number int64
	URL    string
}

// Result summarizes one dispatch pass.
type Result struct {
	IssuesOpened int
	Duplicates   int
	Rejected     int
	// AckedUpdateID is the highest update id that may be acknowledged to Telegram.
	// It stops at the first update whose effect failed for a retryable reason, so
	// a transient failure is retried on the next pass instead of being consumed.
	AckedUpdateID int64
}

// permanentError marks a failure that will not succeed on retry, so the update
// is consumed rather than blocking the lane forever.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps an error that must not be retried — content the outbound guard
// will always reject, a project that will never be routable.
func Permanent(err error) error { return permanentError{err: err} }

// IsPermanent reports whether an error is one Dispatch will consume rather than
// retry.
func IsPermanent(err error) bool {
	var permanent permanentError
	return errors.As(err, &permanent)
}

// Dispatch routes a batch of updates in order, stopping the acknowledgement at
// the first retryable failure.
//
// Updates are processed in order and the acknowledgement is contiguous: a later
// success never acknowledges an earlier failure. This costs re-processing the
// successful tail on the next pass — which is safe, because the duplicate check
// below is what actually prevents a second Issue.
func Dispatch(ctx context.Context, updates []Update, deps Deps) Result {
	var result Result
	for _, update := range updates {
		err := dispatchOne(ctx, update, deps, &result)
		if err != nil && !IsPermanent(err) {
			if deps.LogWarn != nil {
				deps.LogWarn("telegram intake: update failed, holding the offset", map[string]any{
					"updateId": update.UpdateID, "error": err.Error(),
				})
			}
			return result
		}
		if err != nil && deps.LogWarn != nil {
			deps.LogWarn("telegram intake: dropped an update that cannot succeed on retry", map[string]any{
				"updateId": update.UpdateID, "error": err.Error(),
			})
		}
		result.AckedUpdateID = update.UpdateID
	}
	return result
}

func dispatchOne(ctx context.Context, update Update, deps Deps, result *Result) error {
	msg := update.Message
	if msg == nil || msg.Chat == nil || msg.From == nil {
		return nil
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return nil
	}
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	if !allowed(msg.From.ID, deps.AllowedUserIDs) {
		// Silent drop: replying would confirm the bot is live to anyone who
		// guesses its handle.
		result.Rejected++
		if deps.LogDebug != nil {
			deps.LogDebug("telegram intake: dropped update from user outside allowlist", map[string]any{"userId": msg.From.ID, "chatId": chatID})
		}
		return nil
	}

	projectID, body := splitProjectPrefix(text, deps.DefaultProjectID)
	if projectID == "" {
		result.Rejected++
		reply(ctx, deps, chatID, "沒有可用的預設 project,請用 #<projectId> 指定。")
		return nil
	}
	target, err := deps.ResolveProject(ctx, projectID)
	if err != nil {
		// A lookup failure is not the sender's mistake: hold the offset and retry.
		return fmt.Errorf("resolve project %s: %w", projectID, err)
	}
	if target.Unroutable != "" {
		result.Rejected++
		reply(ctx, deps, chatID, fmt.Sprintf("project %q 無法接收:%s", projectID, target.Unroutable))
		return nil
	}

	stamp := SourceStamp(chatID, msg.MessageID)
	if deps.FindIssueBySourceStamp != nil {
		existing, err := deps.FindIssueBySourceStamp(ctx, target, stamp)
		if err != nil {
			return fmt.Errorf("look up existing issue for %s: %w", stamp, err)
		}
		if existing > 0 {
			result.Duplicates++
			if deps.LogDebug != nil {
				deps.LogDebug("telegram intake: update already has an issue", map[string]any{"stamp": stamp, "issue": existing})
			}
			return nil
		}
	}

	issue, err := deps.CreateIssue(ctx, target, issueTitle(body), issueBody(body, chatID, msg.MessageID))
	if err != nil {
		return err
	}
	result.IssuesOpened++
	reply(ctx, deps, chatID, fmt.Sprintf("已開 issue #%d — %s\n(%s)", issue.Number, issue.URL, target.ProjectID))
	return nil
}

// splitProjectPrefix reads an optional leading "#<projectId>" token and returns
// the project the Issue belongs to plus the remaining text. The token ends at any
// whitespace, so a prefix followed by a newline or tab routes the same as one
// followed by a space.
func splitProjectPrefix(text, defaultProjectID string) (projectID, body string) {
	if !strings.HasPrefix(text, "#") {
		return strings.TrimSpace(defaultProjectID), text
	}
	rest := text[len("#"):]
	end := strings.IndexFunc(rest, unicode.IsSpace)
	if end < 0 {
		return strings.TrimSpace(rest), ""
	}
	candidate := strings.TrimSpace(rest[:end])
	if candidate == "" {
		return strings.TrimSpace(defaultProjectID), text
	}
	return candidate, strings.TrimSpace(rest[end:])
}

// issueTitle takes the first line, truncated on a rune boundary.
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

// issueBody keeps the message verbatim and carries the source stamp. Intake does
// not expand, rewrite, or classify the request: judging whether it is specific
// enough is Triager's job, and rewriting here would hide the original wording
// from that judgement.
func issueBody(text, chatID string, messageID int64) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n\n---\n")
	b.WriteString(fmt.Sprintf("_Opened by looper Telegram intake._\n\n<!-- %s -->\n", SourceStamp(chatID, messageID)))
	return b.String()
}

func reply(ctx context.Context, deps Deps, chatID, text string) {
	if deps.Reply == nil {
		return
	}
	if err := deps.Reply(ctx, chatID, text); err != nil && deps.LogWarn != nil {
		// The Issue already exists (or was already declined); a lost confirmation
		// message is cosmetic and must not hold the offset.
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
