package fixer

import (
	"context"
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
