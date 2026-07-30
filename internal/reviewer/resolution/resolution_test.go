package resolution

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func objectiveDecision() Decision {
	return Decision{ThreadID: "t1", Decision: "objectively_fixed", Evidence: "internal/foo.go:12 now checks the error", Confidence: "high"}
}

func resolveObjectivePolicy() config.ReviewerThreadResolutionConfig {
	return config.ReviewerThreadResolutionConfig{
		Mode:                config.ReviewerThreadResolutionModeResolveObjective,
		AutoResolve:         config.ReviewerThreadResolutionAutoResolveObjectiveOnly,
		RequireAuditComment: true,
	}
}

// The audit marker is scanned verbatim by HasAuditForHead and
// HasObjectiveAuditForHead, and replies in the wild carry it. Its shape
// is a published contract.
func TestAuditMarkerShapeIsStable(t *testing.T) {
	t.Parallel()

	got := AuditMarker("t1", "abc123", "objectively_fixed")
	want := "<!-- looper:thread-resolution thread=t1 head=abc123 decision=objectively_fixed -->"
	if got != want {
		t.Fatalf("AuditMarker() = %q, want %q", got, want)
	}
}

func TestReplyEndsWithMarkerAndMatchesAuditScan(t *testing.T) {
	t.Parallel()

	policy := resolveObjectivePolicy()
	body := Reply("t1", "abc123", objectiveDecision(), policy)
	if !strings.HasSuffix(body, AuditMarker("t1", "abc123", "objectively_fixed")) {
		t.Fatalf("Reply must end with the audit marker:\n%s", body)
	}
	if !strings.Contains(body, "so I’m resolving this thread") {
		t.Fatalf("resolve-objective reply must announce the resolution:\n%s", body)
	}

	// The rendered reply, posted back as a comment, must satisfy the audit
	// scans that gate a later resolution pass — the round-trip the step
	// relies on.
	thread := Thread{ID: "t1", Comments: []Comment{{Body: body}}}
	if !HasAuditForHead(thread, "abc123") {
		t.Fatal("posted reply does not satisfy HasAuditForHead for its own head")
	}
	if !HasObjectiveAuditForHead(thread, "t1", "abc123") {
		t.Fatal("posted objective reply does not satisfy HasObjectiveAuditForHead")
	}
	if HasObjectiveAuditForHead(thread, "t1", "otherhead") {
		t.Fatal("audit must be head-scoped")
	}
	if HasObjectiveAuditForHead(thread, "t2", "abc123") {
		t.Fatal("audit must be thread-scoped")
	}
}

func TestReplyVariants(t *testing.T) {
	t.Parallel()

	suggest := config.ReviewerThreadResolutionConfig{Mode: config.ReviewerThreadResolutionModeSuggestResolution}
	if body := Reply("t1", "h", objectiveDecision(), suggest); !strings.Contains(body, "Please resolve this thread if you agree.") {
		t.Fatalf("suggest-mode reply wrong:\n%s", body)
	}
	commentOnly := config.ReviewerThreadResolutionConfig{Mode: config.ReviewerThreadResolutionModeCommentOnly}
	if body := Reply("t1", "h", objectiveDecision(), commentOnly); strings.Contains(body, "resolving this thread") || strings.Contains(body, "Please resolve") {
		t.Fatalf("comment-only reply must not suggest or announce resolution:\n%s", body)
	}

	notFixed := Decision{ThreadID: "t1", Decision: "not_fixed", Confidence: "high"}
	body := Reply("t1", "h", notFixed, commentOnly)
	if !strings.Contains(body, "could not verify") {
		t.Fatalf("non-objective reply wrong:\n%s", body)
	}
	if !strings.Contains(body, "the current head") {
		t.Fatalf("empty evidence must fall back to a neutral phrase:\n%s", body)
	}
	if !strings.HasSuffix(body, AuditMarker("t1", "h", "not_fixed")) {
		t.Fatalf("non-objective reply must carry its decision in the marker:\n%s", body)
	}

	// Empty decision value defaults to needs_human in the marker.
	if body := Reply("t1", "h", Decision{}, commentOnly); !strings.HasSuffix(body, AuditMarker("t1", "h", "needs_human")) {
		t.Fatalf("empty decision must default to needs_human:\n%s", body)
	}
}

func TestIsObjective(t *testing.T) {
	t.Parallel()

	if !IsObjective(Decision{Decision: " Objectively_Fixed ", Confidence: "HIGH"}) {
		t.Fatal("IsObjective must be case- and space-insensitive")
	}
	if IsObjective(Decision{Decision: "objectively_fixed", Confidence: "medium"}) {
		t.Fatal("medium confidence must not be objective")
	}
	if IsObjective(Decision{Decision: "needs_human", Confidence: "high"}) {
		t.Fatal("needs_human must not be objective")
	}
}

