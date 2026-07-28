package fixer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/hitl"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// hitlPromptInstruction is appended to the fixer repair prompt ONLY when
// hitl.enabled is true. Authority order + conflict escalation — not generic
// "when unsure, ask" guidance.
const hitlPromptInstruction = `

---
AUTHORITY ORDER (highest wins):
  1. latest explicit human instruction
  2. repo AGENTS.md / documented project rules (explicit files only — do not invent "norms")
  3. PR explicit goal / design intent (title + body)
  4. reviewer suggestion
  5. agent judgment

You must NOT blindly obey reviewers. When a reviewer request conflicts with higher authority and the conflict is truly unresolvable by you, escalate to a human instead of implementing the wrong change.

Escalate — by writing JSON at .looper/ask.json in the repository root AND/OR emitting action "needs_human" on the relevant review_thread_replies / repair_results entry, then STOPPING — ONLY when one of these genuinely holds:
  1. Reviewer request conflicts with PR title/body or the latest explicit human instruction, and you cannot reconcile them from public evidence.
  2. Implementing the request would undo an intentional product/design choice already established on the PR.
  3. A→B→A thrash or a second reopen of the same decision.
  4. The choice needs private product intent only a human has.
  5. High-risk / hard-to-reverse action (security, data loss, public commitment) that needs human sign-off even when you have an opinion.
Do NOT escalate merely for low confidence.
- Demonstrably wrong / incorrect reviewer request with clear public evidence → action "declined" with concrete evidence (and dismiss.json only when appropriate).
- High-risk needs human sign-off → escalate (needs_human / ask.json), do not silently proceed.
Ordinary tech questions → investigate and decide.

When you DO escalate, make it a decision brief the human can confirm in seconds:
{
  "question": "<one concise question naming the conflict>",
  "options": ["<concrete action 1>", "<concrete action 2>"],
  "recommendation": "<1-2 sentences: what you found, what you'd do, and why>",
  "recommendedOption": "<the option you recommend, matching one of options>",
  "consequences": {"<option 1>": "<what happens if picked>", "<option 2>": "<what happens if picked>"},
  "confidence": "<high|medium|low>"
}
question + options are required (at least one non-empty option); use concrete action labels (not bare Accept/Reject). Then STOP immediately without pushing, resolving threads, writing dismissals you want held, or making further remote changes. A human will answer and you will be resumed with their decision.
You may also set "action": "needs_human" on review_thread_replies / repair_results with an explanation that becomes part of the brief.
---`

// HITLAskNotification is the payload handed to HITLNotify when Fixer pauses
// mid-run to ask a human. Field shape matches worker so the scheduler can adapt
// either role onto notify.HITLAskCard.
type HITLAskNotification struct {
	ProjectID         string
	LoopID            string
	LoopSeq           int64
	RunID             string
	Repo              string
	Title             string
	Question          string
	Options           []string
	SourceType        string
	SourceRef         string
	SourceURL         string
	TriggerLogin      string
	Recommendation    string
	RecommendedOption string
	Consequences      map[string]string
	Confidence        string
	// NotifyOnly suppresses interactive Feishu answer buttons. Used when
	// answerTransport is github so answers arrive via PR comment / dashboard and
	// GitHub awaiting-label cleanup still runs.
	NotifyOnly bool
}

// HITLNotifyFunc delivers a mid-run ask to the human channel. Best-effort.
type HITLNotifyFunc func(context.Context, HITLAskNotification) error

// HITLGitHubSettings tunes the GitHub PR-comment ask transport.
type HITLGitHubSettings struct {
	AwaitingLabel string
	MentionLogins []string
}

// awaitingHumanError is returned from the repair step when the agent needs a
// human decision. The step loop catches it and suspends as awaiting_human.
type awaitingHumanError struct {
	question          string
	options           []string
	sessionID         string
	executionID       string
	vendor            string
	recommendation    string
	recommendedOption string
	consequences      map[string]string
	confidence        string
	headSHA           string
	reviewThreadID    string
	reviewCommentID   string
	reviewContentFP   string
	prIntentFP        string
	worktreePath      string
}

func (e *awaitingHumanError) Error() string { return "fixer paused awaiting human decision" }

