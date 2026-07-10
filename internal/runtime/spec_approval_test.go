package runtime

import "testing"

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
		a, d, u, _ := loopSpecApprovalState(tc.meta)
		if a != tc.wantAwaiting || d != tc.wantDispatched || u != tc.wantIssueURL {
			t.Fatalf("%s: got (%v,%v,%q); want (%v,%v,%q)", tc.name, a, d, u, tc.wantAwaiting, tc.wantDispatched, tc.wantIssueURL)
		}
	}
}