func TestShouldCommentAndShouldResolveModeMatrix(t *testing.T) {
	t.Parallel()

	objective := objectiveDecision()
	subjective := Decision{Decision: "needs_human", Confidence: "high"}

	commentOnly := config.ReviewerThreadResolutionConfig{Mode: config.ReviewerThreadResolutionModeCommentOnly}
	suggest := config.ReviewerThreadResolutionConfig{Mode: config.ReviewerThreadResolutionModeSuggestResolution}
	resolve := resolveObjectivePolicy()

	if !ShouldComment(commentOnly, subjective) || !ShouldComment(suggest, subjective) {
		t.Fatal("advisory modes must always comment")
	}
	if !ShouldComment(resolve, objective) {
		t.Fatal("resolve-objective must comment to lay down the audit for an objective decision")
	}
	if ShouldComment(resolve, subjective) {
		t.Fatal("resolve-objective must not comment on subjective decisions")
	}
	noAudit := resolve
	noAudit.RequireAuditComment = false
	if ShouldComment(noAudit, objective) {
		t.Fatal("resolve-objective without required audit must not comment")
	}
	if ShouldComment(config.ReviewerThreadResolutionConfig{Mode: "unknown"}, objective) {
		t.Fatal("unknown mode must not comment")
	}

	if !ShouldResolve(resolve, objective) {
		t.Fatal("resolve-objective with objective-only auto-resolve and audit must resolve an objective decision")
	}
	for name, p := range map[string]config.ReviewerThreadResolutionConfig{
		"comment-only":     commentOnly,
		"suggest":          suggest,
		"no-audit-comment": noAudit,
	} {
		if ShouldResolve(p, objective) {
			t.Fatalf("%s policy must never resolve", name)
		}
	}
	if ShouldResolve(resolve, subjective) {
		t.Fatal("subjective decisions must never resolve")
	}
}

func TestCandidate(t *testing.T) {
	t.Parallel()

	base := Thread{ID: "t1", Comments: []Comment{{Author: "looper", Body: "<!-- looper:review --> finding", CommitOID: "old"}}}
	open := config.ReviewerThreadResolutionConfig{}

	if Candidate(Thread{}, "h", "looper", open) {
		t.Fatal("empty thread must not be a candidate")
	}
	resolved := base
	resolved.IsResolved = true
	if Candidate(resolved, "h", "looper", open) {
		t.Fatal("resolved thread must not be a candidate")
	}
	if !Candidate(base, "h", "looper", open) {
		t.Fatal("unresolved thread with comments must be a candidate under an open policy")
	}

	scoped := config.ReviewerThreadResolutionConfig{Scope: config.ReviewerThreadResolutionScopeLooperAuthoredOnly}
	if !Candidate(base, "h", "looper", scoped) {
		t.Fatal("looper-authored thread must pass the authorship scope")
	}
	if Candidate(base, "h", "someoneelse", scoped) {
		t.Fatal("author mismatch must fail the authorship scope")
	}
	foreign := Thread{ID: "t1", Comments: []Comment{{Author: "looper", Body: "manual comment"}}}
	if Candidate(foreign, "h", "looper", scoped) {
		t.Fatal("thread without looper stamps must fail the authorship scope")
	}

	newHead := config.ReviewerThreadResolutionConfig{RequireNewHeadSinceThread: true}
	if Candidate(base, "old", "looper", newHead) {
		t.Fatal("feedback anchored at the current head must not be a candidate when new commits are required")
	}
	if !Candidate(base, "newhead", "looper", newHead) {
		t.Fatal("feedback anchored at an older commit must be a candidate")
	}
	unanchored := Thread{ID: "t1", Comments: []Comment{{Author: "looper", Body: "x"}}}
	if Candidate(unanchored, "newhead", "looper", newHead) {
		t.Fatal("unanchored feedback must not be a candidate when new commits are required")
	}

	audited := Thread{ID: "t1", Comments: []Comment{{Body: "finding"}, {Body: AuditMarker("t1", "h", "needs_human")}}}
	if Candidate(audited, "h", "looper", open) {
		t.Fatal("already-audited thread must not be re-processed outside resolve-objective mode")
	}
	if !Candidate(audited, "h", "looper", resolveObjectivePolicy()) {
		t.Fatal("resolve-objective mode defers the audit check to the per-decision gate")
	}
}

func TestLatestFeedbackCommitOID(t *testing.T) {
	t.Parallel()

	thread := Thread{Comments: []Comment{
		{Body: "first", CommitOID: "c1"},
		{Body: "second", OriginalCommitOID: "c2"},
		{Body: "audit " + AuditMarker("t", "h", "needs_human"), CommitOID: "c3"},
	}}
	if got := LatestFeedbackCommitOID(thread); got != "c2" {
		t.Fatalf("LatestFeedbackCommitOID() = %q, want c2 (walks backwards, skips looper audits, prefers CommitOID then OriginalCommitOID)", got)
	}
	if got := LatestFeedbackCommitOID(Thread{}); got != "" {
		t.Fatalf("LatestFeedbackCommitOID(empty) = %q, want empty", got)
	}
}

func TestPromptAndParseOutputRoundTrip(t *testing.T) {
	t.Parallel()

	prompt := Prompt("acme/looper", 42, "abc", []Thread{{ID: "t1"}})
	for _, needle := range []string{`"prNumber": 42`, `"headSHA": "abc"`, "objectively_fixed|needs_human|not_fixed", "Do not call GitHub APIs"} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("Prompt missing %q", needle)
		}
	}

	out, err := ParseOutput("agent prose before\n{\"decisions\":[{\"threadId\":\"t1\",\"decision\":\"objectively_fixed\",\"evidence\":\"e\",\"confidence\":\"high\"}]}\nprose after")
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if len(out.Decisions) != 1 || out.Decisions[0].ThreadID != "t1" || !IsObjective(out.Decisions[0]) {
		t.Fatalf("ParseOutput() = %+v", out)
	}

	if _, err := ParseOutput("no json here"); err == nil {
		t.Fatal("ParseOutput(no JSON) must error")
	}
	if _, err := ParseOutput("{broken"); err == nil {
		t.Fatal("ParseOutput(broken JSON) must error")
	}
}
