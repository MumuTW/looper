package discovery

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func openPR() PR {
	return PR{State: "OPEN", Author: "alice", Labels: []string{"fixer"}}
}

func anyAuthorPolicy() Policy {
	return Policy{IncludeDrafts: false, AuthorFilter: config.FixerAuthorFilterAny}
}

func TestEligiblePrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		pr          PR
		currentUser string
		policy      Policy
		facts       Facts
		want        bool
	}{
		{
			name:  "open PR under an open policy is eligible",
			pr:    openPR(),
			facts: Facts{},
			want:  true,
		},
		{
			name:  "non-open state is never eligible even with a manual follow-up",
			pr:    PR{State: "MERGED"},
			facts: Facts{HasManualFollowupLoop: true},
			want:  false,
		},
		{
			name:  "draft is gated when the policy excludes drafts",
			pr:    PR{State: "open", IsDraft: true},
			facts: Facts{},
			want:  false,
		},
		{
			name:   "draft passes when the policy includes drafts",
			pr:     PR{State: "open", IsDraft: true},
			policy: Policy{IncludeDrafts: true, AuthorFilter: config.FixerAuthorFilterAny},
			facts:  Facts{},
			want:   true,
		},
		{
			name:  "manual follow-up waives the draft gate",
			pr:    PR{State: "open", IsDraft: true},
			facts: Facts{HasManualFollowupLoop: true},
			want:  true,
		},
		{
			name:  "held lock without a running loop blocks even a manual follow-up",
			pr:    openPR(),
			facts: Facts{HasManualFollowupLoop: true, LockHeld: true},
			want:  false,
		},
		{
			name:  "held lock with a manual loop lacking follow-updates blocks",
			pr:    openPR(),
			facts: Facts{LockHeld: true, HasRunningLoop: true, RunningLoopManual: true},
			want:  false,
		},
		{
			name:  "held lock with a manual loop with follow-updates passes",
			pr:    openPR(),
			facts: Facts{LockHeld: true, HasRunningLoop: true, RunningLoopManual: true, RunningLoopFollowUpdates: true},
			want:  true,
		},
		{
			name:  "held lock with an automatic running loop passes",
			pr:    openPR(),
			facts: Facts{LockHeld: true, HasRunningLoop: true},
			want:  true,
		},
		{
			name:        "manual follow-up bypasses the author filter and label matching",
			pr:          PR{State: "open", Author: "someone-else", Labels: nil},
			currentUser: "looper",
			policy:      Policy{AuthorFilter: config.FixerAuthorFilterCurrentUser, Labels: []string{"required"}, LabelMode: config.LabelModeAll},
			facts:       Facts{HasManualFollowupLoop: true},
			want:        true,
		},
		{
			name:        "author filter rejects a foreign author",
			pr:          PR{State: "open", Author: "someone-else"},
			currentUser: "looper",
			policy:      Policy{AuthorFilter: config.FixerAuthorFilterCurrentUser},
			facts:       Facts{},
			want:        false,
		},
		{
			name:        "author filter accepts a case-insensitive self match",
			pr:          PR{State: "open", Author: "Looper"},
			currentUser: "looper",
			policy:      Policy{AuthorFilter: config.FixerAuthorFilterCurrentUser},
			facts:       Facts{},
			want:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := tc.policy
			if policy.AuthorFilter == "" {
				policy = anyAuthorPolicy()
				policy.IncludeDrafts = tc.policy.IncludeDrafts
			}
			if got := Eligible(tc.pr, tc.currentUser, policy, tc.facts); got != tc.want {
				t.Fatalf("Eligible(%+v, %q, %+v, %+v) = %t, want %t", tc.pr, tc.currentUser, policy, tc.facts, got, tc.want)
			}
		})
	}
}

func TestEligibleLabelMatching(t *testing.T) {
	t.Parallel()

	policy := Policy{AuthorFilter: config.FixerAuthorFilterAny, Labels: []string{"fixer", "ready"}, LabelMode: config.LabelModeAll}
	pr := PR{State: "open", Labels: []string{"fixer"}}
	if Eligible(pr, "", policy, Facts{}) {
		t.Fatal("missing required label must reject under all-mode matching")
	}
	pr.Labels = []string{"fixer", "ready", "extra"}
	if !Eligible(pr, "", policy, Facts{}) {
		t.Fatal("all required labels present must pass")
	}
}
