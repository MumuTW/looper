package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

// noRequiredReviews is the shape that made the defect invisible: a repository
// whose base branch requires no review, where GitHub answers "" for
// reviewDecision. Every case below runs against it, because that is the
// configuration under which "nobody reviewed" and "review not required"
// previously produced the same report.
func noRequiredReviews(fixture *gatekeeperFixture) {
	fixture.github.detail.ReviewDecision = ""
	fixture.github.protection.HasRequiredReviews = false
	fixture.github.protection.RequiredApprovingReviewCount = 0
}

func TestReviewProvenanceSeparatesReviewedRefusedAndAbsent(t *testing.T) {
	tests := []struct {
		name          string
		reviews       []githubinfra.ReviewSummary
		comments      []githubinfra.CommentInfo
		wantStatus    ReviewProvenanceStatus
		wantCompleted int
		wantReviewers []ReviewerObservation
		wantRefusals  []ReviewRefusal
	}{
		{
			name:          "nothing looked at it",
			wantStatus:    ReviewProvenanceAbsent,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{},
		},
		{
			name:          "a bot reviewer submitted a review",
			reviews:       []githubinfra.ReviewSummary{{ID: 1, State: "COMMENTED", Author: "coderabbitai[bot]", IsBot: true}},
			wantStatus:    ReviewProvenanceReviewed,
			wantCompleted: 1,
			wantReviewers: []ReviewerObservation{{Login: "coderabbitai[bot]", IsBot: true, State: "COMMENTED"}},
			wantRefusals:  []ReviewRefusal{},
		},
		{
			name:          "a human approved",
			reviews:       []githubinfra.ReviewSummary{{ID: 2, State: "APPROVED", Author: "octocat"}},
			wantStatus:    ReviewProvenanceReviewed,
			wantCompleted: 1,
			wantReviewers: []ReviewerObservation{{Login: "octocat", State: "APPROVED"}},
			wantRefusals:  []ReviewRefusal{},
		},
		{
			name:          "a rate-limit notice is a refusal, not participation",
			comments:      []githubinfra.CommentInfo{{ID: 77, Author: "coderabbitai[bot]", IsBot: true, Body: "> [!NOTE]\n> Review limit reached. Additional reviews become available gradually."}},
			wantStatus:    ReviewProvenanceRefused,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{{Login: "coderabbitai[bot]", Detector: "coderabbit_review_limit", CommentID: 77}},
		},
		{
			name:          "a refusal alongside a completed review still reads as reviewed",
			reviews:       []githubinfra.ReviewSummary{{ID: 3, State: "CHANGES_REQUESTED", Author: "octocat"}},
			comments:      []githubinfra.CommentInfo{{ID: 78, Author: "coderabbitai[bot]", IsBot: true, Body: "Review limit reached"}},
			wantStatus:    ReviewProvenanceReviewed,
			wantCompleted: 1,
			wantReviewers: []ReviewerObservation{{Login: "octocat", State: "CHANGES_REQUESTED"}},
			wantRefusals:  []ReviewRefusal{{Login: "coderabbitai[bot]", Detector: "coderabbit_review_limit", CommentID: 78}},
		},
		{
			// The detector's authority is the account, not the body. A person
			// pasting the notice — to complain about it, or to ask what it means —
			// must not be recorded as having declined to review the change.
			name:          "a human quoting the notice has not refused to review",
			comments:      []githubinfra.CommentInfo{{ID: 79, Author: "octocat", Body: "CodeRabbit says `Review limit reached` again — can we raise the quota?"}},
			wantStatus:    ReviewProvenanceAbsent,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{},
		},
		{
			// The same account, spelled the way gh's GraphQL projections spell it.
			// Identity is the account, so the `[bot]` suffix REST adds must not be
			// what decides whether this matches.
			name:          "the account without the REST bot suffix is the same account",
			comments:      []githubinfra.CommentInfo{{ID: 80, Author: "coderabbitai", IsBot: true, Body: "Review limit reached"}},
			wantStatus:    ReviewProvenanceRefused,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{{Login: "coderabbitai", Detector: "coderabbit_review_limit", CommentID: 80}},
		},
		{
			// A login is a string a person can also hold, especially on an
			// enterprise install. GitHub's own account classification is what
			// separates the reviewer from someone wearing its name.
			name:          "an account the forge calls human is not the reviewer",
			comments:      []githubinfra.CommentInfo{{ID: 81, Author: "coderabbitai", Body: "Review limit reached"}},
			wantStatus:    ReviewProvenanceAbsent,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{},
		},
		{
			name:          "a review the reviewer never submitted is not a review",
			reviews:       []githubinfra.ReviewSummary{{ID: 4, State: "PENDING", Author: "octocat"}},
			wantStatus:    ReviewProvenanceAbsent,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{},
		},
		{
			name:          "a dismissed review is not scrutiny anything still carries",
			reviews:       []githubinfra.ReviewSummary{{ID: 5, State: "DISMISSED", Author: "octocat"}},
			wantStatus:    ReviewProvenanceAbsent,
			wantReviewers: []ReviewerObservation{},
			wantRefusals:  []ReviewRefusal{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withRefusalsFrom(reviewsObservedFrom(tt.reviews), tt.comments)
			if got.Status != tt.wantStatus || got.CompletedReviews != tt.wantCompleted {
				t.Fatalf("provenance = %#v, want status %q with %d completed reviews", got, tt.wantStatus, tt.wantCompleted)
			}
			if !slices.Equal(got.Reviewers, tt.wantReviewers) {
				t.Fatalf("reviewers = %#v, want %#v", got.Reviewers, tt.wantReviewers)
			}
			if !slices.Equal(got.Refusals, tt.wantRefusals) {
				t.Fatalf("refusals = %#v, want %#v", got.Refusals, tt.wantRefusals)
			}
		})
	}
}

