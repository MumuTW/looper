package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/labels"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// hitlSentinelRelPath is where an agent writes a mid-run question, relative to
// the worktree root. Mirrors synclo's afk ask sentinel.
const hitlSentinelRelPath = ".looper/ask.json"

// hitlPromptInstruction is appended to the worker prompt ONLY when hitl.enabled
// is true. It tells the agent how to pause and ask a human instead of guessing.
const hitlPromptInstruction = `

---
HUMAN-IN-THE-LOOP: You are trusted to make implementation decisions yourself. Being ABLE to form a reasonable recommendation is NOT a reason to ask a human — it is a reason to PROCEED. Do your homework (read the codebase and context), pick the best option, and carry on. State what you chose and why in your PR description so a human can course-correct in review — that review IS the checkpoint for reversible calls.

Escalate to a human — by writing a JSON file at .looper/ask.json in the repository root and then STOPPING — ONLY when one of these genuinely holds:
  1. You cannot form a confident recommendation: the options are a real toss-up, or the choice hinges on information only a human has (product intent, private context, an unstated requirement).
  2. The action is high-stakes or hard to reverse: destructive or data-losing, security- or privacy-sensitive, a public or contractual commitment, or it spends real money — a human should sign off even when you have an opinion.
  3. It is a direction or strategy call, not an implementation detail.
For everything else — naming, file contents, formats, defaults, structure, which reversible approach to take — DECIDE and proceed. Do not ask.

When you DO escalate, make it a decision brief the human can confirm in seconds (not raw research):
{
  "question": "<one concise question>",
  "options": ["<option 1>", "<option 2>"],
  "recommendation": "<1-2 sentences: what you found, what you'd do, and why>",
  "recommendedOption": "<the option you recommend, matching one of options>",
  "consequences": {"<option 1>": "<what happens if picked>", "<option 2>": "<what happens if picked>"},
  "confidence": "<high|medium|low>"
}
question + options are required; the rest are strongly encouraged (a bare question with no homework is a poor ask). Then STOP immediately without making further changes. A human will answer and you will be resumed in this same session with their decision.
---`

type hitlAsk struct {
	Question          string            `json:"question"`
	Options           []string          `json:"options"`
	Recommendation    string            `json:"recommendation,omitempty"`
	RecommendedOption string            `json:"recommendedOption,omitempty"`
	Consequences      map[string]string `json:"consequences,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`
}

// hitlSentinelQuarantinePattern names per-event quarantine directories under
// the system temp dir: outside the Git worktree, so auto-commit reconciliation
// can never commit the evidence, and unique per event, so a repeated malformed
// ask cannot clobber earlier preserved evidence.
const hitlSentinelQuarantinePattern = "looper-ask-quarantine-*"

// maxAskSentinelBytes bounds a sentinel read; a decision brief is small, and
// an enormous file is not a decodable gate request.
const maxAskSentinelBytes = 1 << 20

// consumeAskSentinel reads and removes the agent's ask sentinel from the
// worktree, if present. A missing sentinel is the ONLY no-question case:
// the sentinel is the agent's structured request for a human gate, so once
// the file exists, inability to read or decode it fails closed with the
// evidence quarantined for inspection — a truncated ask for a destructive
// decision must not let the worker continue to validation and publication.
// Consuming (deleting) a valid sentinel prevents the same question from
// re-suspending on resume.
func consumeAskSentinel(worktreePath string) (*hitlAsk, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, nil
	}
	path := filepath.Join(worktreePath, hitlSentinelRelPath)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ask sentinel exists but cannot be read: %w", err)
	}
	if info.Size() > maxAskSentinelBytes {
		return nil, quarantineAskSentinel(path, fmt.Errorf("ask sentinel is %d bytes (limit %d)", info.Size(), maxAskSentinelBytes))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ask sentinel exists but cannot be read: %w", err)
	}
	var ask hitlAsk
	if err := json.Unmarshal(raw, &ask); err != nil {
		return nil, quarantineAskSentinel(path, fmt.Errorf("ask sentinel is not valid JSON: %w", err))
	}
	if strings.TrimSpace(ask.Question) == "" {
		return nil, quarantineAskSentinel(path, errors.New("ask sentinel has no question"))
	}
	_ = os.Remove(path)
	return &ask, nil
}