func asAwaitingHumanError(err error) (*awaitingHumanError, bool) {
	var typed *awaitingHumanError
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

func (r *Runner) pendingHumanAnswer(ctx context.Context, loop *storage.LoopRecord, agentVendor, liveHeadSHA, liveReviewFP, liveIntentFP string) (string, string) {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return "", ""
	}
	// Drift detection: stale head / review / PR-intent fingerprints invalidate
	// the prior answer — do not execute it.
	if !hitl.MaterialFingerprintsMatch(ask.HeadSHA, liveHeadSHA, ask.ReviewContentFingerprint, liveReviewFP, ask.PRIntentFingerprint, liveIntentFP) {
		if r.logger != nil {
			r.logger.Warn("fixer HITL answer invalidated by fingerprint drift; not injecting decision", map[string]any{
				"loopId": loop.ID, "storedHead": ask.HeadSHA, "liveHead": liveHeadSHA,
			})
		}
		return "", ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	resumePrompt := fmt.Sprintf("A human answered the conflict question you asked earlier (%q). Their decision: %s\nContinue the repair using this decision; do not ask the same question again unless new evidence creates a new unresolvable conflict.", ask.Question, ask.Answer)
	if strings.TrimSpace(ask.Vendor) != strings.TrimSpace(agentVendor) {
		return resumePrompt + "\nThe configured agent vendor changed after the question was asked, so continue in a fresh session rather than trying to attach to the prior vendor's session.", ""
	}
	return resumePrompt, strings.TrimSpace(ask.SessionID)
}

// markHITLAnswerResumeStarted records that this execution is injecting the parked
// human answer. Later same-text ask.json files are treated as a new generation.
func (r *Runner) markHITLAnswerResumeStarted(ctx context.Context, loop *storage.LoopRecord, executionID string) error {
	if r.repos == nil || r.repos.Loops == nil {
		return fmt.Errorf("loops repository unavailable")
	}
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return nil
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return fmt.Errorf("loop not found: %s", loop.ID)
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return nil
	}
	if strings.TrimSpace(ask.ResumeExecutionID) == executionID {
		return nil
	}
	ask.ResumeExecutionID = executionID
	meta, werr := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if werr != nil {
		return werr
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err != nil {
		return err
	}
	loop.MetadataJSON = &meta
	return nil
}

func (r *Runner) markHumanAnswerConsumed(ctx context.Context, loop *storage.LoopRecord) error {
	if r.repos == nil || r.repos.Loops == nil {
		return fmt.Errorf("loops repository unavailable")
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return fmt.Errorf("loop not found: %s", loop.ID)
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return nil
	}
	ask.Status = "consumed"
	ask.ResumeExecutionID = ""
	meta, werr := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if werr != nil {
		return werr
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err != nil {
		return err
	}
	loop.MetadataJSON = &meta
	return nil
}

func (r *Runner) readFreshHITLAsk(ctx context.Context, loop *storage.LoopRecord) (loops.HITLAsk, bool) {
	meta := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil {
		if got, err := r.repos.Loops.GetByID(ctx, loop.ID); err == nil && got != nil {
			meta = got.MetadataJSON
		}
	}
	return loops.ReadHITLAsk(meta)
}

// refreshLiveHITLDetail fetches live PR state for fingerprint checks. Fail closed
// on provider error so a stale checkpoint head is never treated as live.
func (r *Runner) refreshLiveHITLDetail(ctx context.Context, input stepInput) (*checkpointDetail, error) {
	if r.github == nil {
		return nil, fmt.Errorf("github gateway unavailable for HITL live refresh")
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{
		Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath,
	})
	if err != nil {
		return nil, fmt.Errorf("HITL live PR refresh failed: %w", err)
	}
	return pullRequestCheckpointDetail(detail), nil
}

// missingLiveReviewMarker is substituted when a review item cannot be found live
// so the fingerprint diverges from ask-time content (invalidate the answer).
const missingLiveReviewMarker = "__hitl_missing_review__"

// liveReviewContentFingerprint rebuilds the review-content fingerprint from live
// provider content (not the pre-suspend checkpoint snapshot alone).
//
// Field layout mirrors collect-time normalizeFixItems:
//   - GitHub review threads: text lives in Summary, Body is empty; ThreadFingerprint
//     covers the full non-Looper reply chain (id@updatedAt|...)
//   - Native Forgejo comments: text lives in both Summary and Body; ObservedFingerprint
//     becomes ThreadFingerprint
//   - Forgejo reviewer-summary items: re-parsed from live PR issue comments
//
// Missing/deleted threads clear to missingLiveReviewMarker (do not keep stale
// checkpoint text). Provider/auth/rate-limit errors fail closed (returned), while
// true not-found results mark the review missing. Unknown item sources fail closed.
func (r *Runner) liveReviewContentFingerprint(ctx context.Context, input stepInput, fixItems []FixItem) (string, error) {
	if r.github == nil {
		return "", fmt.Errorf("github gateway unavailable for HITL review refresh")
	}
	var nativeByProvider map[int64]NativeReviewComment
	var summaryByID map[string]FixItem
	needNative := false
	needSummary := false
	for _, item := range fixItems {
		src := strings.TrimSpace(item.Source)
		if src == NativeReviewCommentSource && item.ProviderCommentID > 0 {
			needNative = true
		}
		if src == "forgejo-reviewer-summary" {
			needSummary = true
		}
	}
	if needNative {
		comments, err := r.github.ListNativeReviewComments(ctx, ListNativeReviewCommentsInput{
			Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath,
		})
		if err != nil {
			return "", fmt.Errorf("HITL live native review refresh failed: %w", err)
		}
		nativeByProvider = make(map[int64]NativeReviewComment, len(comments))
		for _, c := range comments {
			nativeByProvider[c.ProviderCommentID] = c
		}
	}
	if needSummary {
		// Re-parse open forgejo-reviewer-summary items from live PR issue comments
		// so a human answer can resume after summary-sourced needs_human escalation.
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{
			Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath,
		})
		if err != nil {
			return "", fmt.Errorf("HITL live forgejo reviewer-summary refresh failed: %w", err)
		}
		summaryByID = make(map[string]FixItem)
		for _, liveItem := range collectFixItems(detail) {
			if strings.TrimSpace(liveItem.Source) != "forgejo-reviewer-summary" {
				continue
			}
			if id := strings.TrimSpace(liveItem.ID); id != "" {
				summaryByID[id] = liveItem
			}
		}
	}

	liveItems := make([]FixItem, 0, len(fixItems))
	for _, item := range fixItems {
		live := item
		src := strings.TrimSpace(item.Source)
		switch {
		case src == NativeReviewCommentSource && item.ProviderCommentID > 0:
			// Forgejo native: ListNativeReviewComments only (no ViewReviewThread).
			if c, ok := nativeByProvider[item.ProviderCommentID]; ok {
				if text := strings.TrimSpace(c.Body); text != "" {
					live.Summary = text
					live.Body = text
					// ObservedFingerprint tracks comment identity+updatedAt drift.
					if fp := strings.TrimSpace(c.ObservedFingerprint); fp != "" {
						live.ThreadFingerprint = fp
						live.ObservedFingerprint = fp
					}
				} else {
					markLiveReviewMissing(&live)
				}
			} else {
				markLiveReviewMissing(&live)
			}
		case src == "forgejo-reviewer-summary":
			if liveItem, ok := summaryByID[strings.TrimSpace(item.ID)]; ok {
				live.Summary = liveItem.Summary
				live.Body = liveItem.Body
				live.ThreadFingerprint = liveItem.ThreadFingerprint
				live.Files = cloneStrings(liveItem.Files)
				live.Path = liveItem.Path
			} else {
				// Item closed/removed from the live summary → invalidate answer.
				markLiveReviewMissing(&live)
			}
		case src != "" && src != NativeReviewCommentSource:
			return "", fmt.Errorf("HITL cannot live-verify fix item source %q (id=%s)", src, item.ID)
		case strings.TrimSpace(item.ThreadID) != "":
			// GitHub GraphQL review threads.
			thread, err := r.github.ViewReviewThread(ctx, ViewReviewThreadInput{
				ThreadID: item.ThreadID, CWD: input.Project.RepoPath,
			})
			if err != nil {
				// Only true not-found marks the review missing. Auth, rate-limit,
				// and other provider errors must fail closed so a human answer is
				// not injected against an unverifiable thread.
				if isLiveReviewNotFound(err) {
					markLiveReviewMissing(&live)
				} else {
					return "", fmt.Errorf("HITL live review thread refresh failed for %s: %w", item.ThreadID, err)
				}
			} else if body := primaryReviewThreadBody(thread, item); body != "" {
				// GitHub collect-time layout: Summary holds body text, Body empty.
				live.Summary = body
				live.Body = ""
				// Full non-Looper reply chain so mid-park reply add/edit drifts FP.
				live.ThreadFingerprint = liveReviewThreadFingerprint(thread)
			} else {
				markLiveReviewMissing(&live)
			}
		default:
			// Comment-type item without thread/native identity — cannot verify.
			if item.Type == "comment" {
				return "", fmt.Errorf("HITL cannot live-verify comment fix item %q (no threadId/providerCommentId)", item.ID)
			}
			// Non-comment items (checks/conflicts) keep checkpoint fields.
		}
		liveItems = append(liveItems, live)
	}
	return computeReviewContentFingerprint(liveItems), nil
}

func markLiveReviewMissing(item *FixItem) {
	if item == nil {
		return
	}
	item.Summary = missingLiveReviewMarker
	item.Body = missingLiveReviewMarker
	// Clear thread identity so a missing live review cannot match ask-time FP via
	// a still-present ThreadFingerprint alone.
	item.ThreadFingerprint = missingLiveReviewMarker
}

// isLiveReviewNotFound reports whether a ViewReviewThread failure means the
// thread is gone (safe to mark missing) versus a transient/auth failure that
// must fail closed.
func isLiveReviewNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound *githubinfra.ReviewThreadNotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	return githubinfra.IsNotFoundError(err)
}

