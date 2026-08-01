package github

import "testing"

func TestPullRequestDiffStatsFromRowParsesGHAndRESTFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		row  map[string]any
		want *PullRequestDiffStats
	}{
		{name: "gh view", row: map[string]any{"changedFiles": float64(3), "deletions": float64(2)}, want: &PullRequestDiffStats{ChangedFiles: 3, Deletions: 2}},
		{name: "rest", row: map[string]any{"changed_files": float64(4), "deletions": float64(5)}, want: &PullRequestDiffStats{ChangedFiles: 4, Deletions: 5}},
		{name: "missing field", row: map[string]any{"changedFiles": float64(3)}, want: nil},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pullRequestDiffStatsFromRow(tc.row)
			if got == nil || tc.want == nil {
				if got != nil || tc.want != nil {
					t.Fatalf("pullRequestDiffStatsFromRow() = %#v, want %#v", got, tc.want)
				}
				return
			}
			if *got != *tc.want {
				t.Fatalf("pullRequestDiffStatsFromRow() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
