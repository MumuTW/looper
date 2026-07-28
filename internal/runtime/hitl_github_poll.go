package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

type contextType = context.Context

// githubAnswerComment is the minimal shape the HITL answer detector needs from a
// PR issue comment.
type githubAnswerComment struct {
	ID     int64
	Author string
	Body   string
}

// looperCommentMarker tags every comment looper itself posts (the ask marker and
// the disclosure stamp both start with it), so a comment carrying it is
// bot-authored and can never be mistaken for a human answer — this is robust even
// when the bot and a human share the same GitHub account.
const looperCommentMarker = "<!-- looper:"

// detectGitHubHITLAnswer returns the human's answer to a GitHub HITL ask, or ""
// when none has arrived yet. The answer is the FIRST comment posted after the ask
// (comment id > askCommentID; GitHub comment ids are monotonic) that is NOT one of
// looper's own comments (no looper marker). When answerAuthors is non-empty the
// commenter must be on that allowlist; otherwise any human reply may answer.
// Empty-bodied comments are ignored so ordinary reactions/edits don't count.
func detectGitHubHITLAnswer(comments []githubAnswerComment, askCommentID int64, answerAuthors []string) string {
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
		if strings.Contains(c.Body, looperCommentMarker) {
			continue // looper's own comment (ask / progress / decision-log), never an answer
		}
		author := strings.TrimSpace(c.Author)
		if author == "" {
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
	Provider     string // "github" | "forgejo" | "" (legacy: resolve from project)
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
		answer := detectGitHubHITLAnswer(comments, loop.AskCommentID, deps.answerAuthors)
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

// deliverHITLAnswerToLoop is the runtime-side equivalent of the api
// handler's deliverHumanAnswer for the poll lane: it stores the human's answer on
// an awaiting_human loop, flips it back to running, and requeues the queue item
// that suspendForHuman cancelled — so the worker resumes with the answer.
// enqueueHumanMessageToLoop queues a free-text human message for a loop and makes
// sure it gets consumed soon: a loop that isn't actively running is nudged to
// queued so the scheduler picks it up and the worker drains the message on its
// next turn; a running loop drains it when the current turn ends. Terminal loops
// are left alone (a message can't reopen a finished loop yet). Unlike a button
// answer, a message does NOT resolve a pending ask — the agent reads it and
// decides whether to proceed, answer, or ask again.
func enqueueHumanMessageToLoop(ctx context.Context, repos *storage.Repositories, nowISO, loopID, text string) error {
	// Share process-wide requeue exclusion with API discard+retry so free-text
	// inbox delivery cannot requeue paused/waiting/manual_intervention loops
	// between discard preflight and git reset (see LockLoopRequeue).
	// Call order: per-loop lock first, then same-target lock (matches API).
	unlock := LockLoopRequeue(loopID)
	defer unlock()

	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	switch loop.Status {
	case "completed", "failed", "stopped", "terminated", "human_takeover":
		return nil
	}
	// Same-target exclusion: a different waiting loop on this PR/issue can
	// otherwise requeue while discard+retry holds only that other loop's
	// per-loop mutex and wipes the shared worktree before the retry TX.
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*loop))
	defer unlockTarget()

	meta, werr := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: nowISO, Text: text})
	if werr != nil {
		return werr
	}
	updated := *loop
	updated.MetadataJSON = &meta
	updated.UpdatedAt = nowISO
	notRunning := loop.Status != "running"
	if notRunning {
		// Wake it so the message is consumed ASAP; a running loop keeps running and
		// drains on its next turn.
		updated.Status = "queued"
		updated.NextRunAt = &nowISO
	}
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	if notRunning {
		_, err = repos.Queue.RequeueLatestCancelledByLoop(ctx, loopID, nowISO)
	}
	return err
}

func deliverHITLAnswerToLoop(ctx context.Context, repos *storage.Repositories, nowISO, loopID, answer string) error {
	// Same requeue + target exclusion as free-text enqueue / API discard+retry.
	unlock := LockLoopRequeue(loopID)
	defer unlock()

	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	if loop.Status != "awaiting_human" {
		return nil
	}
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*loop))
	defer unlockTarget()
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		return nil
	}
	ask.Answer = answer
	ask.Status = "answered"
	ask.AnsweredAt = nowISO
	meta, werr := loops.WriteHITLAsk(loop.MetadataJSON, ask)
	if werr != nil {
		return werr
	}
	updated := *loop
	updated.MetadataJSON = &meta
	updated.Status = "running"
	updated.NextRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	_, err = repos.Queue.RequeueLatestCancelledByLoop(ctx, loopID, nowISO)
	return err
}

