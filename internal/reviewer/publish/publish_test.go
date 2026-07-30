package publish

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/reviewer/criteria"
)

// The markers are dedup keys scanned for verbatim in existing PR comments;
// their exact shapes are a published contract.
func TestMarkerShapesAreStable(t *testing.T) {
	t.Parallel()

	if CriteriaFailMarker != "<!-- looper:reviewer:criteria-fail -->" {
		t.Fatalf("CriteriaFailMarker = %q", CriteriaFailMarker)
	}
	if AutoMergeRefusedMarker != "<!-- looper:reviewer:automerge-refused -->" {
		t.Fatalf("AutoMergeRefusedMarker = %q", AutoMergeRefusedMarker)
	}
	if CriteriaVerificationHeading != "### Acceptance criteria verification" {
		t.Fatalf("CriteriaVerificationHeading = %q", CriteriaVerificationHeading)
	}
}

func TestAuthorMention(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"alice", "@alice"},
		{"@alice", "@alice"},
		// Historical quirk pinned as-is: the prefix strip runs before the
		// space trim, so a padded @login keeps its @ and gains another.
		// Callers pass API-clean logins, so this never fires in practice;
		// changing it belongs to a behavior PR, not this extraction.
		{"  @alice  ", "@@alice"},
		{"", ""},
		{"  ", ""},
		{"@", ""},
	}
	for _, tc := range cases {
		if got := AuthorMention(tc.in); got != tc.want {
			t.Fatalf("AuthorMention(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanApprovalBody(t *testing.T) {
	t.Parallel()

	results := []criteria.CriterionResult{
		{Criterion: "compiles", Verdict: criteria.VerdictPass, Justification: "go build passes"},
		{Criterion: "flaky", Verdict: criteria.VerdictFail},
	}
	got := CleanApprovalBody("alice", CriteriaVerificationHeading, results, "I verified each stated acceptance criterion.")

	if !strings.HasPrefix(got, "@alice Thanks for the update") {
		t.Fatalf("body must open with the author mention:\n%s", got)
	}
	if !strings.Contains(got, "I verified each stated acceptance criterion.") {
		t.Fatalf("body missing intro:\n%s", got)
	}
	if !strings.Contains(got, CriteriaVerificationHeading) {
		t.Fatalf("body missing heading:\n%s", got)
	}
	if !strings.Contains(got, "- **compiles** — PASS: go build passes") {
		t.Fatalf("body missing passing criterion:\n%s", got)
	}
	if strings.Contains(got, "flaky") {
		t.Fatalf("approval body must list only passing criteria:\n%s", got)
	}
	if !strings.HasSuffix(got, "Happy to see this tightened up — nice work.") {
		t.Fatalf("body missing warm closer:\n%s", got)
	}

	// No heading: no criteria section at all, intro omitted when blank.
	bare := CleanApprovalBody("bob", "", nil, "  ")
	if strings.Contains(bare, CriteriaVerificationHeading) || strings.Contains(bare, "criteria") {
		t.Fatalf("headingless body must omit the criteria section:\n%s", bare)
	}
}

func TestCriteriaFailureBody(t *testing.T) {
	t.Parallel()

	results := []criteria.CriterionResult{
		{Criterion: "compiles", Verdict: criteria.VerdictFail, Justification: "build broke"},
	}
	got := CriteriaFailureBody(results)

	if !strings.HasPrefix(got, "Acceptance criteria could not be fully verified") {
		t.Fatalf("failure body opener wrong:\n%s", got)
	}
	if !strings.Contains(got, "- **compiles** — FAIL: build broke") {
		t.Fatalf("failure body missing failing criterion:\n%s", got)
	}
	if !strings.HasSuffix(got, CriteriaFailMarker) {
		t.Fatalf("failure body must end with the dedup marker:\n%s", got)
	}
}

func TestCriteriaResults(t *testing.T) {
	t.Parallel()

	evidence := []criteria.Evidence{
		{FilePath: "a.go", StartLine: 3},
		{FilePath: "b.go", StartLine: 5, EndLine: 9},
		{FilePath: "", StartLine: 2},
		{FilePath: "c.go", StartLine: 0},
	}
	results := []criteria.CriterionResult{
		{Criterion: "has tests", Verdict: criteria.VerdictPass, Evidence: evidence},
		{Criterion: "documented", Verdict: criteria.VerdictFail, Justification: "no docs"},
	}

	all := CriteriaResults(results, false)
	if !strings.Contains(all, "- **has tests** — PASS (a.go:3, b.go:5-9)") {
		t.Fatalf("evidence pointers wrong (want valid entries only, range formatting):\n%s", all)
	}
	if !strings.Contains(all, "- **documented** — FAIL: no docs") {
		t.Fatalf("failing line wrong:\n%s", all)
	}

	onlyPass := CriteriaResults(results, true)
	if strings.Contains(onlyPass, "documented") {
		t.Fatalf("includeOnlyPass must drop failing criteria:\n%s", onlyPass)
	}

	// Empty and filtered-to-empty fallbacks.
	if got := CriteriaResults(nil, true); got != "- No explicit acceptance criteria were available to verify." {
		t.Fatalf("CriteriaResults(nil, true) = %q", got)
	}
	if got := CriteriaResults(nil, false); got != "- No acceptance criteria results were recorded." {
		t.Fatalf("CriteriaResults(nil, false) = %q", got)
	}
	onlyFails := []criteria.CriterionResult{{Criterion: "x", Verdict: criteria.VerdictFail}}
	if got := CriteriaResults(onlyFails, true); got != "- No passing acceptance criteria were recorded." {
		t.Fatalf("CriteriaResults(onlyFails, true) = %q", got)
	}
}