// quarantineAskSentinel preserves undecodable gate evidence in a unique
// directory OUTSIDE the Git worktree and returns an actionable diagnostic.
// In-worktree quarantine would become an untracked change that auto-commit
// reconciliation commits into the branch, and a fixed name would let a
// repeated malformed ask clobber earlier evidence.
func quarantineAskSentinel(path string, cause error) error {
	dir, err := os.MkdirTemp("", hitlSentinelQuarantinePattern)
	if err != nil {
		return fmt.Errorf("%v; quarantine also failed: %v (evidence left at %s)", cause, err, path)
	}
	quarantined := filepath.Join(dir, filepath.Base(path))
	if err := os.Rename(path, quarantined); err != nil {
		// Cross-device or permission failure: fall back to copy-and-remove so
		// the evidence still leaves the consume path.
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("%v; quarantine also failed: %v (evidence left at %s)", cause, err, path)
		}
		if writeErr := os.WriteFile(quarantined, raw, 0o600); writeErr != nil {
			return fmt.Errorf("%v; quarantine also failed: %v (evidence left at %s)", cause, writeErr, path)
		}
		_ = os.Remove(path)
	}
	return fmt.Errorf("%v; evidence quarantined at %s — inspect it, then answer or retry the loop", cause, quarantined)
}

// awaitingHumanError is returned from the execute step when the agent asked a
// human mid-run. The step loop catches it and suspends the loop as
// awaiting_human instead of treating it as a failure.
type awaitingHumanError struct {
	question    string
	options     []string
	sessionID   string
	executionID string
	vendor      string
	// The agent's decision brief (optional) — carried through to the ask card.
	recommendation    string
	recommendedOption string
	consequences      map[string]string
	confidence        string
}

func (e *awaitingHumanError) Error() string { return "worker paused awaiting human decision" }

