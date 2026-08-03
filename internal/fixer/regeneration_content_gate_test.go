package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/outboundguard"
)

// contentGateRejection is the error shape the outbound content safety gate
// returns for the escalation comment body.
func contentGateRejection() error {
	return &outboundguard.Rejection{Field: "issue comment body", Reason: "contains a high-entropy credential-shaped token"}
}

// TestHandleTerminalExhaustionEscalationSurvivesContentGateRejection covers the
// first defect in issue #547: the escalation body embeds the durable
// FailureContext, so a content-gate rejection is deterministic and must not be
// returned as a retryable failure. The fallback comment keeps the authority
// marker, withholds the context, and the escalation still lands.
func TestHandleTerminalExhaustionEscalationSurvivesContentGateRejection(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
		commentErrs:       []error{contentGateRejection()},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")

	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err != nil {
		t.Fatalf("handleTerminalExhaustion() error = %v, want the deterministic rejection handled in-place", err)
	}
	if action != regenerationEscalated {
		t.Fatalf("action = %q, want escalation completed despite the rejected comment", action)
	}
	if len(gateway.createIssueComments) != 2 {
		t.Fatalf("comment attempts = %d, want the rejected original plus the fallback", len(gateway.createIssueComments))
	}
	fallback := gateway.createIssueComments[1].Body
	if !strings.Contains(fallback, regenerationCommentMarker) || !strings.Contains(fallback, "outcome=escalated") {
		t.Fatalf("fallback body = %q, want the dedupe authority marker", fallback)
	}
	if !strings.Contains(fallback, "Failure context withheld") {
		t.Fatalf("fallback body = %q, want the withheld-context notice", fallback)
	}
	if len(gateway.addLabelCalls) != 1 || gateway.addLabelCalls[0].Labels[0] != labels.NeedsHuman {
		t.Fatalf("PR labels = %#v, want needs-human applied after the fallback", gateway.addLabelCalls)
	}

	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON))
	if !ok || !state.Escalated || !state.Commented {
		t.Fatalf("regeneration state = %#v, want escalated with the fallback comment checkpointed", state)
	}
}

// TestHandleTerminalExhaustionEscalationCompletesWhenFallbackAlsoRejected
// covers the bounded degrade: if even the daemon-composed fallback body is
// rejected, the comment is omitted but the needs-human label still lands and
// the loop reaches its terminal escalated state instead of retrying.
func TestHandleTerminalExhaustionEscalationCompletesWhenFallbackAlsoRejected(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
		commentErrs:       []error{contentGateRejection(), contentGateRejection()},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")

	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err != nil {
		t.Fatalf("handleTerminalExhaustion() error = %v, want the escalation to complete without a comment", err)
	}
	if action != regenerationEscalated {
		t.Fatalf("action = %q, want escalation completed with the comment omitted", action)
	}
	if len(gateway.createIssueComments) != 2 {
		t.Fatalf("comment attempts = %d, want the rejected original plus the rejected fallback", len(gateway.createIssueComments))
	}
	if len(gateway.addLabelCalls) != 1 || gateway.addLabelCalls[0].Labels[0] != labels.NeedsHuman {
		t.Fatalf("PR labels = %#v, want needs-human applied even without a comment", gateway.addLabelCalls)
	}

	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON))
	if !ok || !state.Escalated || state.Commented {
		t.Fatalf("regeneration state = %#v, want escalated without a comment", state)
	}
}

// TestHandleTerminalExhaustionEscalationRetriesNonGateFallbackFailure covers
// the boundary between the two fallback failures: only a content-gate
// rejection of the fallback body completes the escalation without a comment.
// A forge outage or transport error on the fallback comment is transient and
// must propagate so the run is retried with the comment still pending —
// swallowing it would permanently drop the explanation for a loop that could
// otherwise have published it.
func TestHandleTerminalExhaustionEscalationRetriesNonGateFallbackFailure(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
		commentErrs:       []error{contentGateRejection(), errors.New("connection refused")},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")

	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err == nil {
		t.Fatalf("handleTerminalExhaustion() action = %q, want the transport failure on the fallback comment propagated for retry", action)
	}
	if len(gateway.addLabelCalls) != 0 {
		t.Fatalf("PR labels = %#v, want no escalation side effects before the comment is settled", gateway.addLabelCalls)
	}

	updated, err2 := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err2 != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err2)
	}
	if state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON)); ok && state.Escalated {
		t.Fatalf("regeneration state = %#v, want the loop left un-escalated for retry", state)
	}
}

