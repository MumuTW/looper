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
	// NotifyOnly suppresses interactive Feishu answer buttons. True for every
	// answerTransport except feishu so github/respond secondary cards cannot
	// authorize repair through an explicitly disabled channel.
	NotifyOnly bool
	// AnswerTransport is the configured hitl.answerTransport so Feishu card
	// copy can name the real answer path (github vs respond vs feishu).
	AnswerTransport string
	// ExecutionID and AskedAt bind Feishu card actions to this ask generation so
	// a stale card from a prior escalation cannot answer a later park.
	ExecutionID string
	AskedAt     string
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

// retireInvalidatedHITLAnswer clears a parked answered decision that can no longer
// be injected (head / review / intent fingerprint drift). Must run before
// restart_from_discover so rediscovery rebuilds FixItems and the next repair does
// not re-abort on hasAnsweredHITLAsk + stale stored fingerprints until retries
// exhaust.
func (r *Runner) retireInvalidatedHITLAnswer(ctx context.Context, loop *storage.LoopRecord, reason string) error {
	if loop == nil || !r.hasAnsweredHITLAsk(ctx, loop) {
		return nil
	}
	if r.logger != nil {
		r.logger.Warn("fixer HITL answered decision retired before rediscovery", map[string]any{
			"loopId": loop.ID, "reason": reason,
		})
	}
	return r.markHumanAnswerConsumed(ctx, loop)
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
// The live set starts from checkpoint FixItems, then appends any newly listed
// provider items (GitHub threads, Forgejo native comments, forgejo-reviewer-summary
// opens) that appeared while parked. Without that append, ask/resume hashes stay
// equal while the agent would resume with stale FixItems that omit the new feedback.
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
	var liveGitHubThreads []ReviewThread
	// Always refresh the project's applicable review surfaces — including pure
	// conflict/check parks whose FixItems contain no review comments. Otherwise a
	// new actionable review opened while parked is never listed, live FP still
	// matches the empty ask hash, and resume can push with stale FixItems.
	// Surfaces already present in FixItems still enable the same listing path.
	needNative := false
	needSummary := false
	needGitHubThreads := false
	if r.isForgejoProject(input.Project.ID) {
		needNative = true
		needSummary = true
	} else {
		needGitHubThreads = true
	}
	for _, item := range fixItems {
		src := strings.TrimSpace(item.Source)
		if src == NativeReviewCommentSource && item.ProviderCommentID > 0 {
			needNative = true
		}
		if src == "forgejo-reviewer-summary" {
			needSummary = true
		}
		if src == "" && strings.TrimSpace(item.ThreadID) != "" {
			needGitHubThreads = true
		}
	}
	// Live native/summary surfaces must apply the same current-user authority filters
	// as ask-time collection (attachManualForgejoNativeComments / sanitizeForgejoSummaryAuthority).
	// Without that, self-authored native comments or untrusted summary markers change
	// the resume fingerprint and retire a still-valid human answer.
	var liveCurrentUser string
	if needNative || needSummary {
		login, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return "", fmt.Errorf("HITL live review refresh current-user lookup failed: %w", err)
		}
		liveCurrentUser = strings.TrimSpace(login)
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
		// Discovery only trusts summary markers from the configured current user.
		// Sanitize before collectFixItems so a second marker from anyone else cannot
		// uniqueness-error the authoritative summary and falsely invalidate the answer.
		sanitizeForgejoSummaryAuthority(&detail, liveCurrentUser)
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
	if needGitHubThreads {
		threads, err := r.github.ListReviewThreads(ctx, ListReviewThreadsInput{
			Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath,
		})
		if err != nil {
			return "", fmt.Errorf("HITL live GitHub review-thread list failed: %w", err)
		}
		liveGitHubThreads = threads
	}

	seenThreadIDs := make(map[string]struct{})
	seenNativeIDs := make(map[int64]struct{})
	seenSummaryIDs := make(map[string]struct{})
	liveItems := make([]FixItem, 0, len(fixItems)+len(liveGitHubThreads)+len(nativeByProvider)+len(summaryByID))
	for _, item := range fixItems {
		live := item
		src := strings.TrimSpace(item.Source)
		switch {
		case src == NativeReviewCommentSource && item.ProviderCommentID > 0:
			seenNativeIDs[item.ProviderCommentID] = struct{}{}
			// Forgejo native: ListNativeReviewComments only (no ViewReviewThread).
			if c, ok := nativeByProvider[item.ProviderCommentID]; ok {
				// Resolved native comments are no longer live feedback; invalidate
				// the parked answer the same way as a missing comment.
				if c.IsResolved {
					markLiveReviewMissing(&live)
				} else if text := strings.TrimSpace(c.Body); text != "" {
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
			if id := strings.TrimSpace(item.ID); id != "" {
				seenSummaryIDs[id] = struct{}{}
			}
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
			if tid := strings.TrimSpace(item.ThreadID); tid != "" {
				seenThreadIDs[tid] = struct{}{}
			}
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
			} else if thread.IsResolved {
				// Reviewer resolved the thread while parked: body/timestamps may be
				// unchanged, but the feedback is no longer live — invalidate.
				markLiveReviewMissing(&live)
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

	// Append provider items that appeared while parked (not in the checkpoint).
	if needGitHubThreads {
		for _, thread := range liveGitHubThreads {
			if thread.IsResolved {
				continue
			}
			tid := strings.TrimSpace(thread.ID)
			if tid == "" {
				continue
			}
			if _, seen := seenThreadIDs[tid]; seen {
				continue
			}
			if item, ok := fixItemFromLiveGitHubThread(thread); ok {
				liveItems = append(liveItems, item)
				seenThreadIDs[tid] = struct{}{}
			}
		}
	}
	if needNative {
		// Match attachManualForgejoNativeComments: only unresolved, non-self comments
		// are FixItems at ask time, so only those can count as mid-park drift.
		for _, c := range actionableNativeReviewComments(mapValuesNativeComments(nativeByProvider), liveCurrentUser) {
			providerID := c.ProviderCommentID
			if providerID <= 0 {
				continue
			}
			if _, seen := seenNativeIDs[providerID]; seen {
				continue
			}
			if item, ok := fixItemFromLiveNativeComment(c); ok {
				liveItems = append(liveItems, item)
				seenNativeIDs[providerID] = struct{}{}
			}
		}
	}
	if needSummary {
		for id, liveItem := range summaryByID {
			if id == "" {
				continue
			}
			if _, seen := seenSummaryIDs[id]; seen {
				continue
			}
			liveItems = append(liveItems, liveItem)
			seenSummaryIDs[id] = struct{}{}
		}
	}
	return computeReviewContentFingerprint(liveItems), nil
}

// fixItemFromLiveGitHubThread builds a GitHub-shaped FixItem for a live thread
// that was not present at ask time (mid-park new top-level review).
func fixItemFromLiveGitHubThread(thread ReviewThread) (FixItem, bool) {
	tid := strings.TrimSpace(thread.ID)
	if tid == "" {
		return FixItem{}, false
	}
	body := ""
	id := ""
	for _, c := range thread.Comments {
		if isLooperFixerReplyComment(c) {
			continue
		}
		if b := strings.TrimSpace(c.Body); b != "" {
			body = b
			id = strings.TrimSpace(c.ID)
			break
		}
	}
	if body == "" {
		return FixItem{}, false
	}
	if id == "" {
		id = tid
	}
	return FixItem{
		Type:              "comment",
		ID:                id,
		ThreadID:          tid,
		Summary:           body,
		Body:              "",
		ThreadFingerprint: liveReviewThreadFingerprint(thread),
	}, true
}

// mapValuesNativeComments returns native comments from a provider-id map in
// arbitrary order (callers re-filter/sort as needed).
func mapValuesNativeComments(byID map[int64]NativeReviewComment) []NativeReviewComment {
	if len(byID) == 0 {
		return nil
	}
	out := make([]NativeReviewComment, 0, len(byID))
	for _, c := range byID {
		out = append(out, c)
	}
	return out
}

// fixItemFromLiveNativeComment builds a native Forgejo FixItem for a live
// comment that was not present at ask time.
func fixItemFromLiveNativeComment(c NativeReviewComment) (FixItem, bool) {
	if c.ProviderCommentID <= 0 {
		return FixItem{}, false
	}
	text := strings.TrimSpace(c.Body)
	if text == "" {
		return FixItem{}, false
	}
	fp := strings.TrimSpace(c.ObservedFingerprint)
	if fp == "" {
		fp = NativeReviewCommentFingerprint(c.ProviderCommentID, c.UpdatedAt)
	}
	return FixItem{
		Type:                "comment",
		Source:              NativeReviewCommentSource,
		ID:                  NativeReviewCommentFixItemID(c.ProviderCommentID),
		ThreadID:            NativeReviewCommentThreadID(c.ProviderCommentID),
		ProviderCommentID:   c.ProviderCommentID,
		Summary:             text,
		Body:                text,
		ThreadFingerprint:   fp,
		ObservedFingerprint: fp,
		Path:                strings.TrimSpace(c.Path),
		URL:                 strings.TrimSpace(c.URL),
		Author:              strings.TrimSpace(c.Author),
		ResolverPresent:     c.ResolverPresent,
	}, true
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
// non-Looper-fixer comments as id@updatedAt joined by "|". Exclusion uses
// githubinfra.IsLooperFixerReplyBody (via isLooperFixerReplyComment), the same
// authority as collect-time, so declined replies cannot diverge ask vs resume.
// Mid-park human/reviewer reply add/edit changes this value so resume-time
// fingerprints diverge from ask-time.
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
	// IsDraft is eligibility-critical (IncludeDrafts policy); copy so resume can
	// abort before the agent when the PR became draft while parked.
	checkpoint.Detail.IsDraft = live.IsDraft
	if live.Labels != nil {
		checkpoint.Detail.Labels = cloneStrings(live.Labels)
	}
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

// abortStaleWorktreeMergeBeforeRediscover clears an in-progress merge left by a
// conflicted repair when HITL drift forces restart_from_discover. Best-effort
// no-op when there is no MERGE_HEAD or git is unavailable.
func (r *Runner) abortStaleWorktreeMergeBeforeRediscover(ctx context.Context, worktreePath string) error {
	path := strings.TrimSpace(worktreePath)
	if path == "" || !worktreeHasInProgressMerge(path) {
		return nil
	}
	if r.git == nil {
		return fmt.Errorf("git gateway unavailable to abort obsolete merge in %s", path)
	}
	if err := r.git.AbortInProgressMerge(ctx, path); err != nil {
		return err
	}
	if r.logger != nil {
		r.logger.Info("fixer HITL aborted obsolete merge before rediscovery", map[string]any{
			"worktreePath": path,
		})
	}
	return nil
}

// worktreeHasInProgressMerge reports whether the worktree still has a git merge
// in progress (MERGE_HEAD). Used so answered HITL conflict resume skips
// MergeBaseIntoWorktree only when the retained worktree still carries the
// original conflict; a replaced worktree at PR head must re-run the merge.
func worktreeHasInProgressMerge(worktreePath string) bool {
	path := strings.TrimSpace(worktreePath)
	if path == "" {
		return false
	}
	// Ordinary checkout: .git/ is a directory.
	if st, err := os.Stat(filepath.Join(path, ".git", "MERGE_HEAD")); err == nil && !st.IsDir() {
		return true
	}
	// Linked worktree: .git is a gitdir: pointer file.
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return false
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if gitdir == "" {
		return false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	st, err := os.Stat(filepath.Join(gitdir, "MERGE_HEAD"))
	return err == nil && !st.IsDir()
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
	// Prefer live surface FP so conflict-only parks snapshot open reviews the same
	// way resume does (mid-park new review must diverge).
	reviewFP := r.askTimeReviewContentFingerprint(ctx, input)
	out := &awaitingHumanError{
		sessionID:       sessionID,
		executionID:     executionID,
		vendor:          vendor,
		headSHA:         detailHeadSHA(input.Checkpoint.Detail),
		reviewContentFP: reviewFP,
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
	// Canonical fingerprints — same helpers used on resume (live surfaces so
	// conflict-only parks include open reviews in the ask-time baseline).
	reviewFP := r.askTimeReviewContentFingerprint(ctx, input)
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

// askTimeReviewContentFingerprint snapshots review content at park time using the
// same live-surface listing as resume. Falls back to checkpoint FixItems when the
// live refresh fails so suspend still parks (resume will fail closed on mismatch).
func (r *Runner) askTimeReviewContentFingerprint(ctx context.Context, input stepInput) string {
	if r.github != nil {
		if fp, err := r.liveReviewContentFingerprint(ctx, input, input.Checkpoint.FixItems); err == nil {
			return fp
		}
	}
	return computeReviewContentFingerprint(input.Checkpoint.FixItems)
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

// errHITLLoopTerminated is returned when parkHITLLoop observes the loop was
// stopped/terminated concurrently; suspend must exit without completing a park.
var errHITLLoopTerminated = errors.New("fixer HITL park aborted: loop terminated")

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

	// Serialize answer delivery with this suspension: once park publishes
	// awaiting_human the cancelled queue row is requeueable by /respond and the
	// GitHub poll. Holding the shared requeue lock until suspend fully finishes
	// (delivery, correlation attach, run complete, notify) keeps the item
	// nonclaimable for answer paths that take the same lock before requeue.
	// Call order matches deliverHITLAnswerToLoop: loop lock, then target lock.
	unlockRequeue := loops.LockLoopRequeue(input.Loop.ID)
	defer unlockRequeue()
	unlockTarget := loops.LockLoopTarget(loops.LoopTargetGuardKeyFromRecord(input.Loop))
	defer unlockTarget()

	// Durable park BEFORE remote GitHub mutation so a DB failure cannot leave an
	// orphan PR ask without a loop record. Stamp transport/PR identity first so
	// the parked ask is poll-eligible once AskCommentID is filled after delivery.
	// DeliveryPending stays true until AskCommentID correlation is durable so a
	// daemon crash mid-delivery is recoverable at startup (poll skips id==0).
	// Cancel the claimable queue item BEFORE publishing awaiting_human. Publishing
	// answerable status while a queue item is still active races /respond and the
	// GitHub poll: mutateLoopStatus sees the active item and skips requeue, then
	// cancel drops the only claimable work (lost-wakeup).
	if r.hitlTransportGitHub() {
		if err := r.stampGitHubAskTransport(input, &ask); err != nil {
			return ProcessResult{}, &loopError{
				message: fmt.Sprintf("HITL GitHub ask transport setup failed: %v", err),
				kind:    FailureRetryableTransient,
			}
		}
		ask.DeliveryPending = true
	} else {
		// Persist the configured answer channel so Feishu callbacks can reject
		// github/respond parks even when secondary notify cards exist.
		ask.Transport = r.hitlConfiguredTransport()
	}
	reason := "fixer suspended awaiting human decision"
	if err := r.parkHITLLoop(ctx, input.Loop, ask, nowISO, reason); err != nil {
		if errors.Is(err, errHITLLoopTerminated) {
			// Concurrent stop: do not mark interrupted, notify, or return awaiting_human.
			return r.finishHITLSuspendAfterTerminate(ctx, input, run, checkpoint)
		}
		return ProcessResult{}, err
	}

	if r.hitlTransportGitHub() {
		if err := r.deliverAskToGitHub(ctx, input, awaiting, &ask); err != nil {
			// Park happened without a PR comment. Roll back to running, clear the
			// incomplete ask, and requeue the cancelled claim so the scheduler can
			// MarkRetry-equivalent retry even when callers use suspendForHuman
			// directly (ask.json remains for undurable re-park).
			if rbErr := r.rollbackHITLParkForDeliveryRetry(ctx, input.Loop, nowISO); rbErr != nil && r.logger != nil {
				r.logger.Warn("fixer HITL rollback after github delivery failure failed", map[string]any{
					"loopId": input.Loop.ID, "error": rbErr.Error(),
				})
			}
			return ProcessResult{}, &loopError{
				message: fmt.Sprintf("HITL GitHub ask delivery failed after park: %v", err),
				kind:    FailureRetryableTransient,
			}
		}
		// Persist AskCommentID onto the durable park (delivery is remote-side;
		// comment id must land in metadata for detectGitHubHITLAnswer / poll).
		// When both CAS attach paths fail, do not complete suspension with
		// AskCommentID==0: stash the delivered id on disk, roll the park back for
		// re-entry, and return retryable. deliverAskToGitHub reloads the stash so
		// the next attempt does not CreateIssueComment again.
		if err := r.persistParkedHITLAsk(ctx, input.Loop, ask, nowISO); err != nil {
			if forceErr := r.forcePersistDeliveredAskComment(ctx, input.Loop, ask, nowISO); forceErr != nil {
				if worktreePath != "" && ask.AskCommentID > 0 {
					if stashErr := hitl.WriteDeliveredCommentStash(worktreePath, hitl.DeliveredCommentStash{
						AskCommentID: ask.AskCommentID,
						ExecutionID:  ask.ExecutionID,
						Generation:   deliveryGenerationForAsk(worktreePath, &ask),
						PRNumber:     ask.PRNumber,
						Provider:     ask.Provider,
						Transport:    ask.Transport,
						Question:     ask.Question,
					}); stashErr != nil && r.logger != nil {
						r.logger.Warn("fixer HITL could not stash delivered AskCommentID for correlation retry", map[string]any{
							"loopId": input.Loop.ID, "askCommentId": ask.AskCommentID, "error": stashErr.Error(),
						})
					}
				}
				if rbErr := r.rollbackHITLParkForDeliveryRetry(ctx, input.Loop, nowISO); rbErr != nil && r.logger != nil {
					r.logger.Warn("fixer HITL rollback after correlation attach failure failed", map[string]any{
						"loopId": input.Loop.ID, "error": rbErr.Error(),
					})
				}
				return ProcessResult{}, &loopError{
					message: fmt.Sprintf("HITL GitHub ask correlation failed after delivery (comment %d): persist=%v; force=%v", ask.AskCommentID, err, forceErr),
					kind:    FailureRetryableTransient,
				}
			}
		}
		// Correlation is durable — drop delivery artifacts and clear
		// DeliveryPending so startup recovery will not requeue a complete park.
		ask.DeliveryPending = false
		if worktreePath != "" {
			hitl.RemoveDeliveryArtifacts(worktreePath)
		}
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
	// Feishu card. GitHub PR-comment delivery above is independent. Interactive
	// Feishu answer buttons are only for answerTransport=feishu; github/respond
	// secondary cards are notify-only so a disabled channel cannot authorize repair.
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
			NotifyOnly:        !r.hitlTransportFeishu(),
			AnswerTransport:   r.hitlConfiguredTransport(),
			ExecutionID:       ask.ExecutionID,
			AskedAt:           ask.AskedAt,
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

// finishHITLSuspendAfterTerminate ends the run without notifying or returning
// awaiting_human when the loop was terminated concurrently during park.
func (r *Runner) finishHITLSuspendAfterTerminate(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint fixerCheckpoint) (ProcessResult, error) {
	summary := "Fixer HITL suspend aborted because the loop was terminated"
	if _, err := r.completeRun(ctx, run, "interrupted", summary, summary, checkpoint); err != nil {
		return ProcessResult{}, err
	}
	// Best-effort: drop ask/dismiss sentinels so a terminated loop cannot re-park.
	if checkpoint.Worktree != nil {
		hitl.RemoveAskSentinel(checkpoint.Worktree.Path)
		_ = clearDismissSentinel(checkpoint.Worktree.Path)
	}
	return ProcessResult{
		LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID,
		Status: "terminated", Summary: summary,
	}, nil
}

// rollbackHITLParkForDeliveryRetry undoes an awaiting_human park that never got
// a GitHub AskCommentID so the scheduler can claim work again and re-enter
// suspend. parkHITLLoop already cancelled the active queue row; without requeue
// here a transient CreateIssueComment failure leaves running + no claimable item
// when callers return the error without going through recoverClaimedItem.
func (r *Runner) rollbackHITLParkForDeliveryRetry(ctx context.Context, loop storage.LoopRecord, nowISO string) error {
	apply := func(repos *storage.Repositories) error {
		current, err := repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return nil
		}
		if current.Status != "awaiting_human" {
			return nil
		}
		updated := *current
		if meta, cerr := loops.ClearHITLAsk(updated.MetadataJSON); cerr == nil {
			updated.MetadataJSON = &meta
		}
		// running + requeued cancelled claim so the scheduler retries suspend.
		updated.Status = "running"
		updated.NextRunAt = &nowISO
		updated.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return err
		}
		if _, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, loop.ID, nowISO); err != nil {
			return err
		}
		return nil
	}
	if r.db != nil {
		return storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
			return apply(storage.NewRepositories(tx))
		})
	}
	if r.repos == nil {
		return fmt.Errorf("repositories unavailable for HITL park rollback")
	}
	return apply(r.repos)
}

// forcePersistDeliveredAskComment writes AskCommentID (and transport identity)
// onto the durable park after a successful remote CreateIssueComment when the
// normal CAS attach failed. Never clears the park: a human may already see the
// first question, and a full rollback would cause a second post on retry.
func (r *Runner) forcePersistDeliveredAskComment(ctx context.Context, loop storage.LoopRecord, delivered loops.HITLAsk, nowISO string) error {
	if delivered.AskCommentID <= 0 {
		return fmt.Errorf("no delivered AskCommentID to force-persist")
	}
	apply := func(repos *storage.Repositories) error {
		current, err := repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("loop %s not found for HITL ask force-attach", loop.ID)
		}
		if current.Status == "terminated" || current.Status == "stopped" {
			return errHITLLoopTerminated
		}
		existing, ok := loops.ReadHITLAsk(current.MetadataJSON)
		if !ok {
			// Park metadata missing; re-seed minimal awaiting ask with the
			// delivered comment so poll/dashboard can still correlate.
			existing = loops.HITLAsk{
				Question: delivered.Question, Options: delivered.Options,
				SessionID: delivered.SessionID, ExecutionID: delivered.ExecutionID,
				Vendor: delivered.Vendor, Status: "awaiting", AskedAt: delivered.AskedAt,
				Role: delivered.Role,
			}
		}
		if existing.AskCommentID == 0 {
			existing.AskCommentID = delivered.AskCommentID
		}
		if existing.AskCommentID > 0 {
			existing.DeliveryPending = false
		}
		if strings.TrimSpace(existing.Transport) == "" {
			existing.Transport = delivered.Transport
		}
		if strings.TrimSpace(existing.Provider) == "" {
			existing.Provider = delivered.Provider
		}
		if existing.PRNumber <= 0 {
			existing.PRNumber = delivered.PRNumber
		}
		meta, werr := loops.WriteHITLAsk(current.MetadataJSON, existing)
		if werr != nil {
			return werr
		}
		updated := *current
		updated.MetadataJSON = &meta
		updated.UpdatedAt = nowISO
		return repos.Loops.Upsert(ctx, updated)
	}
	if r.db != nil {
		return storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
			return apply(storage.NewRepositories(tx))
		})
	}
	if r.repos == nil {
		return fmt.Errorf("repositories unavailable for HITL ask force-attach")
	}
	return apply(r.repos)
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
			if current.Status == "terminated" || current.Status == "stopped" {
				return errHITLLoopTerminated
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

// persistParkedHITLAsk attaches post-delivery correlation fields (AskCommentID
// and transport identity) onto the durable park without replacing the entire
// current ask. A dashboard /respond or poll delivery that wins after park but
// before CreateIssueComment returns must keep status=answered and the answer
// body; overwriting with the local awaiting snapshot would drop the decision
// while the loop stays running and never re-polls.
//
// Attachment is CAS-scoped to the park generation: when the durable ask is still
// awaiting and (if set) shares the same ExecutionID, correlation fields are
// merged onto that record. When the ask was already answered/consumed, only a
// missing AskCommentID is filled in so label cleanup can still find the PR
// comment.
func (r *Runner) persistParkedHITLAsk(ctx context.Context, loop storage.LoopRecord, ask loops.HITLAsk, nowISO string) error {
	apply := func(repos *storage.Repositories) error {
		current, err := repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("loop %s not found for HITL ask update", loop.ID)
		}
		if current.Status == "terminated" || current.Status == "stopped" {
			return errHITLLoopTerminated
		}
		existing, ok := loops.ReadHITLAsk(current.MetadataJSON)
		if !ok {
			// Park metadata missing (cleared concurrently); nothing to attach to.
			return nil
		}
		merged, changed := mergeHITLAskDeliveryCorrelation(existing, ask)
		if !changed {
			return nil
		}
		updated := *current
		meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, merged)
		if werr != nil {
			return werr
		}
		updated.MetadataJSON = &meta
		updated.UpdatedAt = nowISO
		return repos.Loops.Upsert(ctx, updated)
	}
	if r.db != nil {
		return storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
			return apply(storage.NewRepositories(tx))
		})
	}
	if r.repos == nil {
		return fmt.Errorf("repositories unavailable for HITL ask update")
	}
	return apply(r.repos)
}

