package main

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	reviewed := Classify(PullRequest{
		Number:  1,
		Reviews: []Review{{Author: Author{Login: "codex"}, State: "COMMENTED"}, {Author: Author{Login: "codex"}, State: "APPROVED"}},
	})
	if reviewed.Verdict != VerdictReviewed || len(reviewed.Reviewers) != 1 || reviewed.Reviewers[0] != "codex" {
		t.Fatalf("Classify(reviewed) = %+v, want reviewed by codex deduplicated", reviewed)
	}

	refused := Classify(PullRequest{
		Number:   2,
		Comments: []Comment{{Author: Author{Login: "coderabbitai"}, Body: "> ## Review limit reached\n> wait 30 minutes"}},
	})
	if refused.Verdict != VerdictRefused {
		t.Fatalf("Classify(refused) = %+v, want rate-limit-refused: a refusal notice is not participation", refused)
	}

	// A refusal notice does not downgrade a PR that also got a real review.
	both := Classify(PullRequest{
		Number:   3,
		Reviews:  []Review{{Author: Author{Login: "codex"}}},
		Comments: []Comment{{Body: "Review limit reached"}},
	})
	if both.Verdict != VerdictReviewed {
		t.Fatalf("Classify(review+refusal) = %+v, want reviewed", both)
	}

	bare := Classify(PullRequest{Number: 4, Comments: []Comment{{Body: "ordinary discussion"}}})
	if bare.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(bare) = %+v, want unreviewed", bare)
	}
}

func TestAuditReportAndThreshold(t *testing.T) {
	input := []byte(`[
		{"number": 10, "title": "reviewed change", "additions": 500, "deletions": 100,
		 "reviews": [{"author": {"login": "codex"}, "state": "APPROVED"}]},
		{"number": 11, "title": "big silent change", "additions": 900, "deletions": 141,
		 "comments": [{"author": {"login": "coderabbitai"}, "body": "Review limit reached"}]},
		{"number": 12, "title": "small silent change", "additions": 3, "deletions": 1}
	]`)

	report, flagged, err := Audit(input, 300)
	if err != nil {
		t.Fatalf("Audit() error = %v", err)
	}
	if !flagged {
		t.Fatal("Audit() flagged = false, want true: a 1041-line refused merge meets the 300-line threshold")
	}
	for _, want := range []string{
		"#10\treviewed\t+500/-100\t[codex]\treviewed change",
		"#11\trate-limit-refused\t+900/-141\tLARGE-UNREVIEWED\tbig silent change",
		"#12\tunreviewed\t+3/-1\tsmall silent change",
		"total=3 reviewed=1 rate-limit-refused=1 unreviewed=1",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "#12\tunreviewed\t+3/-1\tLARGE-UNREVIEWED") {
		t.Fatalf("small unreviewed change must not be flagged large:\n%s", report)
	}

	// Threshold 0 reports without failing.
	_, flagged, err = Audit(input, 0)
	if err != nil || flagged {
		t.Fatalf("Audit(threshold 0) = flagged %t err %v, want informational", flagged, err)
	}

	if _, _, err := Audit([]byte("not json"), 0); err == nil {
		t.Fatal("Audit(bad input) must error")
	}
}