// liveReviewThreadFingerprint mirrors collect-time reviewThreadFingerprintFromNodes:
// non-Looper-fixer comments as id@updatedAt joined by "|". Exclusion uses the same
// looper-fixer-reply / looper:fixer-round body markers as collect-time (including
// declined replies). Mid-park human/reviewer reply add/edit changes this value so
// resume-time fingerprints diverge from ask-time.
func liveReviewThreadFingerprint(thread ReviewThread) string {
	parts := make([]string, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		if isLooperFixerReplyComment(comment) {
			continue
		}
		id := strings.TrimSpace(comment.ID)
		updatedAt := strings.TrimSpace(comment.UpdatedAt)
		if id == "" && updatedAt == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s@%s", id, updatedAt))
	}
	return strings.Join(parts, "|")
}

func primaryReviewThreadBody(thread ReviewThread, item FixItem) string {
	if len(thread.Comments) == 0 {
		return ""
	}
	wantID := strings.TrimSpace(item.ID)
	// When the fix item targets a specific comment, only that comment counts as
	// live content. Falling back to another comment would let a deleted target
	// falsely match via a sibling reply.
	if wantID != "" {
		for _, c := range thread.Comments {
			if strings.TrimSpace(c.ID) == wantID {
				return strings.TrimSpace(c.Body)
			}
		}
		return ""
	}
	// No target id: use the first non-empty comment body (typically the root).
	for _, c := range thread.Comments {
		if b := strings.TrimSpace(c.Body); b != "" {
			return b
		}
	}
	return ""
}