// hitlPRCommentProvider reports whether a PR-comment HITL ask should be polled
// via Forgejo vs GitHub.com. Prefer the persisted ask.Provider; fall back to
// the project's configured provider binding for legacy asks.
func hitlPRCommentProvider(cfg *config.Config, project storage.ProjectRecord, askProvider string) string {
	if p := strings.TrimSpace(strings.ToLower(askProvider)); p == "forgejo" || p == "github" {
		return p
	}
	if cfg == nil {
		return "github"
	}
	if binding, ok := runtimeProjectBinding(*cfg, project.ID); ok {
		if config.ResolvedProjectProviderKind(*cfg, binding) == config.ProviderKindForgejo {
			return "forgejo"
		}
	}
	return "github"
}

// clearHITLAwaitingLabel removes the awaiting-human label from a PR after a human
// answer is delivered (poll lane or dashboard /respond). Routes through the
// matching forge client so Forgejo PRs are not left permanently labeled.
// Best-effort: returns the first error but callers typically log and continue.
func clearHITLAwaitingLabel(ctx context.Context, cfg *config.Config, gw *githubinfra.Gateway, repo string, prNumber int64, cwd, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "looper:awaiting-human"
	}
	repo = strings.TrimSpace(repo)
	if repo == "" || prNumber <= 0 {
		return nil
	}
	if client, ok, err := forgeClientForLocation(cfg, repo, []string{cwd}, forgejoClientForCWD, forgejoClientForRepo); ok || err != nil {
		if err != nil {
			return err
		}
		return client.RemoveIssueLabel(ctx, prNumber, label)
	}
	if gw == nil {
		return nil
	}
	return gw.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{
		Repo: repo, PRNumber: prNumber, Labels: []string{label}, CWD: cwd,
	})
}

// listHITLIssueComments lists PR issue comments via the forge that hosts the PR.
func listHITLIssueComments(ctx context.Context, cfg *config.Config, gw *githubinfra.Gateway, provider, repo string, prNumber int64, cwd string) ([]githubAnswerComment, error) {
	if strings.EqualFold(strings.TrimSpace(provider), "forgejo") {
		client, ok, err := forgeClientForLocation(cfg, repo, []string{cwd}, forgejoClientForCWD, forgejoClientForRepo)
		if err != nil {
			return nil, err
		}
		if !ok || client == nil {
			return nil, fmt.Errorf("hitl poll: forgejo client not configured for repo %s", repo)
		}
		cs, err := client.ListIssueComments(ctx, prNumber)
		if err != nil {
			return nil, err
		}
		out := make([]githubAnswerComment, 0, len(cs))
		for _, c := range cs {
			out = append(out, githubAnswerComment{ID: c.ID, Author: c.User.Login, Body: c.Body})
		}
		return out, nil
	}
	if gw == nil {
		return nil, fmt.Errorf("hitl github poll: github gateway is not configured")
	}
	cs, err := gw.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: prNumber, CWD: cwd})
	if err != nil {
		return nil, err
	}
	out := make([]githubAnswerComment, 0, len(cs))
	for _, c := range cs {
		out = append(out, githubAnswerComment{ID: c.ID, Author: c.Author, Body: c.Body})
	}
	return out, nil
}

// ClearHITLAwaitingLabel is the exported best-effort cleanup used by the API
// /respond path so dashboard answers remove the same awaiting-human label the
// poll lane clears after detecting a PR reply.
func ClearHITLAwaitingLabel(ctx context.Context, cfg *config.Config, gw *githubinfra.Gateway, repo string, prNumber int64, cwd, label string) error {
	return clearHITLAwaitingLabel(ctx, cfg, gw, repo, prNumber, cwd, label)
}

