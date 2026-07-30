// Package resolution holds the reviewer's thread-resolution decision
// authority: which unresolved review threads are candidates, what the
// classifier is asked and how its output is parsed, when a decision may
// comment or resolve under each policy mode, the audit-marker contract
// that gates destructive resolution, and the published reply bodies. It
// has no I/O — the reviewer package owns GitHub access, the agent
// executor, and checkpoint state, and calls into this package to decide
// what the fetched state means. Reviewer slice under issue #120, following
// internal/reviewer/workflow (#131) and internal/reviewer/publish (#344).
package resolution

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/config"
)

// Thread is one PR review thread as fetched from the forge.
type Thread struct {
	ID         string
	IsResolved bool
	Path       string
	Line       int64
	URL        string
	Comments   []Comment
}

// Comment is one comment inside a review thread.
type Comment struct {
	ID                string
	Body              string
	Author            string
	CreatedAt         string
	UpdatedAt         string
	Path              string
	Line              int64
	OriginalCommitOID string
	CommitOID         string
	URL               string
}

// Decision is the classifier's verdict for one thread.
type Decision struct {
	ThreadID   string `json:"threadId"`
	Decision   string `json:"decision"`
	Evidence   string `json:"evidence"`
	Confidence string `json:"confidence"`
}

// Output is the classifier's full JSON output contract.
type Output struct {
	Decisions []Decision `json:"decisions"`
}

// Candidate reports whether a thread is eligible for this run's
// resolution pass: unresolved, non-empty, inside the policy's authorship
// scope, carrying feedback older than the current head when the policy
// demands new commits, and not already audited for this head (except in
// resolve-objective mode, where the audit is what authorizes resolution
// and is checked per decision instead).
func Candidate(thread Thread, headSHA, currentLogin string, policy config.ReviewerThreadResolutionConfig) bool {
	if thread.IsResolved || thread.ID == "" || len(thread.Comments) == 0 {
		return false
	}
	first := thread.Comments[0]
	if policy.Scope == config.ReviewerThreadResolutionScopeLooperAuthoredOnly && normalizeLogin(first.Author) != currentLogin {
		return false
	}
	if policy.Scope == config.ReviewerThreadResolutionScopeLooperAuthoredOnly && !LooperAuthored(thread) {
		return false
	}
	if policy.RequireNewHeadSinceThread && headSHA != "" {
		threadSHA := LatestFeedbackCommitOID(thread)
		if threadSHA == "" || threadSHA == headSHA {
			return false
		}
	}
	if policy.Mode != config.ReviewerThreadResolutionModeResolveObjective && HasAuditForHead(thread, headSHA) {
		return false
	}
	return true
}

// ShouldComment reports whether the decision warrants a reply under the
// policy mode: advisory modes always reply, resolve-objective replies
// only to lay down the audit trail for an objective decision.
func ShouldComment(policy config.ReviewerThreadResolutionConfig, decision Decision) bool {
	switch policy.Mode {
	case config.ReviewerThreadResolutionModeCommentOnly, config.ReviewerThreadResolutionModeSuggestResolution:
		return true
	case config.ReviewerThreadResolutionModeResolveObjective:
		return policy.RequireAuditComment && IsObjective(decision)
	default:
		return false
	}
}

// ShouldResolve reports whether the decision authorizes actually
// resolving the thread: only resolve-objective mode with objective-only
// auto-resolve and a required audit comment, and only for an objective
// high-confidence decision.
func ShouldResolve(policy config.ReviewerThreadResolutionConfig, decision Decision) bool {
	return policy.Mode == config.ReviewerThreadResolutionModeResolveObjective && policy.AutoResolve == config.ReviewerThreadResolutionAutoResolveObjectiveOnly && policy.RequireAuditComment && IsObjective(decision)
}

// IsObjective reports an objectively-fixed decision held with high
// confidence — the only combination allowed to drive resolution.
func IsObjective(decision Decision) bool {
	return strings.EqualFold(strings.TrimSpace(decision.Decision), "objectively_fixed") && strings.EqualFold(strings.TrimSpace(decision.Confidence), "high")
}

// AuditMarker is the hidden audit line appended to every resolution
// reply. Its exact shape is a contract: HasAuditForHead and
// HasObjectiveAuditForHead scan existing comments for these substrings,
// and replies already posted in the wild carry them.
func AuditMarker(threadID, headSHA, decisionValue string) string {
	return fmt.Sprintf("<!-- looper:thread-resolution thread=%s head=%s decision=%s -->", threadID, headSHA, decisionValue)
}

// Reply renders the thread reply for a decision under the policy mode,
// ending with the audit marker.
func Reply(threadID, headSHA string, decision Decision, policy config.ReviewerThreadResolutionConfig) string {
	evidence := strings.TrimSpace(decision.Evidence)
	if evidence == "" {
		evidence = "the current head"
	}
	decisionValue := strings.ToLower(strings.TrimSpace(decision.Decision))
	if decisionValue == "" {
		decisionValue = "needs_human"
	}
	marker := AuditMarker(threadID, headSHA, decisionValue)
	if IsObjective(decision) {
		if policy.Mode == config.ReviewerThreadResolutionModeSuggestResolution {
			return fmt.Sprintf("Looper checked this thread against head `%s`. The requested change appears objectively addressed by %s. Please resolve this thread if you agree.\n%s", headSHA, evidence, marker)
		}
		if policy.Mode == config.ReviewerThreadResolutionModeResolveObjective {
			return fmt.Sprintf("Looper checked this thread against head `%s`. The requested change appears objectively addressed by %s, so I’m resolving this thread. Reopen if this still needs discussion.\n%s", headSHA, evidence, marker)
		}
		return fmt.Sprintf("Looper checked this thread against head `%s`. The requested change appears objectively addressed by %s.\n%s", headSHA, evidence, marker)
	}
	return fmt.Sprintf("Looper checked this thread against head `%s`. I could not verify that this thread is objectively resolved: %s.\n%s", headSHA, evidence, marker)
}