// mergeLiveHITLDetailOntoCheckpoint copies live authority fields (title/body/head)
// onto the checkpoint detail without discarding unrelated checkpoint state.
func mergeLiveHITLDetailOntoCheckpoint(checkpoint *fixerCheckpoint, live *checkpointDetail) {
	if checkpoint == nil || live == nil {
		return
	}
	if checkpoint.Detail == nil {
		checkpoint.Detail = live
		return
	}
	checkpoint.Detail.Title = live.Title
	checkpoint.Detail.Body = live.Body
	checkpoint.Detail.HeadSHA = live.HeadSHA
	checkpoint.Detail.BaseSHA = live.BaseSHA
	checkpoint.Detail.BaseRefName = live.BaseRefName
	checkpoint.Detail.HeadRefName = live.HeadRefName
	checkpoint.Detail.State = live.State
}

// worktreeHeadMatchesLive reports whether the prepared worktree head still matches
// the live PR head. Empty sides are treated as unknown (not a match failure here;
// callers that require a known head should check separately).
func worktreeHeadMatchesLive(worktreeHead, liveHead string) bool {
	w := strings.TrimSpace(worktreeHead)
	l := strings.TrimSpace(liveHead)
	if w == "" || l == "" {
		return true
	}
	return w == l
}

// hasAnsweredHITLAsk reports whether the loop carries an unanswered-consumed
// human decision that resume may try to inject.
func (r *Runner) hasAnsweredHITLAsk(ctx context.Context, loop *storage.LoopRecord) bool {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	return ok && ask.Status == "answered" && strings.TrimSpace(ask.Answer) != ""
}

// durableHITLParkForAsk reports whether the loop's durable HITL record is an
// active park for the same ask generation as the on-disk ask.json. A historical
// "consumed" park must NOT authorize deleting a new ask.json — that would drop
// an undurable new escalation on the same loop. An "answered" park that already
// started resume (ResumeExecutionID set) also must not match: the agent may
// have written a same-text re-escalation after the human answered.
func (r *Runner) durableHITLParkForAsk(ctx context.Context, loop *storage.LoopRecord, fileAsk *hitl.AskPayload) bool {
	if fileAsk == nil {
		return false
	}
	parked, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok {
		return false
	}
	status := strings.TrimSpace(parked.Status)
	if status != "awaiting" && status != "answered" {
		return false
	}
	parkedQ := strings.TrimSpace(parked.Question)
	fileQ := strings.TrimSpace(fileAsk.Question)
	if parkedQ == "" || fileQ == "" || parkedQ != fileQ {
		return false
	}
	// Same question alone is not enough once a resume turn bound the answer to
	// an execution — treat the on-disk file as a new generation.
	if status == "answered" && strings.TrimSpace(parked.ResumeExecutionID) != "" {
		return false
	}
	// When the sentinel carries an execution identity, require it to match the
	// parked ask generation (prevents same-text re-asks with explicit ids).
	if fileExec := strings.TrimSpace(fileAsk.ExecutionID); fileExec != "" {
		if parkedExec := strings.TrimSpace(parked.ExecutionID); parkedExec != "" && fileExec != parkedExec {
			return false
		}
	}
	return true
}