func asAwaitingHumanError(err error) (*awaitingHumanError, bool) {
	var typed *awaitingHumanError
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

// pendingHumanAnswer returns a resume prompt + native session id when the loop
// carries a human answer to a prior mid-run question. It is READ-ONLY: it does
// NOT mark the answer consumed. That matters because a resumed agent turn can
// fail or time out before completing — leaving the answer "answered" lets the
// retry re-read and re-deliver it instead of silently dropping the human's
// decision. The answer is flipped to "consumed" only once the turn completes,
// via markHumanAnswerConsumed. Returns empty strings when no answer is pending.
// Only called when hitl.enabled is true.
// pendingTakeoverResume returns the native session id (+ a continue prompt) a
// human drove during an interactive takeover that has since been handed back, so
// the daemon's next worker run resumes THAT session and sees their turns. Empty
// when no takeover resume is pending. Independent of hitl.enabled.
func (r *Runner) pendingTakeoverResume(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	tr, ok := loops.ReadTakeoverResume(loop.MetadataJSON)
	if !ok || strings.TrimSpace(tr.SessionID) == "" {
		return "", ""
	}
	prompt := strings.TrimSpace(tr.Prompt)
	if prompt == "" {
		prompt = "A human took this task's agent session over directly and has handed it back. Review the whole conversation so far — including their turns — and continue from where they left off; do not restart from scratch."
	}
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return takeoverCheckpointRestartPrompt(), ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || execution == nil || execution.NativeSessionID == nil || strings.TrimSpace(*execution.NativeSessionID) != strings.TrimSpace(tr.SessionID) || strings.TrimSpace(execution.Vendor) != strings.TrimSpace(agentVendor) {
		return takeoverCheckpointRestartPrompt(), ""
	}
	return prompt, strings.TrimSpace(tr.SessionID)
}

func takeoverCheckpointRestartPrompt() string {
	return "A human took this task over directly and has handed it back. The configured agent vendor no longer matches the captured session, so start a fresh session, inspect the current worktree and task state, and continue without trying to attach to the old conversation."
}

// latestNativeSessionID returns the loop's most recent captured agent session id,
// so a mailbox-driven turn can native-resume the SAME session and have the full
// conversation context. Empty when none is recorded. agentVendor is the run's
// execution-authority vendor (snapshot when present).
func (r *Runner) latestNativeSessionID(ctx context.Context, loopID, agentVendor string) string {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil || execution.NativeSessionID == nil || strings.TrimSpace(execution.Vendor) != strings.TrimSpace(agentVendor) {
		return ""
	}
	return strings.TrimSpace(*execution.NativeSessionID)
}

// acknowledgeHumanInbox removes exactly the drained messages after a successful
// turn so they are not re-injected on a later run, while messages that arrived
// mid-run — never seen by the agent — stay queued. No-op when nothing was
// included in the turn or the inbox is empty.
func (r *Runner) acknowledgeHumanInbox(ctx context.Context, loop *storage.LoopRecord, consumed []loops.HumanMessage) {
	if r.repos == nil || r.repos.Loops == nil || len(consumed) == 0 {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	if len(loops.ReadHumanInbox(fresh.MetadataJSON)) == 0 {
		return
	}
	meta, werr := loops.AcknowledgeHumanMessages(fresh.MetadataJSON, consumed)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

// markTakeoverResumeConsumed clears the takeover-resume marker after a successful
// resumed turn so it is not re-applied on later runs. No-op when absent.
func (r *Runner) markTakeoverResumeConsumed(ctx context.Context, loop *storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	if _, ok := loops.ReadTakeoverResume(fresh.MetadataJSON); !ok {
		return
	}
	meta, werr := loops.ClearTakeoverResume(fresh.MetadataJSON)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

func (r *Runner) pendingHumanAnswer(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return "", ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	resumePrompt := fmt.Sprintf("A human answered the question you asked earlier (%q). Their decision: %s\nContinue the task using this decision; do not ask the same question again.", ask.Question, ask.Answer)
	if strings.TrimSpace(ask.Vendor) != strings.TrimSpace(agentVendor) {
		return resumePrompt + "\nThe configured agent vendor changed after the question was asked, so continue in a fresh session rather than trying to attach to the prior vendor's session.", ""
	}
	return resumePrompt, strings.TrimSpace(ask.SessionID)
}

// markHumanAnswerConsumed flips a delivered human answer from "answered" to
// "consumed" so it is not re-injected on any later run of the same loop. It is
// called only after the resumed agent turn completes successfully. No-op when
// there is no answered ask. Persists against the freshest loop record so it does
// not clobber concurrent metadata writes. Only called when hitl.enabled is true.
func (r *Runner) markHumanAnswerConsumed(ctx context.Context, loop *storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return
	}
	ask.Status = "consumed"
	meta, werr := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

// readFreshHITLAsk reads the loop's HITL ask from the freshest persisted record,
// falling back to the in-memory copy when the store is unavailable.
func (r *Runner) readFreshHITLAsk(ctx context.Context, loop *storage.LoopRecord) (loops.HITLAsk, bool) {
	meta := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil {
		if got, err := r.repos.Loops.GetByID(ctx, loop.ID); err == nil && got != nil {
			meta = got.MetadataJSON
		}
	}
	return loops.ReadHITLAsk(meta)
}

// detectHumanAsk consumes the agent's ask sentinel (if any) and, when present,
// returns a typed awaitingHumanError carrying the question, options, and the
// agent's native session id (so the run can resume the same session).
func (r *Runner) detectHumanAsk(ctx context.Context, input stepInput, worktreePath, executionID string) (*awaitingHumanError, error) {
	ask, err := consumeAskSentinel(worktreePath)
	if err != nil {
		// Fail closed AND parked: the sentinel exists, so the agent requested
		// a human gate. A plain error would classify as retryable-transient,
		// auto-requeue, and the retry — finding the sentinel quarantined —
		// would proceed to publication, exactly the bypass the gate exists to
		// prevent. Manual intervention keeps the loop stopped until a human
		// inspects the quarantined evidence.
		return nil, &loopError{message: fmt.Sprintf("HITL ask sentinel failure for loop %s: %v", input.Loop.ID, err), kind: FailureManualIntervention}
	}
	if ask == nil {
		return nil, nil
	}
	sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
	return &awaitingHumanError{
		question:          ask.Question,
		options:           ask.Options,
		sessionID:         sessionID,
		executionID:       executionID,
		vendor:            vendor,
		recommendation:    ask.Recommendation,
		recommendedOption: ask.RecommendedOption,
		consequences:      ask.Consequences,
		confidence:        ask.Confidence,
	}, nil
}

func (r *Runner) latestAgentSession(ctx context.Context, loopID string) (string, string) {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return "", ""
	}
	rec, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || rec == nil {
		return "", ""
	}
	sessionID := ""
	if rec.NativeSessionID != nil {
		sessionID = strings.TrimSpace(*rec.NativeSessionID)
	}
	return sessionID, rec.Vendor
}

// suspendForHuman parks a worker run as awaiting_human: it persists the ask
// state on the loop, transitions the loop to awaiting_human, cancels the claimed
// queue item (so /respond can requeue it), ends the run as interrupted
// (resumable from the checkpoint), and sends the ask-card. Only reached when
// hitl.enabled is true.
// abortSuspension unwinds a suspension that could not persist its ask: the
// run is ended as failed (createRunContext resumes failed/interrupted runs;
// a run left running would make the retried claim start a duplicate from
// prepare-work while the stale run stays active) and the cause is surfaced.
func (r *Runner) abortSuspension(ctx context.Context, run storage.RunRecord, checkpoint workerCheckpoint, cause error) (ProcessResult, error) {
	wrapped := fmt.Errorf("persist HITL ask before suspension: %w", cause)
	if _, err := r.completeRun(ctx, run, "failed", "", wrapped.Error(), checkpoint); err != nil {
		return ProcessResult{}, errors.Join(wrapped, err)
	}
	return ProcessResult{}, wrapped
}

func (r *Runner) suspendForHuman(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint workerCheckpoint, awaiting *awaitingHumanError) (ProcessResult, error) {
	nowISO := r.nowISO()
	ask := loops.HITLAsk{
		Question:          awaiting.question,
		Options:           awaiting.options,
		SessionID:         awaiting.sessionID,
		ExecutionID:       awaiting.executionID,
		Vendor:            awaiting.vendor,
		Status:            "awaiting",
		AskedAt:           nowISO,
		Recommendation:    awaiting.recommendation,
		RecommendedOption: awaiting.recommendedOption,
		Consequences:      awaiting.consequences,
		Confidence:        awaiting.confidence,
	}
	// Preflight the strict metadata decode before ANY GitHub side effects: a
	// malformed value would otherwise let deliverAskToGitHub publish a branch,
	// draft PR, question comment, and label whose ask can never be stored —
	// and queue retries would keep publishing unanswerable comments.
	if _, err := loops.DecodeMetadataObjectForWrite(input.Loop.MetadataJSON); err != nil {
		return r.abortSuspension(ctx, run, checkpoint, err)
	}
	// GitHub transport (default): post the question on a (draft) PR before parking,
	// so the ask metadata carries the PR + comment id the answer-poll lane needs.
	// Best-effort — the loop still parks awaiting_human if delivery fails.
	if r.hitlTransportGitHub() {
		if err := r.deliverAskToGitHub(ctx, input, &checkpoint, awaiting, &ask); err != nil && r.logger != nil {
			r.logger.Warn("worker HITL github ask delivery failed; loop parked awaiting human without a PR comment", map[string]any{
				"loopId": input.Loop.ID, "error": err.Error(),
			})
		}
	}
	var askWriteErr error
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask)
		if werr != nil {
			// Without a stored ask the response paths can never resume this
			// loop; abort the whole suspension instead of parking it stranded.
			askWriteErr = werr
			return
		}
		// The agent re-asked after reading the queued human messages, so the
		// ones it actually saw are consumed — but a message that arrived while
		// it was running was never observed and stays queued for the resumed
		// turn.
		if meta, werr := loops.AcknowledgeHumanMessages(&meta, loops.ReadHumanInbox(input.Loop.MetadataJSON)); werr == nil {
			updated.MetadataJSON = &meta
		}
		updated.Status = "awaiting_human"
		updated.LastRunAt = stringPtr(nowISO)
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	if askWriteErr != nil {
		return r.abortSuspension(ctx, run, checkpoint, askWriteErr)
	}
	reason := "worker suspended awaiting human decision"
	if _, err := r.repos.Queue.CancelByLoop(ctx, input.Loop.ID, nowISO, &reason); err != nil {
		return ProcessResult{}, err
	}
	summary := "Awaiting human decision: " + awaiting.question
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	if !r.hitlTransportGitHub() && r.hitlNotify != nil {
		notif := HITLAskNotification{
			ProjectID:         input.Project.ID,
			LoopID:            input.Loop.ID,
			LoopSeq:           input.Loop.Seq,
			RunID:             run.ID,
			Repo:              derefString(input.Loop.Repo),
			Title:             awaiting.question,
			Question:          awaiting.question,
			Options:           awaiting.options,
			Recommendation:    awaiting.recommendation,
			RecommendedOption: awaiting.recommendedOption,
			Consequences:      awaiting.consequences,
			Confidence:        awaiting.confidence,
		}
		// Source + trigger come from the loop's work metadata (issue #, url, author).
		if w := checkpoint.Work; w != nil {
			notif.TriggerLogin = w.TriggerLogin
			switch {
			case w.PRNumber > 0:
				notif.SourceType = "GitHub PR"
				notif.SourceRef = "#" + strconv.FormatInt(w.PRNumber, 10)
			case w.IssueNumber > 0:
				notif.SourceType = "GitHub Issue"
				notif.SourceRef = "#" + strconv.FormatInt(w.IssueNumber, 10)
				notif.SourceURL = w.IssueURL
			}
		}
		if err := r.hitlNotify(ctx, notif); err != nil && r.logger != nil {
			// The loop is already parked in awaiting_human; if the human is never
			// notified they must find it via the dashboard / API. Surface loudly so an
			// unconfigured or failing notifier can't silently strand a run.
			r.logger.Warn("worker HITL ask notification failed; loop parked awaiting human with no notification sent", map[string]any{
				"loopId": input.Loop.ID, "loopSeq": input.Loop.Seq, "runId": run.ID, "error": err.Error(),
			})
		}
	}
	return ProcessResult{LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID, Status: "awaiting_human", Summary: summary}, nil
}

// hitlTransportGitHub reports whether the GitHub PR-comment ask transport is
// active. GitHub is the default when hitl is enabled and no transport is set.
func (r *Runner) hitlTransportGitHub() bool {
	t := strings.TrimSpace(strings.ToLower(r.hitlAnswerTransport))
	return t == "" || t == "github"
}

func (r *Runner) hitlAwaitingLabel() string {
	if l := strings.TrimSpace(r.hitlGitHub.AwaitingLabel); l != "" {
		return l
	}
	return labels.AwaitingHuman
}

// deliverAskToGitHub ensures a (draft) PR exists for the loop, posts the agent's
// question as a marked PR comment, labels the PR so the answer-poll lane finds
// it, and records the PR + comment id on the ask. Best-effort; returns an error
// the caller logs while still parking the loop awaiting_human.
func (r *Runner) deliverAskToGitHub(ctx context.Context, input stepInput, checkpoint *workerCheckpoint, awaiting *awaitingHumanError, ask *loops.HITLAsk) error {
	if checkpoint == nil {
		return fmt.Errorf("hitl github: worker checkpoint is required")
	}
	repo := derefString(input.Loop.Repo)
	if repo == "" && checkpoint.Work != nil {
		repo = checkpoint.Work.Repo
	}
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("hitl github: no repo for loop %s", input.Loop.ID)
	}
	cwd := input.Project.RepoPath

	prNumber := int64(0)
	if checkpoint.PullRequest != nil && checkpoint.PullRequest.Number > 0 {
		prNumber = checkpoint.PullRequest.Number
	} else if input.Loop.PRNumber != nil && *input.Loop.PRNumber > 0 {
		prNumber = *input.Loop.PRNumber
	}
	// On a later ask (e.g. a multi-turn second question) the loop/checkpoint may not
	// carry the PR that an earlier ask already opened for this branch — find and
	// reuse it instead of trying to open a duplicate (which gh rejects).
	if prNumber == 0 && checkpoint.Worktree != nil && strings.TrimSpace(checkpoint.Worktree.Branch) != "" {
		base := strings.TrimSpace(checkpoint.Worktree.BaseBranch)
		var aliases []string
		if checkpoint.Work != nil {
			if base == "" {
				base = strings.TrimSpace(checkpoint.Work.BaseBranch)
			}
			aliases = buildWorkerBranchAliases(*checkpoint.Work, input.Loop.ID)
		}
		aliases = append(aliases, checkpoint.Worktree.Branch)
		if base == "" {
			base = "main"
		}
		if existing, err := r.findOpenPullRequestForBranch(ctx, repo, aliases, base, cwd); err == nil && existing != nil {
			prNumber = existing.Number
		}
	}
	if prNumber == 0 {
		created, err := r.ensureDraftPRForAsk(ctx, input, checkpoint, repo, cwd)
		if err != nil {
			return err
		}
		prNumber = created
	}
	// Persist the PR onto the loop so later asks + the answer-poll resolve it fast.
	if prNumber > 0 && (input.Loop.PRNumber == nil || *input.Loop.PRNumber != prNumber) {
		_, _ = r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
			updated.PRNumber = int64Ptr(prNumber)
		})
	}
	if prNumber == 0 {
		return fmt.Errorf("hitl github: could not resolve a PR for loop %s", input.Loop.ID)
	}

	body := buildGitHubAskComment(input.Loop.Seq, awaiting.question, awaiting.options, r.hitlGitHub.MentionLogins)
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	res, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel})
	if err != nil {
		return err
	}
	if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{r.hitlAwaitingLabel()}, CWD: cwd}); err != nil && r.logger != nil {
		r.logger.Warn("hitl github: failed to add awaiting-human label", map[string]any{"repo": repo, "pr": prNumber, "error": err.Error()})
	}

	ask.Transport = "github"
	ask.PRNumber = prNumber
	ask.AskCommentID = res.ID
	return nil
}

