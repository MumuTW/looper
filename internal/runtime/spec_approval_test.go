package runtime

import (
	"testing"
	"time"
)

func TestLoopSpecApprovalState(t *testing.T) {
	cases := []struct {
		name           string
		meta           *string
		wantAwaiting   bool
		wantDispatched bool
		wantIssueURL   string
	}{
		{"nil meta", nil, false, false, ""},
		{"awaiting, not dispatched", strPtr(`{"awaitingSpecApproval":true,"issueUrl":"https://plane.x/w/projects/p/issues/wi-1"}`), true, false, "https://plane.x/w/projects/p/issues/wi-1"},
		{"awaiting + dispatched", strPtr(`{"awaitingSpecApproval":true,"specApprovedDispatched":true,"issueUrl":"u"}`), true, true, "u"},
		{"not awaiting", strPtr(`{"issueUrl":"u"}`), false, false, "u"},
		{"issueURL alt casing", strPtr(`{"awaitingSpecApproval":true,"issueURL":"u2"}`), true, false, "u2"},
	}
	for _, tc := range cases {
		a, d, u := loopSpecApprovalState(tc.meta)
		if a != tc.wantAwaiting || d != tc.wantDispatched || u != tc.wantIssueURL {
			t.Fatalf("%s: got (%v,%v,%q); want (%v,%v,%q)", tc.name, a, d, u, tc.wantAwaiting, tc.wantDispatched, tc.wantIssueURL)
		}
	}
}

func TestLoopSpecApprovalNudgeState(t *testing.T) {
	cases := []struct {
		name       string
		meta       *string
		wantSince  string
		wantNudged bool
	}{
		{"nil", nil, "", false},
		{"since only", strPtr(`{"awaitingSpecApprovalSince":"2026-07-07T10:00:00Z"}`), "2026-07-07T10:00:00Z", false},
		{"since + nudged", strPtr(`{"awaitingSpecApprovalSince":"2026-07-07T10:00:00Z","awaitingSpecNudged":true}`), "2026-07-07T10:00:00Z", true},
	}
	for _, tc := range cases {
		s, n := loopSpecApprovalNudgeState(tc.meta)
		if s != tc.wantSince || n != tc.wantNudged {
			t.Fatalf("%s: got (%q,%v); want (%q,%v)", tc.name, s, n, tc.wantSince, tc.wantNudged)
		}
	}
}

func TestSpecApprovalNudgeThreshold(t *testing.T) {
	t.Setenv("LOOPER_SPEC_NUDGE_MINUTES", "2")
	if got := specApprovalNudgeThreshold(); got != 2*time.Minute {
		t.Fatalf("threshold = %v, want 2m", got)
	}
	t.Setenv("LOOPER_SPEC_NUDGE_MINUTES", "")
	if got := specApprovalNudgeThreshold(); got != 15*time.Minute {
		t.Fatalf("default threshold = %v, want 15m", got)
	}
}
