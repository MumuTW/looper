package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

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

// TestHandleTerminalExhaustionEscalationDeduplicatesContextWithheldEventAcrossReplay
// covers the unbounded-growth defect: when both comment bodies are rejected and
// the subsequent label mutation fails, the handoff is requeued with Commented
// and Escalated still false. Without deduplication every replay would re-enter
// escalateWithoutFailureContext and append another fixer.escalation.context_withheld
// event. The event-log primary key, derived from the loop ID, ensures only the
// first rejection records the event even when the metadata checkpoint is lost.
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
	updated, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	state, ok := parseRegenerationState(parseJSONObject(updated.MetadataJSON))
	if !ok || state.Escalated {
		t.Fatalf("regeneration state after first attempt = %#v, want escalation still incomplete", state)
	}

	// Replay: the label service recovers, both comment bodies are still rejected.
	// The deterministic event ID keeps the audit history deduplicated across
	// the replay without adding another durable regeneration-state field.
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
	events, err := fixture.repos.Events.ListByEntityAndEventTypes(ctx, "loop", loop.ID, []string{"fixer.escalation.context_withheld"})
	if err != nil {
		t.Fatalf("Events.ListByEntityAndEventTypes() error = %v", err)
	}
	if events[0].ID != "fixer.escalation.context_withheld:"+loop.ID {
		t.Fatalf("context_withheld event ID = %q, want deterministic loop-scoped ID", events[0].ID)
	}
}