// mergeHITLAskDeliveryCorrelation merges post-delivery PR/comment correlation
// from delivered onto the durable current ask. It never rewrites answer/status/
// question fields from a stale local park snapshot.
func mergeHITLAskDeliveryCorrelation(current, delivered loops.HITLAsk) (loops.HITLAsk, bool) {
	out := current
	status := strings.TrimSpace(current.Status)
	switch status {
	case "awaiting":
		// Same park generation only: a newer re-ask must not inherit the old
		// comment id or lose its own identity via a late delivery write.
		curExec := strings.TrimSpace(current.ExecutionID)
		delExec := strings.TrimSpace(delivered.ExecutionID)
		if curExec != "" && delExec != "" && curExec != delExec {
			return current, false
		}
	case "answered", "consumed":
		// Decision already landed. Only fill a missing AskCommentID (and empty
		// transport identity) so awaiting-label cleanup can still resolve the PR.
	default:
		// Unknown/empty status: treat like awaiting for correlation attach when
		// the durable record still looks like the park we just wrote.
		if status != "" {
			return current, false
		}
	}

	changed := false
	if delivered.AskCommentID != 0 && out.AskCommentID != delivered.AskCommentID {
		// Never replace a non-zero durable comment id (different generation or
		// already attached); only fill when unset or update when still zero.
		if out.AskCommentID == 0 {
			out.AskCommentID = delivered.AskCommentID
			changed = true
		}
	}
	// Correlation complete (or already had an id): clear delivery-pending so
	// startup recovery will not treat this park as an incomplete delivery.
	if out.AskCommentID > 0 && out.DeliveryPending {
		out.DeliveryPending = false
		changed = true
	}
	if t := strings.TrimSpace(delivered.Transport); t != "" && strings.TrimSpace(out.Transport) == "" {
		out.Transport = t
		changed = true
	}
	if p := strings.TrimSpace(delivered.Provider); p != "" && strings.TrimSpace(out.Provider) == "" {
		out.Provider = p
		changed = true
	}
	if delivered.PRNumber != 0 && out.PRNumber == 0 {
		out.PRNumber = delivered.PRNumber
		changed = true
	}
	return out, changed
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

// hitlTransportFeishu reports whether Feishu is the configured answer channel
// (interactive cards + inbound callback authority).
func (r *Runner) hitlTransportFeishu() bool {
	return strings.EqualFold(strings.TrimSpace(r.hitlAnswerTransport), "feishu")
}

// hitlConfiguredTransport returns the durable transport stamp for a non-github
// park ("feishu" | "respond"). Empty config defaults to github and is handled
// by stampGitHubAskTransport instead.
func (r *Runner) hitlConfiguredTransport() string {
	t := strings.TrimSpace(strings.ToLower(r.hitlAnswerTransport))
	switch t {
	case "feishu", "respond":
		return t
	default:
		if t == "" {
			return "github"
		}
		return t
	}
}

func (r *Runner) hitlAwaitingLabel() string {
	if l := strings.TrimSpace(r.hitlGitHub.AwaitingLabel); l != "" {
		return l
	}
	return "looper:awaiting-human"
}

// stampGitHubAskTransport fills Transport/Provider/PRNumber on the ask before
// the durable park so a post-delivery metadata update only needs AskCommentID.
func (r *Runner) stampGitHubAskTransport(input stepInput, ask *loops.HITLAsk) error {
	if ask == nil {
		return fmt.Errorf("hitl github: nil ask")
	}
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
	ask.Transport = "github"
	if r.isForgejoProject(input.Project.ID) {
		ask.Provider = "forgejo"
	} else {
		ask.Provider = "github"
	}
	ask.PRNumber = prNumber
	return nil
}

// deliveredCommentStashMatchesPark reports whether a worktree-delivered ask
// comment belongs to the current delivery generation. Match on PR identity and
// the retry-stable Generation token (not agent executionId — correlation-retry
// re-entry assigns a fresh execution id while reusing the same delivery cycle).
func deliveredCommentStashMatchesPark(stash *hitl.DeliveredCommentStash, ask *loops.HITLAsk, generation string) bool {
	if stash == nil || ask == nil || stash.AskCommentID <= 0 {
		return false
	}
	if stash.PRNumber > 0 && ask.PRNumber > 0 && stash.PRNumber != ask.PRNumber {
		return false
	}
	if p := strings.TrimSpace(stash.Provider); p != "" {
		if ap := strings.TrimSpace(ask.Provider); ap != "" && !strings.EqualFold(p, ap) {
			return false
		}
	}
	if t := strings.TrimSpace(stash.Transport); t != "" {
		if at := strings.TrimSpace(ask.Transport); at != "" && !strings.EqualFold(t, at) {
			return false
		}
	}
	// Generation is required when the stash carries one (post-binding records).
	// Legacy stashes without Generation still match by PR identity only.
	if sg := strings.TrimSpace(stash.Generation); sg != "" {
		if strings.TrimSpace(generation) == "" || sg != strings.TrimSpace(generation) {
			return false
		}
	}
	return true
}

// resolveDeliveryGeneration returns the retry-stable token for this GitHub ask
// delivery cycle. Incomplete cycles keep the worktree generation file + stash
// pair; a later escalation mints a fresh token so a leftover stash cannot be
// adopted after a completed park.
func resolveDeliveryGeneration(worktreePath string, ask *loops.HITLAsk) string {
	if ask == nil {
		return ""
	}
	stash, _ := hitl.ReadDeliveredCommentStash(worktreePath)
	genFile, _ := hitl.ReadDeliveryGeneration(worktreePath)
	// Incomplete delivery: both artifacts present with the same token.
	if stash != nil && genFile != nil {
		sg := strings.TrimSpace(stash.Generation)
		fg := strings.TrimSpace(genFile.Generation)
		if sg != "" && sg == fg {
			if deliveredCommentStashMatchesPark(stash, ask, sg) {
				return sg
			}
		}
		// Stale leftover pair from a prior cycle — discard.
		hitl.RemoveDeliveredCommentStash(worktreePath)
	} else if stash != nil && genFile == nil {
		// Stash without generation file is stale after a successful correlation
		// that removed the file first (or a partial cleanup).
		hitl.RemoveDeliveredCommentStash(worktreePath)
	}
	// Gen file alone may mean CreateIssueComment succeeded before stash write —
	// keep that token so recoverAskCommentByGeneration can find the remote post.
	if genFile != nil {
		if g := strings.TrimSpace(genFile.Generation); g != "" {
			if genFile.PRNumber <= 0 || ask.PRNumber <= 0 || genFile.PRNumber == ask.PRNumber {
				return g
			}
		}
	}
	// Fresh delivery cycle.
	gen := strings.TrimSpace(ask.ExecutionID)
	if gen == "" {
		gen = hitl.FingerprintContent(ask.Question, ask.AskedAt, strings.Join(ask.Options, "\x00"))
	}
	if worktreePath != "" && gen != "" {
		_ = hitl.WriteDeliveryGeneration(worktreePath, hitl.DeliveryGeneration{
			Generation: gen, PRNumber: ask.PRNumber, Question: ask.Question,
		})
	}
	return gen
}

// deliveryGenerationForAsk returns the current worktree generation token when
// present, else a stable fallback for stash writes.
func deliveryGenerationForAsk(worktreePath string, ask *loops.HITLAsk) string {
	if genFile, _ := hitl.ReadDeliveryGeneration(worktreePath); genFile != nil {
		if g := strings.TrimSpace(genFile.Generation); g != "" {
			return g
		}
	}
	if ask == nil {
		return ""
	}
	if g := strings.TrimSpace(ask.ExecutionID); g != "" {
		return g
	}
	return hitl.FingerprintContent(ask.Question, ask.AskedAt)
}

func (r *Runner) deliverAskToGitHub(ctx context.Context, input stepInput, awaiting *awaitingHumanError, ask *loops.HITLAsk) error {
	if ask == nil {
		return fmt.Errorf("hitl github: nil ask")
	}
	// Transport fields may already be stamped at park time; re-stamp is idempotent.
	if err := r.stampGitHubAskTransport(input, ask); err != nil {
		return err
	}
	worktreePath := ""
	if awaiting != nil {
		worktreePath = strings.TrimSpace(awaiting.worktreePath)
	}
	if worktreePath == "" && input.Checkpoint.Worktree != nil {
		worktreePath = strings.TrimSpace(input.Checkpoint.Worktree.Path)
	}
	// Idempotent recovery: a prior CreateIssueComment already succeeded (e.g.
	// correlation write failed mid-suspend). Never post a second question.
	if ask.AskCommentID > 0 {
		return nil
	}
	if existing, ok := r.readFreshHITLAsk(ctx, &input.Loop); ok && existing.AskCommentID > 0 {
		// Same park generation (or already answered with this comment).
		if strings.TrimSpace(existing.ExecutionID) == "" ||
			strings.TrimSpace(existing.ExecutionID) == strings.TrimSpace(ask.ExecutionID) ||
			existing.Status == "answered" || existing.Status == "consumed" {
			ask.AskCommentID = existing.AskCommentID
			if strings.TrimSpace(ask.Transport) == "" {
				ask.Transport = existing.Transport
			}
			if strings.TrimSpace(ask.Provider) == "" {
				ask.Provider = existing.Provider
			}
			if ask.PRNumber <= 0 {
				ask.PRNumber = existing.PRNumber
			}
			return nil
		}
	}
	// Retry-stable delivery generation (survives correlation rollback + fresh
	// execution IDs; rejects leftover stashes from a prior completed escalation).
	generation := resolveDeliveryGeneration(worktreePath, ask)
	// Worktree stash written when CreateIssueComment succeeded but durable
	// correlation attach failed and the park was rolled back for retry.
	if worktreePath != "" {
		if stash, stashErr := hitl.ReadDeliveredCommentStash(worktreePath); stashErr != nil {
			return fmt.Errorf("hitl github: read delivered comment stash: %w", stashErr)
		} else if stash != nil && stash.AskCommentID > 0 {
			if deliveredCommentStashMatchesPark(stash, ask, generation) {
				ask.AskCommentID = stash.AskCommentID
				if strings.TrimSpace(ask.Transport) == "" && strings.TrimSpace(stash.Transport) != "" {
					ask.Transport = stash.Transport
				}
				if strings.TrimSpace(ask.Provider) == "" && strings.TrimSpace(stash.Provider) != "" {
					ask.Provider = stash.Provider
				}
				if ask.PRNumber <= 0 && stash.PRNumber > 0 {
					ask.PRNumber = stash.PRNumber
				}
				return nil
			}
		}
	}
	repo := firstNonEmpty(input.Repo, derefString(input.Loop.Repo))
	prNumber := ask.PRNumber
	cwd := input.Project.RepoPath
	// Recover a remote ask posted before the local stash was written (crash
	// between CreateIssueComment and WriteDeliveredCommentStash).
	if recoveredID := r.recoverAskCommentByGeneration(ctx, input, prNumber, input.Loop.Seq, generation, ask.Question); recoveredID > 0 {
		ask.AskCommentID = recoveredID
		if worktreePath != "" {
			if stashErr := hitl.WriteDeliveredCommentStash(worktreePath, hitl.DeliveredCommentStash{
				AskCommentID: ask.AskCommentID,
				ExecutionID:  ask.ExecutionID,
				Generation:   generation,
				PRNumber:     ask.PRNumber,
				Provider:     ask.Provider,
				Transport:    ask.Transport,
				Question:     ask.Question,
			}); stashErr != nil && r.logger != nil {
				r.logger.Warn("fixer hitl github: could not stash recovered AskCommentID", map[string]any{
					"loopId": input.Loop.ID, "askCommentId": ask.AskCommentID, "error": stashErr.Error(),
				})
			}
		}
		return nil
	}
	// Generation file without a recoverable comment is leftover or abandoned —
	// mint a fresh delivery token so we do not re-tag a new post with a stale gen.
	if worktreePath != "" {
		if genFile, _ := hitl.ReadDeliveryGeneration(worktreePath); genFile != nil {
			if stash, _ := hitl.ReadDeliveredCommentStash(worktreePath); stash == nil {
				generation = strings.TrimSpace(ask.ExecutionID)
				if generation == "" {
					generation = hitl.FingerprintContent(ask.Question, ask.AskedAt, strings.Join(ask.Options, "\x00"))
				}
			}
		}
	}
	// Ensure generation is on disk before the remote mutation so a crash after
	// CreateIssueComment can still recover via the deterministic marker.
	if worktreePath != "" && strings.TrimSpace(generation) != "" {
		_ = hitl.WriteDeliveryGeneration(worktreePath, hitl.DeliveryGeneration{
			Generation: generation, PRNumber: ask.PRNumber, Question: ask.Question,
		})
	}
	body := buildFixerGitHubAskComment(input.Loop.Seq, generation, awaiting.question, awaiting.options, r.hitlGitHub.MentionLogins)
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
	ask.AskCommentID = res.ID
	// Stash immediately so a post-delivery correlation failure can retry without
	// posting a second PR question.
	if worktreePath != "" {
		if stashErr := hitl.WriteDeliveredCommentStash(worktreePath, hitl.DeliveredCommentStash{
			AskCommentID: ask.AskCommentID,
			ExecutionID:  ask.ExecutionID,
			Generation:   generation,
			PRNumber:     ask.PRNumber,
			Provider:     ask.Provider,
			Transport:    ask.Transport,
			Question:     ask.Question,
		}); stashErr != nil && r.logger != nil {
			r.logger.Warn("fixer hitl github: could not stash delivered AskCommentID", map[string]any{
				"loopId": input.Loop.ID, "askCommentId": ask.AskCommentID, "error": stashErr.Error(),
			})
		}
	}
	return nil
}

// recoverAskCommentByGeneration finds a previously posted HITL ask comment whose
// marker carries the delivery generation token (crash between remote post and
// local stash write). Returns 0 when none found or listing fails.
func (r *Runner) recoverAskCommentByGeneration(ctx context.Context, input stepInput, prNumber, loopSeq int64, generation, question string) int64 {
	generation = strings.TrimSpace(generation)
	if r.github == nil || generation == "" || prNumber <= 0 {
		return 0
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{
		Repo: firstNonEmpty(input.Repo, derefString(input.Loop.Repo)), PRNumber: prNumber, CWD: input.Project.RepoPath,
	})
	if err != nil {
		return 0
	}
	marker := hitlGitHubAskGenerationMarker(loopSeq, generation)
	question = strings.TrimSpace(question)
	var found int64
	for _, raw := range detail.IssueComments {
		body := strings.TrimSpace(asStringMap(raw, "body"))
		if body == "" || !strings.Contains(body, marker) {
			continue
		}
		if question != "" && !strings.Contains(body, question) {
			continue
		}
		id := issueCommentNumericID(raw)
		if id > found {
			found = id
		}
	}
	return found
}

func asStringMap(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	v, ok := raw[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func issueCommentNumericID(raw map[string]any) int64 {
	if raw == nil {
		return 0
	}
	switch v := raw["id"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

const hitlGitHubAskMarkerPrefix = "<!-- looper:hitl:ask v=1"

func hitlGitHubAskGenerationMarker(loopSeq int64, generation string) string {
	generation = strings.TrimSpace(generation)
	if generation == "" {
		return fmt.Sprintf("%s loop=%d role=fixer", hitlGitHubAskMarkerPrefix, loopSeq)
	}
	return fmt.Sprintf("%s loop=%d role=fixer gen=%s", hitlGitHubAskMarkerPrefix, loopSeq, generation)
}

func buildFixerGitHubAskComment(loopSeq int64, generation, question string, options []string, mentionLogins []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s -->\n", hitlGitHubAskGenerationMarker(loopSeq, generation))
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