// TestHandleTerminalExhaustionRegenerationSurvivesContentGateRejection covers
// the normal close-and-regenerate path: a rejected detailed comment must get
// one daemon-composed retry, then continue through the durable close and
// Planner handoff checkpoints when that retry is accepted.
func TestHandleTerminalExhaustionRegenerationSurvivesContentGateRejection(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, Title: "Original issue", URL: "https://example.test/issues/7", State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
		commentErrs:       []error{contentGateRejection()},
	}
	routed := false
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error {
		routed = true
		return nil
	})
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	failure := &runpipe.LoopError{Message: "validation failed with secret-shaped context", Kind: runpipe.FailureRetryableTransient}

	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, failure)
	if err != nil {
		t.Fatalf("handleTerminalExhaustion() error = %v, want close-and-regenerate to continue after the rejected comment", err)
	}
	if action != regenerationCompleted {
		t.Fatalf("action = %q, want completed after fallback comment", action)
	}
	if !routed {
		t.Fatal("Planner route was not called after the fallback comment")
	}
	if len(gateway.createIssueComments) != 2 {
		t.Fatalf("comment attempts = %d, want the rejected original plus the fallback", len(gateway.createIssueComments))
	}
	fallback := gateway.createIssueComments[1].Body
	if !strings.Contains(fallback, regenerationCommentMarker) || !strings.Contains(fallback, "Failure context withheld") {
		t.Fatalf("fallback body = %q, want the authority marker and withheld-context notice", fallback)
	}
	if strings.Contains(fallback, failure.Message) {
		t.Fatalf("fallback body = %q, must not echo rejected failure context", fallback)
	}
	if len(gateway.closeCalls) != 1 {
		t.Fatalf("close calls = %d, want one close after the fallback", len(gateway.closeCalls))
	}

	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON))
	if !ok || !state.Commented || !state.Closed || !state.Routed {
		t.Fatalf("regeneration state = %#v, want commented, closed, and routed", state)
	}
}

// TestHandleTerminalExhaustionRegenerationCompletesWhenFallbackAlsoRejected
// keeps a second content-gate rejection from turning the optional comment into
// a lifecycle blocker. Close and Planner routing remain the durable authority.
func TestHandleTerminalExhaustionRegenerationCompletesWhenFallbackAlsoRejected(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
		commentErrs:       []error{contentGateRejection(), contentGateRejection()},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")

	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err != nil {
		t.Fatalf("handleTerminalExhaustion() error = %v, want lifecycle completion after both comment bodies are rejected", err)
	}
	if action != regenerationCompleted {
		t.Fatalf("action = %q, want completed with the comment omitted", action)
	}
	if len(gateway.createIssueComments) != 2 {
		t.Fatalf("comment attempts = %d, want the rejected original plus the rejected fallback", len(gateway.createIssueComments))
	}
	if len(gateway.closeCalls) != 1 {
		t.Fatalf("close calls = %d, want one close after the rejected fallback", len(gateway.closeCalls))
	}

	updated, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON))
	if !ok || !state.Commented || !state.Closed || !state.Routed {
		t.Fatalf("regeneration state = %#v, want settled comment, closed, and routed checkpoints", state)
	}
}

