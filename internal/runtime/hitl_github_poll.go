package runtime

import (
	"context"
	"strings"
)

type contextType = context.Context

// githubAnswerComment is the minimal shape the HITL answer detector needs from a
// PR issue comment.
type githubAnswerComment struct {
	ID     int64
	Author string
	Body   string
}

// detectGitHubHITLAnswer returns the human's answer to a GitHub HITL ask, or ""
// when none has arrived yet. The answer is the FIRST comment posted after the ask
// (comment id > askCommentID; GitHub comment ids are monotonic) by someone other
// than the bot itself (author != selfLogin). When answerAuthors is non-empty the
// commenter must be on that allowlist; otherwise any non-bot human may answer.
// Empty-bodied comments are ignored so ordinary reactions/edits don't count.
func detectGitHubHITLAnswer(comments []githubAnswerComment, askCommentID int64, selfLogin string, answerAuthors []string) string {
	allow := make(map[string]bool, len(answerAuthors))
	for _, a := range answerAuthors {
		if a = strings.TrimSpace(a); a != "" {
			allow[strings.ToLower(a)] = true
		}
	}
	bestID := int64(0)
	answer := ""
	for _, c := range comments {
		if c.ID <= askCommentID {
			continue
		}
		author := strings.TrimSpace(c.Author)
		if author == "" || strings.EqualFold(author, strings.TrimSpace(selfLogin)) {
			continue
		}
		if len(allow) > 0 && !allow[strings.ToLower(author)] {
			continue
		}
		body := strings.TrimSpace(c.Body)
		if body == "" {
			continue
		}
		if bestID == 0 || c.ID < bestID {
			bestID = c.ID
			answer = body
		}
	}
	return answer
}

// githubHITLPollDeps are the injected dependencies of the answer-poll lane, kept
// as functions so the lane is testable and decoupled from the scheduler wiring.
type githubHITLPollDeps struct {
	// listComments returns a PR's issue comments (oldest-first is fine; the
	// detector orders by id).
	listComments func(ctx contextType, repo string, prNumber int64, cwd string) ([]githubAnswerComment, error)
	// currentUser returns the bot's own login for the repo's host (to exclude the
	// bot's own comments).
	currentUser func(ctx contextType, cwd string) string
	// deliverAnswer feeds the human's answer into the shared HITL core (flips the
	// loop to running + requeues for resume). Wired to the api handler.
	deliverAnswer func(ctx contextType, loopID, answer string) error
	// clearAwaiting removes the awaiting-human label from the PR after delivery.
	clearAwaiting func(ctx contextType, repo string, prNumber int64, cwd string)
	// projectCWD returns the local repo path for a project (gh runs there).
	projectCWD    func(projectID string) string
	answerAuthors []string
	logWarn       func(msg string, fields map[string]any)
}

// githubHITLAwaitingLoop is the minimal loop shape the lane needs.
type githubHITLAwaitingLoop struct {
	ID           string
	ProjectID    string
	Repo         string
	Transport    string
	AskStatus    string
	PRNumber     int64
	AskCommentID int64
}

// pollGitHubHITLAnswersOnce runs one pass of the answer-poll lane: for each loop
// waiting on a GitHub HITL answer, it looks for a human's reply after the ask and
// delivers it. It is idempotent — a loop that leaves awaiting_human on delivery
// simply won't be passed in again.
func pollGitHubHITLAnswersOnce(ctx contextType, loops []githubHITLAwaitingLoop, deps githubHITLPollDeps) int {
	delivered := 0
	for _, loop := range loops {
		if !strings.EqualFold(strings.TrimSpace(loop.Transport), "github") || loop.PRNumber == 0 {
			continue
		}
		if s := strings.TrimSpace(loop.AskStatus); s != "" && s != "awaiting" {
			continue
		}
		repo := strings.TrimSpace(loop.Repo)
		if repo == "" {
			continue
		}
		cwd := ""
		if deps.projectCWD != nil {
			cwd = deps.projectCWD(loop.ProjectID)
		}
		comments, err := deps.listComments(ctx, repo, loop.PRNumber, cwd)
		if err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github poll: list comments failed", map[string]any{"loopId": loop.ID, "repo": repo, "pr": loop.PRNumber, "error": err.Error()})
			}
			continue
		}
		self := ""
		if deps.currentUser != nil {
			self = deps.currentUser(ctx, cwd)
		}
		answer := detectGitHubHITLAnswer(comments, loop.AskCommentID, self, deps.answerAuthors)
		if answer == "" {
			continue
		}
		if err := deps.deliverAnswer(ctx, loop.ID, answer); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github poll: deliver answer failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if deps.clearAwaiting != nil {
			deps.clearAwaiting(ctx, repo, loop.PRNumber, cwd)
		}
		delivered++
	}
	return delivered
}