// awaitingFromAskPayload builds a suspend error from a worktree ask.json without
// requiring structured needs_human replies (undurable-suspend recovery).
func (r *Runner) awaitingFromAskPayload(ctx context.Context, input stepInput, worktreePath, executionID string, ask *hitl.AskPayload) *awaitingHumanError {
	sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
	out := &awaitingHumanError{
		sessionID:       sessionID,
		executionID:     executionID,
		vendor:          vendor,
		headSHA:         detailHeadSHA(input.Checkpoint.Detail),
		reviewContentFP: computeReviewContentFingerprint(input.Checkpoint.FixItems),
		prIntentFP:      computePRIntentFingerprint(input.Checkpoint.Detail),
		worktreePath:    worktreePath,
	}
	if ask != nil {
		out.question = ask.Question
		out.options = ask.Options
		out.recommendation = ask.Recommendation
		out.recommendedOption = ask.RecommendedOption
		out.consequences = ask.Consequences
		out.confidence = ask.Confidence
	}
	if strings.TrimSpace(out.question) == "" {
		out.question = fmt.Sprintf("Fixer needs a decision on %s#%d (unresolvable reviewer conflict).", input.Repo, input.PRNumber)
	}
	if len(compactOptions(out.options)) == 0 {
		out.options = defaultConflictOptions()
	}
	return out
}