// TestGateReportSeparatesUnreviewedFromReviewNotRequired is the defect itself:
// on a repository without required-review protection, a pull request nobody
// reviewed and one that was reviewed used to persist the same evidence.
func TestGateReportSeparatesUnreviewedFromReviewNotRequired(t *testing.T) {
	unreviewed := newGatekeeperFixture(t)
	noRequiredReviews(unreviewed)
	absent, err := unreviewed.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}

	reviewed := newGatekeeperFixture(t)
	noRequiredReviews(reviewed)
	reviewed.github.reviews = []githubinfra.ReviewSummary{{ID: 1, State: "APPROVED", Author: "octocat"}}
	present, err := reviewed.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}

	if absent.Evidence.ReviewDecision != present.Evidence.ReviewDecision {
		t.Fatalf("reviewDecision differs (%q vs %q); the premise of this test is that it does not",
			absent.Evidence.ReviewDecision, present.Evidence.ReviewDecision)
	}
	if absent.Evidence.ReviewProvenance.Status != ReviewProvenanceAbsent {
		t.Fatalf("unreviewed provenance = %#v, want absent", absent.Evidence.ReviewProvenance)
	}
	if present.Evidence.ReviewProvenance.Status != ReviewProvenanceReviewed {
		t.Fatalf("reviewed provenance = %#v, want reviewed", present.Evidence.ReviewProvenance)
	}
}

// TestReviewProvenanceLeavesEligibilityUnchanged holds Gatekeeper to being
// observe-only: provenance changes what the report says, never what it permits.
func TestReviewProvenanceLeavesEligibilityUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*gatekeeperFixture)
		wantStat ReviewProvenanceStatus
	}{
		{name: "absent", mutate: func(*gatekeeperFixture) {}, wantStat: ReviewProvenanceAbsent},
		{
			name: "refused",
			mutate: func(f *gatekeeperFixture) {
				f.github.comments = []githubinfra.CommentInfo{{ID: 9, Author: "coderabbitai[bot]", IsBot: true, Body: "Review limit reached"}}
			},
			wantStat: ReviewProvenanceRefused,
		},
		{
			name: "reviewed",
			mutate: func(f *gatekeeperFixture) {
				f.github.reviews = []githubinfra.ReviewSummary{{ID: 1, State: "APPROVED", Author: "octocat"}}
			},
			wantStat: ReviewProvenanceReviewed,
		},
		{
			name:     "unknown because the forge would not answer",
			mutate:   func(f *gatekeeperFixture) { f.github.reviewsErr = errors.New("gh: secondary rate limit") },
			wantStat: ReviewProvenanceUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGatekeeperFixture(t)
			noRequiredReviews(fixture)
			tt.mutate(fixture)
			report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
				ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
			})
			if err != nil {
				t.Fatalf("EvaluatePullRequest() error = %v", err)
			}
			if report.Evidence.ReviewProvenance.Status != tt.wantStat {
				t.Fatalf("provenance status = %q, want %q", report.Evidence.ReviewProvenance.Status, tt.wantStat)
			}
			if !report.Eligible || report.Status != StatusEligible || len(report.Reasons) != 0 {
				t.Fatalf("report = %#v, want eligible with no reasons regardless of review provenance", report)
			}
		})
	}
}

