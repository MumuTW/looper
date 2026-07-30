package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorder struct {
	issues     []createdIssue
	replies    []string
	stamps     []string
	existing   map[string]int64
	createErr  error
	resolveErr error
	lookupErr  error
}

type createdIssue struct {
	projectID string
	title     string
	body      string
}

func (r *recorder) deps() Deps {
	return Deps{
		DefaultProjectID: "looper",
		AllowedUserIDs:   []int64{42},
		ResolveProject: func(_ context.Context, projectID string) (Target, error) {
			if r.resolveErr != nil {
				return Target{}, r.resolveErr
			}
			switch projectID {
			case "looper", "novel":
				return Target{ProjectID: projectID, Repo: "acme/" + projectID, RepoPath: "/tmp/" + projectID}, nil
			default:
				return Target{Unroutable: "沒有這個 project"}, nil
			}
		},
		FindIssueBySourceStamp: func(_ context.Context, _ Target, stamp string) (int64, error) {
			if r.lookupErr != nil {
				return 0, r.lookupErr
			}
			r.stamps = append(r.stamps, stamp)
			return r.existing[stamp], nil
		},
		CreateIssue: func(_ context.Context, target Target, title, body string) (IssueRef, error) {
			if r.createErr != nil {
				err := r.createErr
				r.createErr = nil
				return IssueRef{}, err
			}
			r.issues = append(r.issues, createdIssue{projectID: target.ProjectID, title: title, body: body})
			return IssueRef{Number: int64(len(r.issues)), URL: "https://example.test/issues/1"}, nil
		},
		Reply: func(_ context.Context, _, text string) error {
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

	if result.IssuesOpened != 1 || result.AckedUpdateID != 1 {
		t.Fatalf("result = %#v, want one issue and update 1 acked", result)
	}
	if rec.issues[0].projectID != "looper" || rec.issues[0].title != "sweeper 沒有回收任何 worktree" {
		t.Fatalf("issue = %+v", rec.issues[0])
	}
	if !strings.Contains(rec.issues[0].body, SourceStamp("555", 10)) {
		t.Fatalf("issue body carries no source stamp:\n%s", rec.issues[0].body)
	}
}

// The stamp is the only durable link between a chat message and its Issue, so a
// replayed update must find the existing Issue instead of opening a second one.
func TestDispatchSkipsAnUpdateThatAlreadyHasAnIssue(t *testing.T) {
	rec := &recorder{existing: map[string]int64{SourceStamp("555", 10): 23}}

	result := Dispatch(context.Background(), []Update{message(1, 42, "修一下 sweeper")}, rec.deps())

	if len(rec.issues) != 0 {
		t.Fatalf("opened %d issues for a replayed update, want 0", len(rec.issues))
	}
	if result.Duplicates != 1 || result.AckedUpdateID != 1 {
		t.Fatalf("result = %#v, want the replay recognised and acked", result)
	}
}

func TestDispatchRoutesProjectPrefix(t *testing.T) {
	rec := &recorder{}
	Dispatch(context.Background(), []Update{message(1, 42, "#novel 第三章的段落間距壞了")}, rec.deps())

	if len(rec.issues) != 1 || rec.issues[0].projectID != "novel" {
		t.Fatalf("issues = %+v", rec.issues)
	}
	if rec.issues[0].title != "第三章的段落間距壞了" {
		t.Fatalf("title kept the prefix: %q", rec.issues[0].title)
	}
}

// A prefix is a token, not a space-delimited word: a newline or tab after it is
// the same instruction and must route the same way.
func TestDispatchRoutesProjectPrefixAcrossAnyWhitespace(t *testing.T) {
	for _, separator := range []string{"\n", "\t", "\r\n", "  "} {
		rec := &recorder{}
		Dispatch(context.Background(), []Update{message(1, 42, "#novel"+separator+"第三章壞了")}, rec.deps())

		if len(rec.issues) != 1 {
			t.Fatalf("separator %q: issues = %d, want 1 (replies: %v)", separator, len(rec.issues), rec.replies)
		}
		if rec.issues[0].projectID != "novel" || rec.issues[0].title != "第三章壞了" {
			t.Fatalf("separator %q: issue = %+v", separator, rec.issues[0])
		}
	}
}

// An unknown project must not silently fall back to the default: a typo would
// otherwise file work against the wrong repository.
func TestDispatchRejectsUnknownProjectInsteadOfFallingBack(t *testing.T) {
	rec := &recorder{}
	result := Dispatch(context.Background(), []Update{message(1, 42, "#noveel 修一下")}, rec.deps())

	if len(rec.issues) != 0 || result.Rejected != 1 {
		t.Fatalf("result = %#v, issues = %d", result, len(rec.issues))
	}
	if len(rec.replies) != 1 || !strings.Contains(rec.replies[0], "noveel") {
		t.Fatalf("replies = %v, want one naming the bad project", rec.replies)
	}
	if result.AckedUpdateID != 1 {
		t.Fatalf("AckedUpdateID = %d — a sender mistake is not retryable", result.AckedUpdateID)
	}
}

func TestDispatchDropsUserOutsideAllowlistSilently(t *testing.T) {
	rec := &recorder{}
	result := Dispatch(context.Background(), []Update{message(1, 999, "開個 issue")}, rec.deps())

	if len(rec.issues) != 0 || len(rec.replies) != 0 {
		t.Fatalf("issues = %d, replies = %v — a stranger got a response", len(rec.issues), rec.replies)
	}
	if result.Rejected != 1 || result.AckedUpdateID != 1 {
		t.Fatalf("result = %#v", result)
	}
}

// A later success must never acknowledge an earlier failure: that is exactly how
// a transient outage silently swallows someone's request.
func TestDispatchStopsAcknowledgingAtTheFirstRetryableFailure(t *testing.T) {
	rec := &recorder{createErr: errors.New("gh: 502 bad gateway")}

	result := Dispatch(context.Background(), []Update{message(7, 42, "第一件事"), message(8, 42, "第二件事")}, rec.deps())

	if result.AckedUpdateID != 0 {
		t.Fatalf("AckedUpdateID = %d, want 0 — the failed update must be retried", result.AckedUpdateID)
	}
	if len(rec.issues) != 0 {
		t.Fatalf("issues = %d, want 0 — the batch stops at the failure", len(rec.issues))
	}
}

// A message the forge will never accept must not wedge the lane behind it.
func TestDispatchConsumesAPermanentlyRejectedUpdate(t *testing.T) {
	rec := &recorder{createErr: Permanent(errors.New("contains a private key block"))}

	result := Dispatch(context.Background(), []Update{message(7, 42, "把這個 key 存起來"), message(8, 42, "第二件事")}, rec.deps())

	if result.AckedUpdateID != 8 {
		t.Fatalf("AckedUpdateID = %d, want 8 — a permanent rejection is consumed", result.AckedUpdateID)
	}
	if len(rec.issues) != 1 {
		t.Fatalf("issues = %d, want the second message still handled", len(rec.issues))
	}
}

// A storage blip is not the sender's mistake. Rejecting it would tell them the
// project does not exist and consume the message.
func TestDispatchHoldsWhenProjectLookupFails(t *testing.T) {
	rec := &recorder{resolveErr: errors.New("database is locked")}

	result := Dispatch(context.Background(), []Update{message(1, 42, "修一下")}, rec.deps())

	if result.AckedUpdateID != 0 || result.Rejected != 0 {
		t.Fatalf("result = %#v, want the update held for retry", result)
	}
	if len(rec.replies) != 0 {
		t.Fatalf("replies = %v, want no misleading rejection", rec.replies)
	}
}

// If the duplicate check itself fails, creating anyway risks a second Issue.
func TestDispatchHoldsWhenTheDuplicateCheckFails(t *testing.T) {
	rec := &recorder{lookupErr: errors.New("gh: rate limited")}

	result := Dispatch(context.Background(), []Update{message(1, 42, "修一下")}, rec.deps())

	if len(rec.issues) != 0 {
		t.Fatalf("created an issue without knowing whether one exists: %+v", rec.issues)
	}
	if result.AckedUpdateID != 0 {
		t.Fatalf("AckedUpdateID = %d, want 0", result.AckedUpdateID)
	}
}

func TestDispatchAcknowledgesUnusableUpdates(t *testing.T) {
	rec := &recorder{}
	updates := []Update{
		{UpdateID: 3},         // no message at all
		message(4, 42, "   "), // whitespace only
	}
	result := Dispatch(context.Background(), updates, rec.deps())

	if result.AckedUpdateID != 4 {
		t.Fatalf("AckedUpdateID = %d — an ignored update must not be re-read forever", result.AckedUpdateID)
	}
	if len(rec.issues) != 0 {
		t.Fatalf("issues = %d, want 0", len(rec.issues))
	}
}

func TestIssueTitleTruncatesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("字", maxTitleRunes+20)
	title := issueTitle(long + "\n第二行")

	if !strings.HasSuffix(title, "…") || strings.Contains(title, "第二行") {
		t.Fatalf("title = %q", title)
	}
	if len([]rune(title)) != maxTitleRunes+1 {
		t.Fatalf("title rune count = %d, want %d", len([]rune(title)), maxTitleRunes+1)
	}
}

func TestSplitProjectPrefix(t *testing.T) {
	cases := []struct {
		name, text, wantProject, wantBody string
	}{
		{name: "no prefix", text: "修一下", wantProject: "looper", wantBody: "修一下"},
		{name: "prefix with space", text: "#novel 修一下", wantProject: "novel", wantBody: "修一下"},
		{name: "prefix with newline", text: "#novel\n修一下", wantProject: "novel", wantBody: "修一下"},
		{name: "prefix only", text: "#novel", wantProject: "novel", wantBody: ""},
		{name: "bare hash", text: "# 修一下", wantProject: "looper", wantBody: "# 修一下"},
		{name: "hash mid-text", text: "看 #novel 的問題", wantProject: "looper", wantBody: "看 #novel 的問題"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project, body := splitProjectPrefix(tc.text, "looper")
			if project != tc.wantProject || body != tc.wantBody {
				t.Fatalf("splitProjectPrefix(%q) = (%q, %q), want (%q, %q)", tc.text, project, body, tc.wantProject, tc.wantBody)
			}
		})
	}
}

func TestTruncateRunesKeepsMessagesInsideTheTelegramLimit(t *testing.T) {
	long := strings.Repeat("字", maxMessageRunes+50)
	got := truncateRunes(long, maxMessageRunes)

	if len([]rune(got)) != maxMessageRunes {
		t.Fatalf("rune count = %d, want %d", len([]rune(got)), maxMessageRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated text does not signal truncation: %q", got[len(got)-10:])
	}
}