// detectHumanAsk reads the ask sentinel (without deleting) and/or builds a brief
// from needs_human replies. Returns a typed awaitingHumanError when the run must
// suspend. Malformed/unreadable sentinels fail closed.
func (r *Runner) detectHumanAsk(ctx context.Context, input stepInput, worktreePath, executionID string, replies []replyExplanationEntry) (*awaitingHumanError, error) {
	if !r.hitlEnabled {
		return nil, nil
	}
	ask, err := hitl.ReadAskSentinel(worktreePath)
	if err != nil {
		return nil, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	needsHumanReplies := filterNeedsHumanReplies(replies)
	if ask == nil && len(needsHumanReplies) == 0 {
		return nil, nil
	}

	sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
	headSHA := detailHeadSHA(input.Checkpoint.Detail)
	reviewThreadID, reviewCommentID := reviewIDsFromNeedsHuman(needsHumanReplies, input.Checkpoint.FixItems)
	// Canonical fingerprints — same helpers used on resume.
	reviewFP := computeReviewContentFingerprint(input.Checkpoint.FixItems)
	intentFP := computePRIntentFingerprint(input.Checkpoint.Detail)

	out := &awaitingHumanError{
		sessionID:       sessionID,
		executionID:     executionID,
		vendor:          vendor,
		headSHA:         headSHA,
		reviewThreadID:  reviewThreadID,
		reviewCommentID: reviewCommentID,
		reviewContentFP: reviewFP,
		prIntentFP:      intentFP,
		worktreePath:    worktreePath,
	}
	if ask != nil {
		out.question = ask.Question
		out.options = ask.Options
		out.recommendation = ask.Recommendation
		out.recommendedOption = ask.RecommendedOption
		out.consequences = ask.Consequences
		out.confidence = ask.Confidence
	}
	if strings.TrimSpace(out.question) == "" {
		out.question, out.options, out.recommendation = briefFromNeedsHumanReplies(needsHumanReplies, input.Repo, input.PRNumber)
	}
	if strings.TrimSpace(out.question) == "" {
		out.question = fmt.Sprintf("Fixer needs a decision on %s#%d (unresolvable reviewer conflict).", input.Repo, input.PRNumber)
	}
	if len(compactOptions(out.options)) == 0 {
		// Structured needs_human path may synthesize; ask.json path already required options.
		out.options = defaultConflictOptions()
	}
	return out, nil
}

func filterNeedsHumanReplies(replies []replyExplanationEntry) []replyExplanationEntry {
	out := make([]replyExplanationEntry, 0)
	for _, entry := range replies {
		if normalizeReplyAction(entry.Action) == string(replyActionNeedsHuman) {
			out = append(out, entry)
		}
	}
	return out
}

func briefFromNeedsHumanReplies(replies []replyExplanationEntry, repo string, prNumber int64) (question string, options []string, recommendation string) {
	if len(replies) == 0 {
		return "", nil, ""
	}
	parts := make([]string, 0, len(replies))
	for _, entry := range replies {
		if exp := strings.TrimSpace(entry.Explanation); exp != "" {
			parts = append(parts, exp)
		}
	}
	recommendation = strings.Join(parts, " ")
	if recommendation == "" {
		recommendation = "Unresolvable conflict between reviewer feedback and higher-authority intent."
	}
	question = fmt.Sprintf("Fixer needs your call on %s#%d: %s", repo, prNumber, truncateRunes(recommendation, 200))
	return question, defaultConflictOptions(), recommendation
}

func defaultConflictOptions() []string {
	return []string{
		"Keep PR intent; decline the conflicting reviewer request",
		"Follow the reviewer request",
		"Provide different guidance",
	}
}

func compactOptions(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func reviewIDsFromNeedsHuman(replies []replyExplanationEntry, fixItems []FixItem) (threadID, commentID string) {
	itemsByID := make(map[string]FixItem, len(fixItems))
	for _, item := range fixItems {
		itemsByID[item.ID] = item
	}
	for _, entry := range replies {
		if threadID == "" {
			threadID = strings.TrimSpace(entry.ThreadID)
		}
		if item, ok := itemsByID[entry.FixItemID]; ok {
			if threadID == "" {
				threadID = strings.TrimSpace(item.ThreadID)
			}
			if commentID == "" && item.ProviderCommentID > 0 {
				commentID = strconv.FormatInt(item.ProviderCommentID, 10)
			}
		}
	}
	return threadID, commentID
}

// computeReviewContentFingerprint is the canonical review-content drift hash.
// Used at BOTH ask-time and resume-time over the same fix-item fields (not agent
// explanations). Sorted by fix-item id for stability.
//
// ThreadFingerprint is included so non-root reply add/edit (which does not change
// the primary targeted comment body) still invalidates a parked human answer.
func computeReviewContentFingerprint(fixItems []FixItem) string {
	items := make([]FixItem, 0, len(fixItems))
	for _, item := range fixItems {
		if item.Type == "comment" || strings.TrimSpace(item.ThreadID) != "" || item.ProviderCommentID > 0 || strings.TrimSpace(item.Body) != "" {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	parts := make([]string, 0, len(items)*6)
	for _, item := range items {
		parts = append(parts,
			item.ID,
			item.ThreadID,
			strconv.FormatInt(item.ProviderCommentID, 10),
			item.Summary,
			item.Body,
			item.ThreadFingerprint,
		)
	}
	return hitl.FingerprintContent(parts...)
}

// computePRIntentFingerprint is the canonical PR-intent drift hash (title, body,
// base/head SHAs). Used at BOTH ask-time and resume-time. Does not hash reviewer
// fix-item bodies.
func computePRIntentFingerprint(detail *checkpointDetail) string {
	return hitl.FingerprintContent(
		detailTitle(detail),
		detailBody(detail),
		detailBaseSHA(detail),
		detailHeadSHA(detail),
		detailBaseRefName(detail),
	)
}

func detailTitle(detail *checkpointDetail) string {
	if detail == nil {
		return ""
	}
	return strings.TrimSpace(detail.Title)
}

func detailBody(detail *checkpointDetail) string {
	if detail == nil {
		return ""
	}
	return strings.TrimSpace(detail.Body)
}

func detailBaseSHA(detail *checkpointDetail) string {
	if detail == nil {
		return ""
	}
	return strings.TrimSpace(detail.BaseSHA)
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

// suspendForHuman parks a fixer run as awaiting_human: persists HITLAsk, sets
// loop status, cancels the queue item, ends the run interrupted with repair NOT
// complete, clears ask/dismiss sentinels after durable state, and notifies.
func (r *Runner) suspendForHuman(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint fixerCheckpoint, awaiting *awaitingHumanError) (ProcessResult, error) {
	nowISO := r.nowISO()
	// Critical: repair must NOT be marked complete so resume re-enters repair.
	checkpoint.Repair = nil
	checkpoint.ResumePolicy = "advance_from_checkpoint"

	// Clear dismiss.json as early as practical so a pre-answer dismissal cannot
	// survive even if later durable-park steps fail after partial progress.
	worktreePath := strings.TrimSpace(awaiting.worktreePath)
	if worktreePath == "" && checkpoint.Worktree != nil {
		worktreePath = checkpoint.Worktree.Path
	}
	if worktreePath != "" {
		if err := clearDismissSentinel(worktreePath); err != nil && r.logger != nil {
			r.logger.Warn("fixer HITL could not clear dismiss.json at suspend start", map[string]any{
				"loopId": input.Loop.ID, "path": worktreePath, "error": err.Error(),
			})
		}
	}

	ask := loops.HITLAsk{
		Question:                 awaiting.question,
		Options:                  awaiting.options,
		SessionID:                awaiting.sessionID,
		ExecutionID:              awaiting.executionID,
		Vendor:                   awaiting.vendor,
		Status:                   "awaiting",
		AskedAt:                  nowISO,
		Recommendation:           awaiting.recommendation,
		RecommendedOption:        awaiting.recommendedOption,
		Consequences:             awaiting.consequences,
		Confidence:               awaiting.confidence,
		HeadSHA:                  awaiting.headSHA,
		ReviewThreadID:           awaiting.reviewThreadID,
		ReviewCommentID:          awaiting.reviewCommentID,
		ReviewContentFingerprint: awaiting.reviewContentFP,
		PRIntentFingerprint:      awaiting.prIntentFP,
		Role:                     "fixer",
	}
	if r.hitlTransportGitHub() {
		if err := r.deliverAskToGitHub(ctx, input, awaiting, &ask); err != nil && r.logger != nil {
			r.logger.Warn("fixer HITL github ask delivery failed; loop parked awaiting human without a PR comment", map[string]any{
				"loopId": input.Loop.ID, "error": err.Error(),
			})
		}
	}
	// Cancel the claimable queue item BEFORE publishing awaiting_human. Publishing
	// answerable status while a queue item is still active races /respond and the
	// GitHub poll: mutateLoopStatus sees the active item and skips requeue, then
	// cancel drops the only claimable work (lost-wakeup).
	reason := "fixer suspended awaiting human decision"
	if err := r.parkHITLLoop(ctx, input.Loop, ask, nowISO, reason); err != nil {
		return ProcessResult{}, err
	}
	summary := "Awaiting human decision: " + awaiting.question
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}

	// Durable suspension recorded — remove ask.json so it cannot re-suspend.
	// dismiss.json was cleared at suspend start; clear again best-effort.
	if worktreePath != "" {
		hitl.RemoveAskSentinel(worktreePath)
		if err := clearDismissSentinel(worktreePath); err != nil && r.logger != nil {
			r.logger.Warn("fixer HITL could not clear dismiss.json after durable suspend", map[string]any{
				"loopId": input.Loop.ID, "path": worktreePath, "error": err.Error(),
			})
		}
	}

	// Always notify on suspend (all answer transports): sticky osascript + optional
	// Feishu card. GitHub PR-comment delivery above is independent. When GitHub is
	// the answer transport, the secondary Feishu card is notify-only so an answer
	// cannot resume without the GitHub awaiting-label cleanup path.
	if r.hitlNotify != nil {
		title := fmt.Sprintf("Fixer needs a decision · %s #%d", firstNonEmpty(input.Repo, derefString(input.Loop.Repo)), input.PRNumber)
		if input.PRNumber <= 0 && input.Loop.PRNumber != nil {
			title = fmt.Sprintf("Fixer needs a decision · %s #%d", firstNonEmpty(input.Repo, derefString(input.Loop.Repo)), *input.Loop.PRNumber)
		}
		notif := HITLAskNotification{
			ProjectID:         input.Project.ID,
			LoopID:            input.Loop.ID,
			LoopSeq:           input.Loop.Seq,
			RunID:             run.ID,
			Repo:              firstNonEmpty(input.Repo, derefString(input.Loop.Repo)),
			Title:             title,
			Question:          awaiting.question,
			Options:           awaiting.options,
			SourceType:        "GitHub PR",
			SourceRef:         "#" + strconv.FormatInt(input.PRNumber, 10),
			Recommendation:    awaiting.recommendation,
			RecommendedOption: awaiting.recommendedOption,
			Consequences:      awaiting.consequences,
			Confidence:        awaiting.confidence,
			NotifyOnly:        r.hitlTransportGitHub(),
		}
		if input.PRNumber <= 0 && input.Loop.PRNumber != nil {
			notif.SourceRef = "#" + strconv.FormatInt(*input.Loop.PRNumber, 10)
		}
		if err := r.hitlNotify(ctx, notif); err != nil && r.logger != nil {
			r.logger.Warn("fixer HITL ask notification failed; loop parked awaiting human with no notification sent", map[string]any{
				"loopId": input.Loop.ID, "loopSeq": input.Loop.Seq, "runId": run.ID, "error": err.Error(),
			})
		}
	}
	return ProcessResult{LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID, Status: "awaiting_human", Summary: summary}, nil
}

// parkHITLLoop persists the durable HITL ask and awaiting_human status only after
// (or atomically with) cancelling claimable queue work for the loop.
func (r *Runner) parkHITLLoop(ctx context.Context, loop storage.LoopRecord, ask loops.HITLAsk, nowISO, cancelReason string) error {
	apply := func(repos *storage.Repositories) error {
		if _, err := repos.Queue.CancelByLoop(ctx, loop.ID, nowISO, &cancelReason); err != nil {
			return err
		}
		current, err := repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return err
		}
		updated := loop
		if current != nil {
			if current.Status == "terminated" {
				return nil
			}
			updated = *current
		}
		if meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask); werr == nil {
			updated.MetadataJSON = &meta
		}
		updated.Status = "awaiting_human"
		updated.LastRunAt = stringPtr(nowISO)
		updated.NextRunAt = nil
		updated.UpdatedAt = nowISO
		return repos.Loops.Upsert(ctx, updated)
	}
	if r.db != nil {
		return storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
			return apply(storage.NewRepositories(tx))
		})
	}
	if r.repos == nil {
		return fmt.Errorf("repositories unavailable for HITL park")
	}
	return apply(r.repos)
}