// ensureDraftPRForAsk pushes the loop's WIP branch and opens a draft PR to carry
// the question. Requires committed WIP on the branch; returns an error when there
// is nothing to open a PR from.
func (r *Runner) ensureDraftPRForAsk(ctx context.Context, input stepInput, checkpoint *workerCheckpoint, repo, cwd string) (int64, error) {
	if checkpoint == nil || checkpoint.Worktree == nil || strings.TrimSpace(checkpoint.Worktree.Branch) == "" {
		return 0, fmt.Errorf("hitl github: no worktree branch to open a draft PR for loop %s", input.Loop.ID)
	}
	base := strings.TrimSpace(checkpoint.Worktree.BaseBranch)
	title := ""
	if checkpoint.Work != nil {
		if base == "" {
			base = strings.TrimSpace(checkpoint.Work.BaseBranch)
		}
		title = strings.TrimSpace(checkpoint.Work.Title)
	}
	if base == "" {
		base = "main"
	}
	if title == "" {
		title = "Looper WIP — awaiting a human decision"
	}
	worktreeRoot, err := workerWorktreeRoot(input.Project)
	if err != nil {
		return 0, err
	}
	if len(r.validationCommands) > 0 {
		validateInput := input
		validateInput.Checkpoint = *checkpoint
		validated, err := r.runValidateStep(ctx, validateInput)
		if err != nil {
			return 0, fmt.Errorf("hitl github: validation blocked draft publication: %w", err)
		}
		*checkpoint = validated
	}
	if err := r.git.Push(ctx, PushInput{RepoPath: cwd, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, LocalHeadSHA: workerValidatedHeadSHA(*checkpoint, r.validationCommands), ProtectedBranches: compactStrings([]string{base})}); err != nil {
		return 0, err
	}
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	created, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{
		Repo:            repo,
		HeadBranch:      checkpoint.Worktree.Branch,
		BaseBranch:      base,
		Title:           title,
		Body:            "🚧 Draft opened by looper to ask a mid-run question — see the comment below. Not ready for review.",
		Draft:           true,
		CWD:             cwd,
		DisclosureAgent: disclosureAgent,
		DisclosureModel: disclosureModel,
	})
	if err != nil {
		return 0, err
	}
	return created.Number, nil
}

const hitlGitHubAskMarkerPrefix = "<!-- looper:hitl:ask v=1"

// buildGitHubAskComment renders the ask as a PR comment carrying a machine marker
// (so the poll lane finds it and never mistakes it for a human answer).
func buildGitHubAskComment(loopSeq int64, question string, options []string, mentionLogins []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s loop=%d -->\n", hitlGitHubAskMarkerPrefix, loopSeq)
	b.WriteString("🤔 **looper needs a decision to continue.**\n\n")
	b.WriteString(strings.TrimSpace(question))
	for _, o := range options {
		if o = strings.TrimSpace(o); o != "" {
			fmt.Fprintf(&b, "\n- %s", o)
		}
	}
	b.WriteString("\n\nReply to this comment with your choice — a letter, an option, or free-form guidance. I'll pick it up and continue on this PR.")
	if m := githubMentionLine(mentionLogins); m != "" {
		b.WriteString("\n\n" + m)
	}
	return b.String()
}

func githubMentionLine(logins []string) string {
	parts := make([]string, 0, len(logins))
	for _, l := range logins {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "@")); l != "" {
			parts = append(parts, "@"+l)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "/cc " + strings.Join(parts, " ")
}