// TestMergedPullRequestReportCarriesReviewProvenance covers the report an
// operator actually reads after the fact. A merged pull request answers the
// mergeability read ambiguously, which returns before the review gate, so a
// provenance recorded any later would be missing from exactly the reports that
// matter.
func TestMergedPullRequestReportCarriesReviewProvenance(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	noRequiredReviews(fixture)
	fixture.github.detail.State = "MERGED"
	fixture.github.mergeable = githubinfra.PullRequestDetail{Number: 42, HeadSHA: "head-1"}
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 5, Author: "coderabbitai[bot]", IsBot: true, Body: "Review limit reached"}}

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Evidence.PullRequestState != "MERGED" {
		t.Fatalf("pull request state = %q, want MERGED", report.Evidence.PullRequestState)
	}
	if report.Evidence.ReviewProvenance.Status != ReviewProvenanceRefused {
		t.Fatalf("provenance = %#v, want refused on the merged report", report.Evidence.ReviewProvenance)
	}
}

func TestListUnreviewedEnumeratesMergedPullRequestsWithoutACompletedReview(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	ctx := context.Background()

	// The pull request an operator is looking for: merged, with a rate-limit
	// refusal standing in for a review.
	seedGateReportWithProvenance(t, fixture, "acme/looper", 280, "MERGED", ReviewProvenance{
		Status:   ReviewProvenanceRefused,
		Refusals: []ReviewRefusal{{Login: "coderabbitai[bot]", Detector: "coderabbit_review_limit", CommentID: 1}},
	}, fixture.now)
	// Merged and genuinely reviewed.
	seedGateReportWithProvenance(t, fixture, "acme/looper", 281, "MERGED", ReviewProvenance{
		Status: ReviewProvenanceReviewed, CompletedReviews: 1,
		Reviewers: []ReviewerObservation{{Login: "octocat", State: "APPROVED"}},
	}, fixture.now)
	// Merged, but the forge never answered. Not knowing is not the same as
	// knowing nobody reviewed, so it must not appear.
	seedGateReportWithProvenance(t, fixture, "acme/looper", 282, "MERGED", ReviewProvenance{Status: ReviewProvenanceUnknown}, fixture.now)
	// A report from a build that predates review provenance.
	seedGateReportWithProvenance(t, fixture, "acme/looper", 283, "MERGED", ReviewProvenance{Status: ReviewProvenanceUnobserved}, fixture.now)
	// Still open and unreviewed: real, but not a merged unreviewed change.
	seedGateReportWithProvenance(t, fixture, "acme/looper", 284, "OPEN", ReviewProvenance{Status: ReviewProvenanceAbsent}, fixture.now)
	// Two reports for one pull request: the later one is the current answer.
	seedGateReportWithProvenance(t, fixture, "acme/looper", 285, "MERGED", ReviewProvenance{Status: ReviewProvenanceAbsent}, fixture.now)
	seedGateReportWithProvenance(t, fixture, "acme/looper", 285, "MERGED", ReviewProvenance{
		Status: ReviewProvenanceReviewed, CompletedReviews: 1,
	}, fixture.now.Add(time.Minute))

	merged, err := ListUnreviewed(ctx, fixture.repos, "MERGED")
	if err != nil {
		t.Fatalf("ListUnreviewed() error = %v", err)
	}
	gotNumbers := make([]int64, 0, len(merged))
	for _, item := range merged {
		gotNumbers = append(gotNumbers, item.PRNumber)
	}
	if !slices.Equal(gotNumbers, []int64{280}) {
		t.Fatalf("merged unreviewed = %#v, want only pull request 280", gotNumbers)
	}
	if merged[0].ReviewProvenance.Status != ReviewProvenanceRefused || len(merged[0].ReviewProvenance.Refusals) != 1 {
		t.Fatalf("item = %#v, want the recorded refusal carried through", merged[0])
	}

	all, err := ListUnreviewed(ctx, fixture.repos, "")
	if err != nil {
		t.Fatalf("ListUnreviewed() error = %v", err)
	}
	allNumbers := make([]int64, 0, len(all))
	for _, item := range all {
		allNumbers = append(allNumbers, item.PRNumber)
	}
	if !slices.Equal(allNumbers, []int64{280, 284}) {
		t.Fatalf("unfiltered unreviewed = %#v, want 280 and 284", allNumbers)
	}
}