func clearDismissSentinel(worktreePath string) error {
	if strings.TrimSpace(worktreePath) == "" {
		return nil
	}
	path := filepath.Join(worktreePath, ".looper", "dismiss.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// dismissSentinelPresent reports whether .looper/dismiss.json exists in the worktree.
func dismissSentinelPresent(worktreePath string) bool {
	if strings.TrimSpace(worktreePath) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(worktreePath, ".looper", "dismiss.json"))
	return err == nil
}

// repliesOrAskNeedHuman reports structured needs_human replies or a present ask sentinel.
func repliesNeedHuman(replies []replyExplanationEntry) bool {
	return len(filterNeedsHumanReplies(replies)) > 0
}

func (r *Runner) hitlTransportGitHub() bool {
	t := strings.TrimSpace(strings.ToLower(r.hitlAnswerTransport))
	return t == "" || t == "github"
}

func (r *Runner) hitlAwaitingLabel() string {
	if l := strings.TrimSpace(r.hitlGitHub.AwaitingLabel); l != "" {
		return l
	}
	return "looper:awaiting-human"
}

func (r *Runner) deliverAskToGitHub(ctx context.Context, input stepInput, awaiting *awaitingHumanError, ask *loops.HITLAsk) error {
	repo := firstNonEmpty(input.Repo, derefString(input.Loop.Repo))
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("hitl github: no repo for loop %s", input.Loop.ID)
	}
	prNumber := input.PRNumber
	if prNumber <= 0 && input.Loop.PRNumber != nil {
		prNumber = *input.Loop.PRNumber
	}
	if prNumber <= 0 {
		return fmt.Errorf("hitl github: no PR number for loop %s", input.Loop.ID)
	}
	cwd := input.Project.RepoPath
	body := buildFixerGitHubAskComment(input.Loop.Seq, awaiting.question, awaiting.options, r.hitlGitHub.MentionLogins)
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	res, err := r.github.CreateIssueComment(ctx, IssueCommentInput{
		Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd,
		DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel,
	})
	if err != nil {
		return err
	}
	if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{
		Repo: repo, PRNumber: prNumber, Labels: []string{r.hitlAwaitingLabel()}, CWD: cwd,
	}); err != nil && r.logger != nil {
		r.logger.Warn("fixer hitl github: failed to add awaiting-human label", map[string]any{"repo": repo, "pr": prNumber, "error": err.Error()})
	}
	// Transport is the answer channel (PR comments). Provider is the forge host
	// that received the ask so the resume poll can use the matching client.
	ask.Transport = "github"
	if r.isForgejoProject(input.Project.ID) {
		ask.Provider = "forgejo"
	} else {
		ask.Provider = "github"
	}
	ask.PRNumber = prNumber
	ask.AskCommentID = res.ID
	return nil
}

