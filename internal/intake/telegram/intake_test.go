package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorder struct {
	issues   []createdIssue
	messages []enqueued
	replies  []string
	failNext error
}

type createdIssue struct {
	projectID string
	title     string
	body      string
}

type enqueued struct {
	loopID string
	text   string
}

func (r *recorder) deps() Deps {
	return Deps{
		DefaultProjectID: "looper",
		AllowedUserIDs:   []int64{42},
		KnownProject:     func(id string) bool { return id == "looper" || id == "novel" },
		CreateIssue: func(_ context.Context, projectID, title, body string) (IssueRef, error) {
			if r.failNext != nil {
				err := r.failNext
				r.failNext = nil
				return IssueRef{}, err
			}
			r.issues = append(r.issues, createdIssue{projectID: projectID, title: title, body: body})
			return IssueRef{Number: int64(len(r.issues)), URL: "https://example.test/issues/1"}, nil
		},
		EnqueueMessage: func(_ context.Context, loopID, text string) error {
			r.messages = append(r.messages, enqueued{loopID: loopID, text: text})
			return nil
		},
		Reply: func(_ context.Context, _ string, _ int64, text string) error {
			r.replies = append(r.replies, text)
			return nil
		},
	}
}

func message(updateID, userID int64, text string) Update {
	return Update{
		UpdateID: updateID,
		Message: &Message{
			MessageID: updateID * 10,
			Text:      text,
			From:      &User{ID: userID},
			Chat:      &Chat{ID: 555},
		},
	}
}

func TestDispatchOpensIssueInDefaultProject(t *testing.T) {
	rec := &recorder{}
	result := Dispatch(context.Background(), []Update{message(1, 42, "sweeper 沒有回收任何 worktree")}, rec.deps())

	if result.IssuesOpened != 1 {
		t.Fatalf("IssuesOpened = %d, want 1", result.IssuesOpened)
	}
	if result.MaxUpdateID != 1 {
		t.Fatalf("MaxUpdateID = %d, want 1", result.MaxUpdateID)
	}
	if got := rec.issues[0].projectID; got != "looper" {
		t.Fatalf("projectID = %q, want looper", got)
	}
	if got := rec.issues[0].title; got != "sweeper 沒有回收任何 worktree" {
		t.Fatalf("title = %q", got)
	}
	if !strings.Contains(rec.issues[0].body, "sweeper 沒有回收任何 worktree") {
		t.Fatalf("body lost the original text: %q", rec.issues[0].body)
	}
}

func TestDispatchRoutesProjectPrefix(t *testing.T) {
	rec := &recorder{}
	Dispatch(context.Background(), []Update{message(1, 42, "#novel 第三章的段落間距壞了")}, rec.deps())

	if len(rec.issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(rec.issues))
	}
	if rec.issues[0].projectID != "novel" {
		t.Fatalf("projectID = %q, want novel", rec.issues[0].projectID)
	}
	if rec.issues[0].title != "第三章的段落間距壞了" {
		t.Fatalf("title kept the prefix: %q", rec.issues[0].title)
	}
}

// An unknown project must not silently fall back to the default: a typo would
// otherwise file work against the wrong repository.
func TestDispatchRejectsUnknownProjectInsteadOfFallingBack(t *testing.T) {
	rec := &recorder{}
	result := Dispatch(context.Background(), []Update{message(1, 42, "#noveel 修一下")}, rec.deps())

	if len(rec.issues) != 0 {
		t.Fatalf("opened %d issues, want 0", len(rec.issues))
	}
	if result.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1", result.Rejected)
	}
	if len(rec.replies) != 1 || !strings.Contains(rec.replies[0], "noveel") {
		t.Fatalf("replies = %v, want one naming the bad project", rec.replies)
	}
	if result.MaxUpdateID != 1 {
		t.Fatalf("MaxUpdateID = %d, want the update consumed", result.MaxUpdateID)
	}
}