// HasAuditForHead reports whether any comment in the thread carries a
// resolution audit for the given head (any decision).
func HasAuditForHead(thread Thread, headSHA string) bool {
	needle := "looper:thread-resolution"
	headNeedle := "head=" + headSHA
	for _, comment := range thread.Comments {
		if strings.Contains(comment.Body, needle) && (headSHA == "" || strings.Contains(comment.Body, headNeedle)) {
			return true
		}
	}
	return false
}

// HasSufficientAuditForDecision reports whether the thread already
// carries the audit trail this decision would lay down: the objective
// audit for an objective decision in resolve-objective mode, any
// head-scoped audit otherwise.
func HasSufficientAuditForDecision(policy config.ReviewerThreadResolutionConfig, thread Thread, headSHA string, decision Decision) bool {
	if policy.Mode == config.ReviewerThreadResolutionModeResolveObjective && IsObjective(decision) {
		return HasObjectiveAuditForHead(thread, thread.ID, headSHA)
	}
	return HasAuditForHead(thread, headSHA)
}

// HasObjectiveAuditForHead reports whether the thread carries an
// objectively-fixed audit for exactly this thread and head — the
// precondition for destructive resolution.
func HasObjectiveAuditForHead(thread Thread, threadID, headSHA string) bool {
	for _, comment := range thread.Comments {
		body := comment.Body
		if strings.Contains(body, "looper:thread-resolution") && strings.Contains(body, "thread="+threadID) && strings.Contains(body, "head="+headSHA) && strings.Contains(body, "decision=objectively_fixed") {
			return true
		}
	}
	return false
}

// LooperAuthored reports whether the thread was opened by a looper
// review, judged by the stamp markers on its first comment.
func LooperAuthored(thread Thread) bool {
	if len(thread.Comments) == 0 {
		return false
	}
	body := thread.Comments[0].Body
	return strings.Contains(body, "looper:stamp") || strings.Contains(body, "looper:review")
}

// LatestFeedbackCommitOID returns the commit the latest human feedback in
// the thread was anchored to, walking backwards past looper's own audit
// replies. "" means the thread carries no anchored feedback at all.
func LatestFeedbackCommitOID(thread Thread) string {
	for i := len(thread.Comments) - 1; i >= 0; i-- {
		comment := thread.Comments[i]
		if strings.Contains(comment.Body, "looper:thread-resolution") {
			continue
		}
		if comment.CommitOID != "" {
			return comment.CommitOID
		}
		if comment.OriginalCommitOID != "" {
			return comment.OriginalCommitOID
		}
	}
	return ""
}

// Prompt builds the classifier prompt: the safety rules and the exact
// output contract ParseOutput expects, with the candidate threads as a
// JSON payload.
func Prompt(repo string, prNumber int64, headSHA string, threads []Thread) string {
	payload, _ := json.MarshalIndent(map[string]any{"repo": repo, "prNumber": prNumber, "headSHA": headSHA, "threads": threads}, "", "  ")
	return strings.TrimSpace(`You are running Looper's reviewer thread reconciliation phase.

Inspect the current worktree and the unresolved pull request review threads in the JSON payload below. Classify whether each requested change is objectively addressed at the current head.

Safety rules:
- The current working directory is Looper's prepared reviewer worktree for this PR and is the canonical local checkout. Reuse it for git fetch, git checkout, diff inspection, and local verification. Do not run gh repo clone, git clone, or create any additional checkout for this PR's base or head repository unless the provided worktree is missing or unusable.
- Return objectively_fixed only for concrete, verifiable code or documentation changes that are present in the worktree.
- Return needs_human for subjective, product, design, security-sensitive, ambiguous, or partially addressed feedback.
- Do not treat an author reply like "fixed" as evidence by itself.
- Do not call GitHub APIs and do not post comments.

Output only valid JSON in this exact shape:
{"decisions":[{"threadId":"<id>","decision":"objectively_fixed|needs_human|not_fixed","evidence":"brief concrete evidence","confidence":"high|medium|low"}]}

Payload:
` + string(payload))
}

// ParseOutput extracts and decodes the classifier's JSON output from raw
// agent stdout, tolerating surrounding prose.
func ParseOutput(stdout string) (Output, error) {
	trimmed := strings.TrimSpace(stdout)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end < start {
		return Output{}, fmt.Errorf("thread resolution classifier did not return JSON")
	}
	var parsed Output
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &parsed); err != nil {
		return Output{}, fmt.Errorf("parse thread resolution classifier output: %w", err)
	}
	return parsed, nil
}

func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}