// TestCommentsFailureKeepsWhatTheReviewsAlreadyProved holds the two reads to
// what each can actually answer. Comments separate refused from absent, and
// only when nothing was reviewed; they can never unmake a submitted review. So
// a comments read that fails must not turn a named, approving reviewer into
// "Looper does not know whether anyone looked at this".
func TestCommentsFailureKeepsWhatTheReviewsAlreadyProved(t *testing.T) {
	tests := []struct {
		name          string
		reviews       []githubinfra.ReviewSummary
		wantStatus    ReviewProvenanceStatus
		wantReviewers []ReviewerObservation
	}{
		{
			name:          "a completed review survives the comments failure",
			reviews:       []githubinfra.ReviewSummary{{ID: 1, State: "APPROVED", Author: "octocat"}},
			wantStatus:    ReviewProvenanceReviewed,
			wantReviewers: []ReviewerObservation{{Login: "octocat", State: "APPROVED"}},
		},
		{
			// Nothing was reviewed, so refused and absent are exactly what the
			// comments would have separated — and they did not answer.
			name:          "without a review the comments failure really is not knowing",
			wantStatus:    ReviewProvenanceUnknown,
			wantReviewers: []ReviewerObservation{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGatekeeperFixture(t)
			noRequiredReviews(fixture)
			fixture.github.reviews = tt.reviews
			fixture.github.commentsErr = errors.New("gh: HTTP 502")

			report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
				ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
			})
			if err != nil {
				t.Fatalf("EvaluatePullRequest() error = %v", err)
			}
			provenance := report.Evidence.ReviewProvenance
			if provenance.Status != tt.wantStatus {
				t.Fatalf("provenance = %#v, want status %q", provenance, tt.wantStatus)
			}
			if !slices.Equal(provenance.Reviewers, tt.wantReviewers) {
				t.Fatalf("reviewers = %#v, want %#v", provenance.Reviewers, tt.wantReviewers)
			}
			if !report.Eligible {
				t.Fatalf("report = %#v, want eligibility unchanged by a failed observation", report)
			}
		})
	}
}

// TestListUnreviewedKeepsSameSlugProjectsApart covers the multi-provider case:
// `acme/looper#280` names a different pull request on github.com than it does
// on an enterprise host, and both are legitimate projects. Collapsing them onto
// the shared entity id drops one from the operator's answer entirely.
func TestListUnreviewedKeepsSameSlugProjectsApart(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	ctx := context.Background()
	nowISO := fixture.now.Format(time.RFC3339Nano)
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "project_2", Name: "Looper (enterprise)", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	seedGateReportFor(t, fixture, "project_1", "acme/looper", 280, "MERGED",
		ReviewProvenance{Status: ReviewProvenanceAbsent}, fixture.now)
	// Same slug, same number, different project — and written later, so a map
	// keyed on the entity id alone would keep only this one.
	seedGateReportFor(t, fixture, "project_2", "acme/looper", 280, "MERGED",
		ReviewProvenance{Status: ReviewProvenanceRefused}, fixture.now.Add(time.Minute))

	merged, err := ListUnreviewed(ctx, fixture.repos, "MERGED")
	if err != nil {
		t.Fatalf("ListUnreviewed() error = %v", err)
	}
	gotProjects := make([]string, 0, len(merged))
	for _, item := range merged {
		gotProjects = append(gotProjects, item.ProjectID)
	}
	if !slices.Equal(gotProjects, []string{"project_1", "project_2"}) {
		t.Fatalf("projects = %#v, want both physical repositories reported", gotProjects)
	}
}

func seedGateReportWithProvenance(t *testing.T, fixture *gatekeeperFixture, repo string, prNumber int64, state string, provenance ReviewProvenance, at time.Time) {
	t.Helper()
	seedGateReportFor(t, fixture, "project_1", repo, prNumber, state, provenance, at)
}

func seedGateReportFor(t *testing.T, fixture *gatekeeperFixture, project, repo string, prNumber int64, state string, provenance ReviewProvenance, at time.Time) {
	t.Helper()
	report := Report{
		Version: reportVersion, ProjectID: project, Repo: repo, PRNumber: prNumber,
		EvaluatedAt: at.UTC().Format(time.RFC3339Nano),
		Evidence:    Evidence{PullRequestState: state, ReviewProvenance: provenance},
	}
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", repo, prNumber)
	projectID := report.ProjectID
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: report, CreatedAt: at,
	}); err != nil {
		t.Fatalf("eventlog.Append() error = %v", err)
	}
}