const hitlGitHubAskMarkerPrefix = "<!-- looper:hitl:ask v=1"

func buildFixerGitHubAskComment(loopSeq int64, question string, options []string, mentionLogins []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s loop=%d role=fixer -->\n", hitlGitHubAskMarkerPrefix, loopSeq)
	b.WriteString("🤔 **Fixer needs a decision to continue.**\n\n")
	b.WriteString(strings.TrimSpace(question))
	for _, o := range options {
		if o = strings.TrimSpace(o); o != "" {
			fmt.Fprintf(&b, "\n- %s", o)
		}
	}
	b.WriteString("\n\nReply to this comment with your choice — a letter, an option, or free-form guidance. I'll pick it up and continue on this PR.")
	if m := fixerGitHubMentionLine(mentionLogins); m != "" {
		b.WriteString("\n\n" + m)
	}
	return b.String()
}

func fixerGitHubMentionLine(logins []string) string {
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

func truncateRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// errHITLDisabledNeedsHuman is returned when the agent emits needs_human or
// ask.json while HITL is disabled — invalid contract, not silent decline.
func errHITLDisabledNeedsHuman() error {
	return &loopError{
		message: "fixer agent requested needs_human / wrote .looper/ask.json but hitl.enabled is false; invalid agent contract",
		kind:    FailureManualIntervention,
	}
}
