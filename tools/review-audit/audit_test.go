package main

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	reviewed := Classify(PullRequest{
		Number:     1,
		Author:     Author{Login: "maintainer"},
		HeadRefOID: "head1",
		Reviews: []Review{
			{Author: Author{Login: "codex"}, State: "COMMENTED", Commit: Commit{OID: "head1"}},
			{Author: Author{Login: "codex"}, State: "APPROVED", Commit: Commit{OID: "head1"}},
		},
	})
	if reviewed.Verdict != VerdictReviewed || len(reviewed.Reviewers) != 1 || reviewed.Reviewers[0] != "codex" {
		t.Fatalf("Classify(reviewed) = %+v, want reviewed by codex deduplicated", reviewed)
	}

	// A PENDING draft was never submitted: no scrutiny happened.
	pending := Classify(PullRequest{
		Number:  2,
		Reviews: []Review{{Author: Author{Login: "codex"}, State: "PENDING"}},
	})
	if pending.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(pending-only) = %+v, want unreviewed", pending)
	}

	// A dismissed review was explicitly invalidated and cannot prove scrutiny.
	dismissed := Classify(PullRequest{
		Number:     3,
		HeadRefOID: "head1",
		Reviews:    []Review{{Author: Author{Login: "codex"}, State: "DISMISSED", Commit: Commit{OID: "head1"}}},
	})
	if dismissed.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(dismissed-only) = %+v, want unreviewed", dismissed)
	}

	// A submitted review without its commit provenance cannot be bound to the
	// merged head and must fail closed.
	missingCommit := Classify(PullRequest{
		Number:     4,
		Author:     Author{Login: "maintainer"},
		HeadRefOID: "head1",
		Reviews:    []Review{{Author: Author{Login: "codex"}, State: "APPROVED"}},
	})
	if missingCommit.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(missing review commit) = %+v, want unreviewed", missingCommit)
	}

	// The same is true if the input lacks the merged head provenance.
	missingHead := Classify(PullRequest{
		Number:  5,
		Author:  Author{Login: "maintainer"},
		Reviews: []Review{{Author: Author{Login: "codex"}, State: "APPROVED", Commit: Commit{OID: "head1"}}},
	})
	if missingHead.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(missing merged head) = %+v, want unreviewed", missingHead)
	}

	// Head binding: a review of an earlier commit does not cover the head
	// that merged.
	stale := Classify(PullRequest{
		Number:     6,
		Author:     Author{Login: "maintainer"},
		HeadRefOID: "head2",
		Reviews:    []Review{{Author: Author{Login: "codex"}, State: "APPROVED", Commit: Commit{OID: "head1"}}},
	})
	if stale.Verdict != VerdictStaleReviewed {
		t.Fatalf("Classify(stale) = %+v, want stale-reviewed", stale)
	}
	current := Classify(PullRequest{
		Number:     7,
		Author:     Author{Login: "maintainer"},
		HeadRefOID: "head2",
		Reviews: []Review{
			{Author: Author{Login: "codex"}, State: "APPROVED", Commit: Commit{OID: "head1"}},
			{Author: Author{Login: "codex"}, State: "COMMENTED", Commit: Commit{OID: "head2"}},
		},
	})
	if current.Verdict != VerdictReviewed {
		t.Fatalf("Classify(current head reviewed) = %+v, want reviewed", current)
	}

	refused := Classify(PullRequest{
		Number:   8,
		Comments: []Comment{{Author: Author{Login: "coderabbitai"}, Body: "> ## Review limit reached\n> wait 30 minutes"}},
	})
	if refused.Verdict != VerdictRefused {
		t.Fatalf("Classify(refused) = %+v, want rate-limit-refused: a refusal notice is not participation", refused)
	}

	// A participant quoting the phrase is not the reviewer refusing.
	quoted := Classify(PullRequest{
		Number:   9,
		Comments: []Comment{{Author: Author{Login: "somedev"}, Body: "we keep hitting 'Review limit reached' lately"}},
	})
	if quoted.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(quoted refusal) = %+v, want unreviewed: untrusted author", quoted)
	}

	// A refusal notice does not downgrade a PR that also got a real review.
	both := Classify(PullRequest{
		Number:     10,
		Author:     Author{Login: "maintainer"},
		HeadRefOID: "head1",
		Reviews:    []Review{{Author: Author{Login: "codex"}, State: "APPROVED", Commit: Commit{OID: "head1"}}},
		Comments:   []Comment{{Author: Author{Login: "coderabbitai"}, Body: "Review limit reached"}},
	})
	if both.Verdict != VerdictReviewed {
		t.Fatalf("Classify(review+refusal) = %+v, want reviewed", both)
	}

	// A review submitted by the PR author does not establish independent
	// coverage, even when it is bound to the merged head.
	selfReviewed := Classify(PullRequest{
		Number:     11,
		Author:     Author{Login: "maintainer"},
		HeadRefOID: "head1",
		Reviews:    []Review{{Author: Author{Login: "Maintainer"}, State: "COMMENTED", Commit: Commit{OID: "head1"}}},
	})
	if selfReviewed.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(self-reviewed) = %+v, want unreviewed", selfReviewed)
	}

	// Missing PR-author provenance cannot prove that a submitted review was
	// independent, so coverage also fails closed.
	missingAuthor := Classify(PullRequest{
		Number:     12,
		HeadRefOID: "head1",
		Reviews:    []Review{{Author: Author{Login: "codex"}, State: "APPROVED", Commit: Commit{OID: "head1"}}},
	})
	if missingAuthor.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(missing PR author) = %+v, want unreviewed", missingAuthor)
	}

	bare := Classify(PullRequest{Number: 13, Comments: []Comment{{Author: Author{Login: "somedev"}, Body: "ordinary discussion"}}})
	if bare.Verdict != VerdictUnreviewed {
		t.Fatalf("Classify(bare) = %+v, want unreviewed", bare)
	}
}