func TestDispatchDropsUserOutsideAllowlistSilently(t *testing.T) {
	rec := &recorder{}
	result := Dispatch(context.Background(), []Update{message(1, 999, "開個 issue")}, rec.deps())

	if len(rec.issues) != 0 {
		t.Fatalf("opened %d issues for a stranger, want 0", len(rec.issues))
	}
	if len(rec.replies) != 0 {
		t.Fatalf("replied to a stranger: %v", rec.replies)
	}
	if result.Rejected != 1 || result.MaxUpdateID != 1 {
		t.Fatalf("Rejected=%d MaxUpdateID=%d, want 1 and 1", result.Rejected, result.MaxUpdateID)
	}
}

func TestDispatchRoutesReplyToAskingLoop(t *testing.T) {
	rec := &recorder{}
	deps := rec.deps()
	deps.LoopByReply = func(_ context.Context, chatID, messageID string) string {
		if chatID == "555" && messageID == "77" {
			return "loop-9"
		}
		return ""
	}
	update := message(1, 42, "選 B")
	update.Message.ReplyToMessage = &MessageID{MessageID: 77}

	result := Dispatch(context.Background(), []Update{update}, deps)

	if result.AnswersDelivered != 1 {
		t.Fatalf("AnswersDelivered = %d, want 1", result.AnswersDelivered)
	}
	if len(rec.issues) != 0 {
		t.Fatalf("a reply to an ask opened %d issues, want 0", len(rec.issues))
	}
	if rec.messages[0].loopID != "loop-9" || rec.messages[0].text != "選 B" {
		t.Fatalf("enqueued = %+v", rec.messages[0])
	}
}

// A reply to some unrelated message in the chat is still work, not a lost answer.
func TestDispatchTreatsReplyToUnknownMessageAsNewWork(t *testing.T) {
	rec := &recorder{}
	deps := rec.deps()
	deps.LoopByReply = func(context.Context, string, string) string { return "" }
	update := message(1, 42, "順便把 README 更新一下")
	update.Message.ReplyToMessage = &MessageID{MessageID: 12345}

	Dispatch(context.Background(), []Update{update}, deps)

	if len(rec.issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(rec.issues))
	}
}

// A failed Issue creation must leave the cursor behind that update so the next
// pass retries it — otherwise the request is silently dropped.
func TestDispatchHoldsCursorWhenIssueCreationFails(t *testing.T) {
	rec := &recorder{failNext: errors.New("gh: rate limited")}
	deps := rec.deps()

	result := Dispatch(context.Background(), []Update{message(7, 42, "修一下"), message(8, 42, "另一件事")}, deps)

	if result.MaxUpdateID != 8 {
		t.Fatalf("MaxUpdateID = %d, want 8 (the later success still advances)", result.MaxUpdateID)
	}
	if result.IssuesOpened != 1 {
		t.Fatalf("IssuesOpened = %d, want 1", result.IssuesOpened)
	}
}

func TestDispatchAdvancesCursorPastUnusableUpdates(t *testing.T) {
	rec := &recorder{}
	updates := []Update{
		{UpdateID: 3},         // no message at all (e.g. an edited post)
		message(4, 42, "   "), // whitespace only
	}
	result := Dispatch(context.Background(), updates, rec.deps())

	if result.MaxUpdateID != 4 {
		t.Fatalf("MaxUpdateID = %d, want 4 — an ignored update must not be re-read forever", result.MaxUpdateID)
	}
	if len(rec.issues) != 0 {
		t.Fatalf("issues = %d, want 0", len(rec.issues))
	}
}

func TestIssueTitleTruncatesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("字", maxTitleRunes+20)
	title := issueTitle(long + "\n第二行")

	if !strings.HasSuffix(title, "…") {
		t.Fatalf("title = %q, want an ellipsis", title)
	}
	if strings.Contains(title, "第二行") {
		t.Fatalf("title leaked a later line: %q", title)
	}
	if len([]rune(title)) != maxTitleRunes+1 {
		t.Fatalf("title rune count = %d, want %d", len([]rune(title)), maxTitleRunes+1)
	}
}

func TestIssueTitleTakesFirstLine(t *testing.T) {
	if got := issueTitle("修 sweeper\n\n細節在這裡"); got != "修 sweeper" {
		t.Fatalf("title = %q", got)
	}
}
