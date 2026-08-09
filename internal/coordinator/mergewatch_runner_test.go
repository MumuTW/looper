package coordinator

import (
	"context"
	"encoding/json"
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

// TestLinkedPullRequestNumbersReadsProjectedCrossReference covers the
// gateway-to-coordinator lifecycle contract for issue #543's projection fix.
// ListIssueTimeline projects cross-referenced events down to the minimal
// identifying fields (number, html_url, url, and the nested pull_request
// marker) and stores those rows in rawTimeline. linkedPullRequestNumbers is
// what resolveWatchedPR and applyMarkReady use to discover the PR a
// cross-reference points at, so the projected shape must still yield the PR
// number — otherwise merge-watch and mark-ready are silently disabled for
// every issue whose linked PR is only reachable through a cross-referenced
// event. A cross-reference to a plain issue (no pull_request marker) must not
// be counted as a PR.
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
	snapshot := mergewatch.PRSnapshot{Repo: "acme/looper", PRNumber: 42, HeadSHA: "head-42", MergeCommitSHA: "merge-42", MergeStrategy: "merge", SourceIssueRepo: "acme/looper", MergedAt: forgeMergedAt, Merged: true}
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
	if !containsAll(events[0].PayloadJSON, `"mergedAt":"`+forgeMergedAt+`"`, `"headSha":"head-42"`, `"mergeCommitSha":"merge-42"`, `"mergeStrategy":"merge"`, `"sourceIssue":{"number":7,"repo":"acme/looper"}`) {
		t.Fatalf("payload = %s, want complete forge merge provenance", events[0].PayloadJSON)
	}
}

func TestApplyMergeWatchRecordsMergifyMergeEvidence(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	seedCoordinatorGatekeeperRoute(t, fixture, 42, "head-42")
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, Body: "Closes #7", State: "closed", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge}, MergedAt: "2026-05-14T11:58:07.000Z", MergedBy: "mergify[bot]",
		Mergeable: boolPtr(true), MergeableState: "clean",
	}
	loaded := []loadedIssue{{
		detail:      githubinfra.IssueDetail{Number: 7, Labels: []string{"triaged"}},
		rawTimeline: []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 42}}}}},
	}}
	if _, err := fixture.runner.applyMergeWatch(context.Background(), fixture.projectID, "acme/looper", t.TempDir(), loaded, fixture.cfg.Roles, labels.DefaultNamespace()); err != nil {
		t.Fatalf("applyMergeWatch() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	coordinatorMerges := 0
	for _, event := range events {
		if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
			coordinatorMerges++
		}
	}
	if err != nil || coordinatorMerges != 1 {
		t.Fatalf("merge events = %#v, %v, want one Coordinator merge event", events, err)
	}
}

func TestApplyMergeWatchRejectsUnbackedMergifyLabel(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	// The PR carries a Looper-owned issue label and a manually-added Mergify
	// route, but there is no successful Gatekeeper route report for this head.
	// The label alone must not make the issue-lane watcher attribute a merge to
	// Looper.
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, Body: "Closes #7", State: "closed", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge}, MergedAt: "2026-05-14T11:58:07.000Z", MergedBy: "mergify[bot]",
	}
	loaded := []loadedIssue{{
		detail:      githubinfra.IssueDetail{Number: 7, Labels: []string{"triaged"}},
		rawTimeline: []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 42}}}}},
	}}
	if _, err := fixture.runner.applyMergeWatch(context.Background(), fixture.projectID, "acme/looper", t.TempDir(), loaded, fixture.cfg.Roles, labels.DefaultNamespace()); err != nil {
		t.Fatalf("applyMergeWatch() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
			t.Fatalf("unbacked Mergify label recorded as Looper merge evidence: %#v", event)
		}
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
		Number: 42, State: "closed", HeadSHA: "head-42", BaseRefName: "main", MergedAt: "2026-05-14T11:58:07.000Z", MergedBy: "mergify[bot]",
	}
	fixture.github.failPRCheckRuns = map[string]error{"head-42": errors.New("HTTP 504 gateway timeout")}
	snapshot, tempErr, err := fixture.runner.mergeWatchSnapshot(context.Background(), "acme/looper", cwd, 7, 42, labels.DefaultNamespace(), "looper")
	if err != nil {
		t.Fatalf("mergeWatchSnapshot() error = %v", err)
	}
	if tempErr != nil {
		t.Fatalf("merged snapshot returned TemporaryError = %v, want authoritative merge", tempErr)
	}
	if !snapshot.Merged || snapshot.MergedAt != "2026-05-14T11:58:07.000Z" || snapshot.HeadSHA != "head-42" {
		t.Fatalf("snapshot = %#v, want authoritative merged state", snapshot)
	}
	if snapshot.MergeStrategy != "" {
		t.Fatalf("snapshot merge strategy = %q, want unknown when the forge does not report it", snapshot.MergeStrategy)
	}
	if reads := fixture.github.prCheckRunReads["head-42"]; reads != 0 {
		t.Fatalf("check-runs reads = %d, want 0 (short-circuited before the read)", reads)
	}
}