func TestAuditReportAndThreshold(t *testing.T) {
	input := []byte(`[
		{"number": 10, "title": "reviewed change", "author": {"login": "maintainer"}, "additions": 500, "deletions": 100,
		 "headRefOid": "h1", "reviews": [{"author": {"login": "codex"}, "state": "APPROVED", "commit": {"oid": "h1"}}]},
		{"number": 11, "title": "big silent change", "additions": 900, "deletions": 141,
		 "comments": [{"author": {"login": "coderabbitai"}, "body": "Review limit reached"}]},
		{"number": 12, "title": "small silent change", "additions": 3, "deletions": 1},
		{"number": 13, "title": "big stale-reviewed rewrite", "author": {"login": "maintainer"}, "additions": 800, "deletions": 10,
		 "headRefOid": "h2",
		 "reviews": [{"author": {"login": "codex"}, "state": "APPROVED", "commit": {"oid": "h1"}}]},
		{"number": 14, "title": "big review without provenance", "additions": 700, "deletions": 1,
		 "headRefOid": "h1", "reviews": [{"author": {"login": "codex"}, "state": "APPROVED"}]},
		{"number": 15, "title": "big dismissed review", "additions": 650, "deletions": 1,
		 "headRefOid": "h1", "reviews": [{"author": {"login": "codex"}, "state": "DISMISSED", "commit": {"oid": "h1"}}]}
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
		"#13\tstale-reviewed\t+800/-10\t[codex]\tLARGE-UNREVIEWED\tbig stale-reviewed rewrite",
		"#14\tunreviewed\t+700/-1\tLARGE-UNREVIEWED\tbig review without provenance",
		"#15\tunreviewed\t+650/-1\tLARGE-UNREVIEWED\tbig dismissed review",
		"total=6 reviewed=1 stale-reviewed=1 rate-limit-refused=1 unreviewed=3",
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
	if _, _, err := Audit(input, -1); err == nil {
		t.Fatal("Audit(negative threshold) must error instead of silently disabling the gate")
	}
}

func TestAuditLargeGateRejectsSelfReviewAndMissingChangedLineCounts(t *testing.T) {
	selfReview := []byte(`[
		{"number": 16, "title": "large author-only review", "author": {"login": "maintainer"}, "additions": 1000, "deletions": 1,
		 "headRefOid": "h1", "reviews": [{"author": {"login": "maintainer"}, "state": "COMMENTED", "commit": {"oid": "h1"}}]}
	]`)
	report, flagged, err := Audit(selfReview, 300)
	if err != nil {
		t.Fatalf("Audit(self review) error = %v", err)
	}
	if !flagged || !strings.Contains(report, "#16\tunreviewed\t+1000/-1\tLARGE-UNREVIEWED") {
		t.Fatalf("Audit(self review) = flagged %t report:\n%s\nwant large self-review to fail", flagged, report)
	}

	for _, test := range []struct {
		input []byte
		want  string
	}{
		{[]byte(`[{"number": 17, "title": "missing additions", "deletions": 1000}]`), "missing additions"},
		{[]byte(`[{"number": 18, "title": "missing deletions", "additions": 1000}]`), "missing deletions"},
		{[]byte(`[{"number": 19, "title": "missing both"}]`), "missing additions and deletions"},
	} {
		if _, _, err := Audit(test.input, 300); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("Audit(missing changed-line count) error = %v, want fail-closed validation", err)
		}
	}

	// Explicit zero is valid data, not an omitted count.
	if _, _, err := Audit([]byte(`[{"number": 20, "title": "zero-sized", "additions": 0, "deletions": 0}]`), 300); err != nil {
		t.Fatalf("Audit(explicit zero counts) error = %v, want accepted", err)
	}
}
