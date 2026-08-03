package coordinator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestLinkedPullRequestNumbersReadsProjectedCrossReference(t *testing.T) {
	t.Parallel()
	timeline := []map[string]any{
		// Projected cross-referenced event pointing at a linked PR: the source
		// issue carries the PR number, URL, and the pull_request marker that
		// distinguishes a PR from a plain issue.
		{
			"event":        "cross-referenced",
			"created_at":   "2026-05-16T10:00:00Z",
			"label":        nil,
			"source":       map[string]any{"issue": map[string]any{"number": int64(42), "html_url": "https://github.com/acme/looper/pull/42", "url": "https://api.github.com/repos/acme/looper/pulls/42", "pull_request": map[string]any{"number": int64(42), "html_url": "https://github.com/acme/looper/pull/42", "url": "https://api.github.com/repos/acme/looper/pulls/42"}}},
			"pull_request": nil,
			"issue":        nil,
		},
		// Projected cross-referenced event pointing at a plain issue: no
		// pull_request marker, and the html_url is an issue URL, so this must
		// not be reported as a linked PR.
		{
			"event":        "cross-referenced",
			"created_at":   "2026-05-17T10:00:00Z",
			"label":        nil,
			"source":       map[string]any{"issue": map[string]any{"number": int64(50), "html_url": "https://github.com/acme/looper/issues/50", "url": nil, "pull_request": nil}},
			"pull_request": nil,
			"issue":        nil,
		},
		// A referenced event carrying a top-level pull_request URL, the other
		// shape linkedPullRequestNumbers walks.
		{
			"event":        "referenced",
			"created_at":   "2026-05-18T10:00:00Z",
			"label":        nil,
			"source":       nil,
			"pull_request": map[string]any{"number": int64(7), "html_url": "https://github.com/acme/looper/pull/7", "url": "https://api.github.com/repos/acme/looper/pulls/7"},
			"issue":        nil,
		},
	}
	got := linkedPullRequestNumbers(timeline)
	want := []int64{42, 7}
	if len(got) != len(want) {
		t.Fatalf("linkedPullRequestNumbers() = %v, want %v (PR 42 from the cross-reference, PR 7 from the referenced event; the plain issue 50 must be excluded)", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("linkedPullRequestNumbers()[%d] = %d, want %d; full result = %v", i, got[i], n, got)
		}
	}
}

func TestRecordPostMergeEventPreservesForgeMergedAtAndIsIdempotent(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	forgeMergedAt := "2026-05-14T11:58:07.000Z"
	snapshot := mergewatch.PRSnapshot{Repo: "acme/looper", PRNumber: 42, HeadSHA: "head-42", MergedAt: forgeMergedAt, Merged: true}
	if err := fixture.runner.recordPostMergeEvent(context.Background(), fixture.projectID, snapshot.Repo, 7, snapshot); err != nil {
		t.Fatalf("recordPostMergeEvent() error = %v", err)
	}
	if err := fixture.runner.recordPostMergeEvent(context.Background(), fixture.projectID, snapshot.Repo, 7, snapshot); err != nil {
		t.Fatalf("second recordPostMergeEvent() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, err=%v, want one idempotent merge event", events, err)
	}
	if events[0].EventType != eventlog.CoordinatorPullRequestMergedEventType || events[0].CreatedAt != "2026-05-14T12:00:00.000Z" {
		t.Fatalf("event = %#v, want durable coordinator observation", events[0])
	}
	if !containsAll(events[0].PayloadJSON, `"mergedAt":"`+forgeMergedAt+`"`, `"headSha":"head-42"`) {
		t.Fatalf("payload = %s, want forge mergedAt and head", events[0].PayloadJSON)
	}
}

func TestApplyMergeWatchRecordsMergifyMergeEvidence(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, Body: "Closes #7", State: "closed", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge}, MergedAt: "2026-05-14T11:58:07.000Z",
		Mergeable: boolPtr(true), MergeableState: "clean",
	}
	loaded := []loadedIssue{{
		detail:      githubinfra.IssueDetail{Number: 7, Labels: []string{"triaged"}},
		rawTimeline: []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 42}}}}},
	}}
	if _, err := fixture.runner.applyMergeWatch(context.Background(), fixture.projectID, "acme/looper", t.TempDir(), loaded, fixture.cfg.Roles); err != nil {
		t.Fatalf("applyMergeWatch() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil || len(events) != 1 || events[0].EventType != eventlog.CoordinatorPullRequestMergedEventType {
		t.Fatalf("merge events = %#v, %v, want one Coordinator merge event", events, err)
	}
}

func TestApplyMergeWatchShortCircuitsMergedSnapshotBeforeCheckRuns(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	cwd := t.TempDir()
	// The PR detail already carries the authoritative merged state, and the
	// check-runs read would fail transiently. The merged snapshot must
	// short-circuit before the check/protection reads so a coincident outage
	// cannot erase Auditor merge evidence.
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", HeadSHA: "head-42", BaseRefName: "main", MergedAt: "2026-05-14T11:58:07.000Z",
	}
	fixture.github.failPRCheckRuns = map[string]error{"head-42": errors.New("HTTP 504 gateway timeout")}
	snapshot, tempErr, err := fixture.runner.mergeWatchSnapshot(context.Background(), "acme/looper", cwd, 7, 42, "looper")
	if err != nil {
		t.Fatalf("mergeWatchSnapshot() error = %v", err)
	}
	if tempErr != nil {
		t.Fatalf("merged snapshot returned TemporaryError = %v, want authoritative merge", tempErr)
	}
	if !snapshot.Merged || snapshot.MergedAt != "2026-05-14T11:58:07.000Z" || snapshot.HeadSHA != "head-42" {
		t.Fatalf("snapshot = %#v, want authoritative merged state", snapshot)
	}
	if reads := fixture.github.prCheckRunReads["head-42"]; reads != 0 {
		t.Fatalf("check-runs reads = %d, want 0 (short-circuited before the read)", reads)
	}
}