// NewHITLGitHubGateway builds a short-lived githubinfra.Gateway for API-side
// label cleanup when the handler does not hold the daemon's long-lived gateway.
func NewHITLGitHubGateway(ghPath, cwd string) *githubinfra.Gateway {
	return githubinfra.New(githubinfra.Options{GHPath: strings.TrimSpace(ghPath), CWD: strings.TrimSpace(cwd)})
}

// runGitHubHITLPoll runs one answer-poll pass for a project's awaiting_human
// loops that carry a PR-comment (transport=github) ask. Gated by hitl.enabled;
// routes list/clear through Forgejo or GitHub.com based on ask.Provider / project
// binding so Forgejo replies are observed.
func runGitHubHITLPoll(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord) {
	if input.Config == nil || !input.Config.HITL.Enabled || input.Repos == nil {
		return
	}
	transport := strings.TrimSpace(strings.ToLower(input.Config.HITL.AnswerTransport))
	if transport != "" && transport != "github" {
		return
	}

	// Need at least one way to talk to a forge: GitHub gateway and/or a Forgejo
	// binding for this project. Forgejo-only daemons must still poll.
	projectProvider := "github"
	if binding, ok := runtimeProjectBinding(*input.Config, project.ID); ok {
		if config.ResolvedProjectProviderKind(*input.Config, binding) == config.ProviderKindForgejo {
			projectProvider = "forgejo"
		}
	}
	if projectProvider != "forgejo" && input.GitHubGateway == nil {
		return
	}

	allLoops, err := input.Repos.Loops.List(ctx)
	if err != nil {
		return
	}
	awaiting := make([]githubHITLAwaitingLoop, 0)
	for _, l := range allLoops {
		if l.ProjectID != project.ID || l.Status != "awaiting_human" {
			continue
		}
		ask, ok := loops.ReadHITLAsk(l.MetadataJSON)
		if !ok || !strings.EqualFold(strings.TrimSpace(ask.Transport), "github") || ask.PRNumber == 0 {
			continue
		}
		repo := ""
		if l.Repo != nil {
			repo = *l.Repo
		}
		provider := hitlPRCommentProvider(input.Config, project, ask.Provider)
		awaiting = append(awaiting, githubHITLAwaitingLoop{
			ID: l.ID, ProjectID: l.ProjectID, Repo: repo,
			Transport: ask.Transport, Provider: provider, AskStatus: ask.Status,
			PRNumber: ask.PRNumber, AskCommentID: ask.AskCommentID,
		})
	}
	if len(awaiting) == 0 {
		return
	}

	awaitingLabel := "looper:awaiting-human"
	var answerAuthors []string
	if gh := input.Config.HITL.GitHub; gh != nil {
		if strings.TrimSpace(gh.AwaitingLabel) != "" {
			awaitingLabel = strings.TrimSpace(gh.AwaitingLabel)
		}
		answerAuthors = gh.AnswerAuthors
	}
	gw := input.GitHubGateway
	nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
	cfg := input.Config

	// Per-loop provider routing: list/clear use the ask's forge host.
	providerByRepoPR := make(map[string]string, len(awaiting))
	for _, loop := range awaiting {
		key := loop.Repo + "#" + strconv.FormatInt(loop.PRNumber, 10)
		providerByRepoPR[key] = loop.Provider
	}

	deps := githubHITLPollDeps{
		listComments: func(ctx contextType, repo string, pr int64, cwd string) ([]githubAnswerComment, error) {
			provider := providerByRepoPR[repo+"#"+strconv.FormatInt(pr, 10)]
			if provider == "" {
				provider = projectProvider
			}
			return listHITLIssueComments(ctx, cfg, gw, provider, repo, pr, cwd)
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoop(ctx, input.Repos, nowISO, loopID, answer)
		},
		clearAwaiting: func(ctx contextType, repo string, pr int64, cwd string) {
			_ = clearHITLAwaitingLabel(ctx, cfg, gw, repo, pr, cwd, awaitingLabel)
		},
		projectCWD:    func(string) string { return project.RepoPath },
		answerAuthors: answerAuthors,
	}
	if input.Logger != nil {
		deps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}

	if n := pollGitHubHITLAnswersOnce(ctx, awaiting, deps); n > 0 && input.Logger != nil {
		input.Logger.Info("hitl github: delivered human answers", map[string]any{"projectId": project.ID, "count": n})
	}
}