func TestApplyMergeWatchMergedSnapshotUsesProjectNamespace(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{"team.looper:plan"}, MergedAt: "2026-05-14T11:58:07.000Z", MergedBy: "mergify[bot]",
	}

	snapshot, tempErr, err := fixture.runner.mergeWatchSnapshot(context.Background(), "acme/looper", t.TempDir(), 7, 42, labels.NewNamespace("team.looper:"), "looper")
	if err != nil {
		t.Fatalf("mergeWatchSnapshot() error = %v", err)
	}
	if tempErr != nil {
		t.Fatalf("mergeWatchSnapshot() TemporaryError = %v, want authoritative merge", tempErr)
	}
	if !snapshot.Merged || !snapshot.HasLooperLabel {
		t.Fatalf("snapshot = %#v, want merged snapshot with custom-namespace Looper label", snapshot)
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
	if _, err := fixture.runner.applyMergeWatch(context.Background(), fixture.projectID, "acme/looper", t.TempDir(), loaded, fixture.cfg.Roles, labels.DefaultNamespace()); err != nil {
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
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge}, MergedBy: "mergify[bot]",
	}
	if err := fixture.runner.applyRoutedMergeWatch(context.Background(), fixture.projectID, "acme/looper", cwd); err != nil {
		t.Fatalf("applyRoutedMergeWatch() settle error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	var mergedEvent *eventlog.CoordinatorPullRequestMerged
	for _, event := range events {
		if event.EventType == eventlog.CoordinatorPullRequestMergedEventType {
			var payload eventlog.CoordinatorPullRequestMerged
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
				t.Fatalf("decode Coordinator merge event: %v", err)
			}
			mergedEvent = &payload
		}
	}
	if mergedEvent == nil {
		t.Fatalf("routed merge outside issue discovery recorded no Coordinator merge event: %#v", events)
	}
	if mergedEvent.MergeStrategy != "" {
		t.Fatalf("routed merge strategy = %q, want unknown when the forge omits it", mergedEvent.MergeStrategy)
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

func TestApplyRoutedMergeWatchBoundsOutOfPagePollingAndServicesLaterRegistrations(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	ctx := context.Background()
	fixture.runner.now = func() time.Time { return fixture.now }
	open := make([]githubinfra.PullRequestSummary, 0, 100)
	for prNumber := int64(1); prNumber <= 100; prNumber++ {
		open = append(open, githubinfra.PullRequestSummary{Number: prNumber, State: "OPEN"})
	}
	fixture.github.openPullRequestSummaries = open
	for prNumber := int64(101); prNumber <= 130; prNumber++ {
		head := fmt.Sprintf("head-%d", prNumber)
		seedCoordinatorGatekeeperRoute(t, fixture, prNumber, head)
		fixture.github.prDetails[prNumber] = githubinfra.PullRequestDetail{
			Number: prNumber, State: "OPEN", HeadSHA: head, BaseRefName: "main", Labels: []string{labels.AutoMerge},
		}
		if err := fixture.runner.upsertRoutedMergeWatch(ctx, fixture.projectID, "acme/looper", prNumber, head, false); err != nil {
			t.Fatalf("seed routed merge watch %d: %v", prNumber, err)
		}
	}

	if err := fixture.runner.applyRoutedMergeWatch(ctx, fixture.projectID, "acme/looper", t.TempDir()); err != nil {
		t.Fatalf("first applyRoutedMergeWatch() error = %v", err)
	}
	if fixture.github.mergeWatchReads != maxRoutedMergeWatchChecksPerTick {
		t.Fatalf("first merge-watch reads = %d, want cap %d", fixture.github.mergeWatchReads, maxRoutedMergeWatchChecksPerTick)
	}

	if err := fixture.runner.applyRoutedMergeWatch(ctx, fixture.projectID, "acme/looper", t.TempDir()); err != nil {
		t.Fatalf("immediate second applyRoutedMergeWatch() error = %v", err)
	}
	if fixture.github.mergeWatchReads != maxRoutedMergeWatchChecksPerTick+10 {
		t.Fatalf("immediate merge-watch reads = %d, want the 10 never-serviced registrations to receive their first check", fixture.github.mergeWatchReads)
	}

	if err := fixture.runner.applyRoutedMergeWatch(ctx, fixture.projectID, "acme/looper", t.TempDir()); err != nil {
		t.Fatalf("immediate third applyRoutedMergeWatch() error = %v", err)
	}
	if fixture.github.mergeWatchReads != maxRoutedMergeWatchChecksPerTick+10 {
		t.Fatalf("third merge-watch reads = %d, want cadence to suppress repeats", fixture.github.mergeWatchReads)
	}

	fixture.now = fixture.now.Add(routedMergeWatchCheckInterval)
	if err := fixture.runner.applyRoutedMergeWatch(ctx, fixture.projectID, "acme/looper", t.TempDir()); err != nil {
		t.Fatalf("cadence applyRoutedMergeWatch() error = %v", err)
	}
	if fixture.github.mergeWatchReads != maxRoutedMergeWatchChecksPerTick*2+10 {
		t.Fatalf("later merge-watch reads = %d, want one bounded batch after the cadence interval", fixture.github.mergeWatchReads)
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