func TestApplyMergeWatchRetiresHumanOwnedNativeAutoMergeWithoutEvidence(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	fixture.github.currentLogin = "looper"
	// Native auto-merge is owned by a human while the Mergify label is also
	// present. The watch must be retired without recording Looper merge
	// evidence: a human-owned route that wins must never be attributed to
	// Looper by the Auditor.
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, Body: "Closes #7", State: "OPEN", HeadSHA: "head-42", BaseRefName: "main",
		Labels:    []string{labels.DefaultPlanTrigger, labels.AutoMerge},
		AutoMerge: &githubinfra.PullRequestAutoMerge{EnabledBy: "human-owner", MergeMethod: "merge"},
	}
	loaded := []loadedIssue{{
		detail:      githubinfra.IssueDetail{Number: 7, Labels: []string{"triaged"}},
		rawTimeline: []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 42}}}}},
	}}
	if _, err := fixture.runner.applyMergeWatch(context.Background(), fixture.projectID, "acme/looper", t.TempDir(), loaded, fixture.cfg.Roles); err != nil {
		t.Fatalf("applyMergeWatch() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
			t.Fatalf("human-owned native auto-merge recorded as Looper merge evidence: %#v", event)
		}
	}
}

func TestApplyRoutedMergeWatchRecordsMergeEvidenceOutsideIssueDiscovery(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	cwd := t.TempDir()
	seedCoordinatorGatekeeperRoute(t, fixture, 42, "head-42")
	// A routed PR (carrying the Mergify auto-merge label) with no tracked issue
	// and no Coordinator-tracked issue linkage. It merges and closes, and the
	// routed registry must record the merge as Auditor evidence even though
	// the open-issue lane can never see it.
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, State: "OPEN", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge},
	}
	if err := fixture.runner.applyRoutedMergeWatch(context.Background(), fixture.projectID, "acme/looper", cwd); err != nil {
		t.Fatalf("applyRoutedMergeWatch() register error = %v", err)
	}
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", HeadSHA: "head-42", BaseRefName: "main", MergedAt: "2026-05-14T11:58:07.000Z",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge},
	}
	if err := fixture.runner.applyRoutedMergeWatch(context.Background(), fixture.projectID, "acme/looper", cwd); err != nil {
		t.Fatalf("applyRoutedMergeWatch() settle error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
			found = true
		}
	}
	if !found {
		t.Fatalf("routed merge outside issue discovery recorded no Coordinator merge event: %#v", events)
	}
}

func TestApplyRoutedMergeWatchSettlesHumanOwnedMergeWithoutEvidence(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	cwd := t.TempDir()
	seedCoordinatorGatekeeperRoute(t, fixture, 42, "head-42")
	fixture.github.currentLogin = "looper"
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, State: "OPEN", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge},
	}
	if err := fixture.runner.applyRoutedMergeWatch(context.Background(), fixture.projectID, "acme/looper", cwd); err != nil {
		t.Fatalf("applyRoutedMergeWatch() register error = %v", err)
	}
	// The human-owned native auto-merge wins: the PR merges without Looper's
	// authority. The registry must settle without recording Looper evidence.
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", HeadSHA: "head-42", BaseRefName: "main", MergedAt: "2026-05-14T11:58:07.000Z",
		Labels:    []string{labels.DefaultPlanTrigger, labels.AutoMerge},
		AutoMerge: &githubinfra.PullRequestAutoMerge{EnabledBy: "human-owner", MergeMethod: "merge"},
	}
	if err := fixture.runner.applyRoutedMergeWatch(context.Background(), fixture.projectID, "acme/looper", cwd); err != nil {
		t.Fatalf("applyRoutedMergeWatch() settle error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
			t.Fatalf("human-owned routed merge recorded as Looper merge evidence: %#v", event)
		}
	}
}

func seedCoordinatorGatekeeperRoute(t *testing.T, fixture coordinatorFixture, prNumber int64, headSHA string) {
	t.Helper()
	projectID := fixture.projectID
	entityType := "pull_request"
	entityID := "acme/looper#" + fmt.Sprint(prNumber)
	routeEstablished := true
	report := gatekeeper.Report{
		Version: 2, Mode: "auto", Status: gatekeeper.StatusEligible, Eligible: true,
		ProjectID: projectID, Repo: "acme/looper", PRNumber: prNumber,
		ObservedHeadSHA: headSHA, RouteEstablished: &routeEstablished,
		Evidence: gatekeeper.Evidence{FinalObservedHeadSHA: headSHA},
	}
	if err := eventlog.Append(context.Background(), fixture.runner.repos, eventlog.AppendInput{
		EventType: gatekeeper.GateReportEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, Payload: report, CreatedAt: fixture.now.Add(-time.Second),
	}); err != nil {
		t.Fatalf("seed Gatekeeper route: %v", err)
	}
}