// TestHandleTerminalExhaustionEscalationDeduplicatesContextWithheldEventAcrossReplay
// covers the unbounded-growth defect: when both comment bodies are rejected and
// the subsequent label mutation fails, the handoff is requeued with Commented
// and Escalated still false. Without deduplication every replay would re-enter
// escalateWithoutFailureContext and append another fixer.escalation.context_withheld
// event. The event-log primary key, derived from the loop and escalation
// identity, ensures only the first rejection for that exact target records the
// event even when the metadata checkpoint is lost.
func TestHandleTerminalExhaustionEscalationDeduplicatesContextWithheldEventAcrossReplay(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
		commentErrs:       []error{contentGateRejection(), contentGateRejection()},
		prLabelErr:        errors.New("label service outage"),
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	failure := &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient}
	ctx := context.Background()

	// First attempt: both comment bodies are rejected, then the label mutation
	// fails, so the escalation does not complete and the handoff is requeued.
	if _, err := runner.handleTerminalExhaustion(ctx, *project, loop, queue, fixerCheckpoint{}, failure); err == nil {
		t.Fatalf("first handleTerminalExhaustion() error = nil, want the label outage propagated for requeue")
	}
	withheldEvents := func() int {
		events, err := fixture.repos.Events.ListByEntityAndEventTypes(ctx, "loop", loop.ID, []string{"fixer.escalation.context_withheld"})
		if err != nil {
			t.Fatalf("Events.ListByEntityAndEventTypes() error = %v", err)
		}
		return len(events)
	}
	if got := withheldEvents(); got != 1 {
		t.Fatalf("context_withheld events after first attempt = %d, want exactly one", got)
	}
	if got := len(gateway.createIssueComments); got != 2 {
		t.Fatalf("comment attempts after first attempt = %d, want the rejected original plus rejected fallback", got)
	}
	updated, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON))
	if !ok || state.Escalated {
		t.Fatalf("regeneration state after first attempt = %#v, want escalation still incomplete", state)
	}

	// Replay: the label service recovers, both comment bodies are still rejected.
	// The deterministic event ID is both the durable audit marker and the
	// settled-comment authority, so replay must not resubmit either body.
	gateway.prLabelErr = nil
	gateway.commentErrs = []error{contentGateRejection(), contentGateRejection()}
	action, err := runner.handleTerminalExhaustion(ctx, *project, loop, queue, fixerCheckpoint{}, failure)
	if err != nil {
		t.Fatalf("second handleTerminalExhaustion() error = %v, want the escalation to complete on replay", err)
	}
	if action != regenerationEscalated {
		t.Fatalf("action = %q, want escalation completed on replay", action)
	}
	if got := withheldEvents(); got != 1 {
		t.Fatalf("context_withheld events after replay = %d, want still one (deduplicated across replay)", got)
	}
	if got := len(gateway.createIssueComments); got != 2 {
		t.Fatalf("comment attempts after replay = %d, want no duplicate attempts after withheld step was settled", got)
	}
	events, err := fixture.repos.Events.ListByEntityAndEventTypes(ctx, "loop", loop.ID, []string{"fixer.escalation.context_withheld"})
	if err != nil {
		t.Fatalf("Events.ListByEntityAndEventTypes() error = %v", err)
	}
	wantEventID := contextWithheldEventID(loop.ID, "acme/looper", 42, "pull request contains a human-authored commit")
	if events[0].ID != wantEventID {
		t.Fatalf("context_withheld event ID = %q, want deterministic escalation-scoped ID %q", events[0].ID, wantEventID)
	}
}

func TestHandleTerminalExhaustionIgnoresStaleContextWithheldEvent(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
		commentErrs:       []error{contentGateRejection(), contentGateRejection()},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	ctx := context.Background()

	// This legacy marker belongs to a different escalation target and reason.
	// Matching only its loop-scoped ID would suppress the current escalation's
	// comment attempts after the target or reason changed during replay.
	if err := eventlog.Append(ctx, fixture.repos, eventlog.AppendInput{
		ID:         legacyContextWithheldEventID(loop.ID),
		EventType:  contextWithheldEventType,
		ProjectID:  runpipe.OptionalString(loop.ProjectID),
		LoopID:     runpipe.OptionalString(loop.ID),
		EntityType: runpipe.OptionalString("loop"),
		EntityID:   runpipe.OptionalString(loop.ID),
		Payload:    map[string]any{"repo": "stale/looper", "prNumber": int64(41), "reason": "stale escalation reason"},
		CreatedAt:  fixture.now(),
	}); err != nil {
		t.Fatalf("append stale context-withheld event: %v", err)
	}

	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	action, err := runner.handleTerminalExhaustion(ctx, *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err != nil {
		t.Fatalf("handleTerminalExhaustion() error = %v", err)
	}
	if action != regenerationEscalated {
		t.Fatalf("action = %q, want escalation completed", action)
	}
	if got := len(gateway.createIssueComments); got != 2 {
		t.Fatalf("comment attempts = %d, want the current original plus fallback despite stale evidence", got)
	}

	events, err := fixture.repos.Events.ListByEntityAndEventTypes(ctx, "loop", loop.ID, []string{contextWithheldEventType})
	if err != nil {
		t.Fatalf("list context-withheld events: %v", err)
	}
	wantEventID := contextWithheldEventID(loop.ID, "acme/looper", 42, "pull request contains a human-authored commit")
	foundCurrent := false
	for _, event := range events {
		if event.ID == wantEventID {
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		t.Fatalf("context-withheld events = %#v, want a new event scoped to the current target and reason", events)
	}
}
