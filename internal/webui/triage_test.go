package webui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

var testNow = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// testLinker mirrors the runtime linker's shape without importing it, which
// would make this package's tests depend on the daemon runtime. Classify only
// needs the links to be consistent between sources.
type testLinker struct{}

func (testLinker) Issue(_ string, repo string, number int64) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, number)
}

func (testLinker) PullRequest(_ string, repo string, number int64) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}

func (testLinker) Loop(_ string, seq int64) string {
	return fmt.Sprintf("http://127.0.0.1:8787/dashboard/loops/%d", seq)
}

func ptr[T any](value T) *T { return &value }

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

type payloadOptions struct {
	draft     bool
	conflicts bool
	state     string
	additions int
	deletions int
}

func payload(t *testing.T, options payloadOptions) *string {
	t.Helper()
	state := options.state
	if state == "" {
		state = "OPEN"
	}
	detail := map[string]any{"State": state, "IsDraft": options.draft, "HasConflicts": options.conflicts}
	diff := ""
	if options.additions > 0 || options.deletions > 0 {
		var builder strings.Builder
		builder.WriteString("--- a/file.go\n+++ b/file.go\n")
		for index := 0; index < options.additions; index++ {
			builder.WriteString("+added\n")
		}
		for index := 0; index < options.deletions; index++ {
			builder.WriteString("-removed\n")
		}
		diff = builder.String()
	}
	raw, err := json.Marshal(map[string]any{"detail": detail, "diff": diff})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return ptr(string(raw))
}

func snapshot(t *testing.T, number int64, options payloadOptions) storage.PullRequestSnapshotRecord {
	t.Helper()
	return storage.PullRequestSnapshotRecord{
		ID:          fmt.Sprintf("snapshot-%d", number),
		ProjectID:   "proj",
		Repo:        "acme/widgets",
		PRNumber:    number,
		HeadSHA:     "abc123",
		Title:       ptr(fmt.Sprintf("Change %d", number)),
		PayloadJSON: payload(t, options),
		CapturedAt:  iso(testNow.Add(-2 * time.Hour)),
		CreatedAt:   iso(testNow.Add(-2 * time.Hour)),
	}
}

func report(number int64, eligible bool, codes ...gatekeeper.ReasonCode) gatekeeper.Report {
	reasons := make([]gatekeeper.Reason, 0, len(codes))
	for _, code := range codes {
		reasons = append(reasons, gatekeeper.Reason{Code: code})
	}
	status := gatekeeper.StatusBlocked
	if eligible {
		status = gatekeeper.StatusEligible
	}
	return gatekeeper.Report{
		Version: 2, Status: status, Eligible: eligible,
		ProjectID: "proj", Repo: "acme/widgets", PRNumber: number,
		Reasons: reasons, EvaluatedAt: iso(testNow.Add(-90 * time.Minute)),
	}
}

func activeLoop(number int64) storage.LoopRecord {
	return storage.LoopRecord{
		ID: fmt.Sprintf("loop-%d", number), Seq: number, ProjectID: "proj",
		Type: string(domain.LoopTypeFixer), TargetType: string(domain.LoopTargetTypePullRequest),
		Repo: ptr("acme/widgets"), PRNumber: ptr(number), Status: string(domain.LoopStatusRunning),
		CreatedAt: iso(testNow.Add(-3 * time.Hour)), UpdatedAt: iso(testNow.Add(-30 * time.Minute)),
	}
}

// rowFor finds the single row for a pull request number across every group.
func rowFor(t *testing.T, board Board, number int64) (Row, Group) {
	t.Helper()
	key := fmt.Sprintf("pr-proj-acme-widgets-%d", number)
	for _, section := range board.Sections {
		for _, row := range section.Rows {
			if row.Key == key {
				return row, section.Group
			}
		}
	}
	t.Fatalf("no row for pull request %d in %#v", number, board.Sections)
	return Row{}, GroupActionable
}

func TestClassifyPrimaryBlockerAndGroup(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		options     payloadOptions
		report      *gatekeeper.Report
		loops       []storage.LoopRecord
		items       []escalator.Item
		threads     *int64
		reviewState *string
		checks      *string
		wantCode    string
		wantLabel   string
		wantGroup   Group
		wantTone    Tone
	}{
		{
			name:      "merge conflict outranks everything else on the report",
			report:    ptr(report(1, false, gatekeeper.ReasonCheckFailed, gatekeeper.ReasonMergeConflict, gatekeeper.ReasonUnresolvedReviewThread)),
			wantCode:  string(gatekeeper.ReasonMergeConflict),
			wantLabel: "merge conflict",
			wantGroup: GroupActionable,
			wantTone:  ToneDanger,
		},
		{
			name:      "conflict being worked by a live loop belongs to the machine",
			report:    ptr(report(1, false, gatekeeper.ReasonMergeConflict)),
			loops:     []storage.LoopRecord{activeLoop(1)},
			wantCode:  string(gatekeeper.ReasonMergeConflict),
			wantLabel: "merge conflict",
			wantGroup: GroupMachine,
			wantTone:  ToneDanger,
		},
		{
			name:      "failing checks outrank convergence",
			report:    ptr(report(1, false, gatekeeper.ReasonReviewerConvergence, gatekeeper.ReasonCheckFailed)),
			wantCode:  string(gatekeeper.ReasonCheckFailed),
			wantLabel: "CI failing",
			wantGroup: GroupActionable,
			wantTone:  ToneDanger,
		},
		{
			name:      "convergence outranks the diff budget and is always stuck",
			report:    ptr(report(1, false, gatekeeper.ReasonDiffBudgetExceeded, gatekeeper.ReasonReviewerConvergence)),
			loops:     []storage.LoopRecord{activeLoop(1)},
			wantCode:  string(gatekeeper.ReasonReviewerConvergence),
			wantLabel: "convergence stuck",
			wantGroup: GroupStuck,
			wantTone:  ToneStuck,
		},
		{
			name:      "diff budget outranks review debt",
			report:    ptr(report(1, false, gatekeeper.ReasonUnresolvedReviewThread, gatekeeper.ReasonDiffBudgetExceeded)),
			wantCode:  string(gatekeeper.ReasonDiffBudgetExceeded),
			wantLabel: "oversized",
			wantGroup: GroupStuck,
			wantTone:  ToneStuck,
		},
		{
			name:      "review debt outranks pending checks and falls to the operator when nothing is working it",
			report:    ptr(report(1, false, gatekeeper.ReasonCheckPending, gatekeeper.ReasonReviewChangesRequested)),
			wantCode:  string(gatekeeper.ReasonReviewChangesRequested),
			wantLabel: "changes requested",
			wantGroup: GroupActionable,
			wantTone:  ToneActionable,
		},
		{
			name:      "review debt with a live fixer belongs to the machine",
			report:    ptr(report(1, false, gatekeeper.ReasonUnresolvedReviewThread)),
			loops:     []storage.LoopRecord{activeLoop(1)},
			wantCode:  string(gatekeeper.ReasonUnresolvedReviewThread),
			wantLabel: "review debt",
			wantGroup: GroupMachine,
			wantTone:  ToneMachine,
		},
		{
			name:      "pending checks are machine work even with no loop",
			report:    ptr(report(1, false, gatekeeper.ReasonCheckPending)),
			wantCode:  string(gatekeeper.ReasonCheckPending),
			wantLabel: "CI running",
			wantGroup: GroupMachine,
			wantTone:  ToneMachine,
		},
		{
			name:      "a missing review is the operator's move",
			report:    ptr(report(1, false, gatekeeper.ReasonReviewRequired)),
			wantCode:  string(gatekeeper.ReasonReviewRequired),
			wantLabel: "needs review",
			wantGroup: GroupActionable,
			wantTone:  ToneActionable,
		},
		{
			name:      "an eligible pull request waits on a human merge decision",
			report:    ptr(report(1, true)),
			wantCode:  "eligible",
			wantLabel: "awaiting merge OK",
			wantGroup: GroupActionable,
			wantTone:  ToneActionable,
		},
		{
			name:      "a hold is a decision, not progress",
			report:    ptr(report(1, false, gatekeeper.ReasonHold)),
			loops:     []storage.LoopRecord{activeLoop(1)},
			wantCode:  string(gatekeeper.ReasonHold),
			wantLabel: "on hold",
			wantGroup: GroupStuck,
			wantTone:  ToneStuck,
		},
		{
			name:      "a stuck digest item moves the row to stuck without renaming the blocker",
			report:    ptr(report(1, false, gatekeeper.ReasonMergeConflict)),
			loops:     []storage.LoopRecord{activeLoop(1)},
			items:     []escalator.Item{{ID: "queue_retries:proj:q1", Kind: escalator.KindStuck, Reason: escalator.ReasonQueueRetries, Link: testLinker{}.PullRequest("proj", "acme/widgets", 1), AgeSeconds: 3600}},
			wantCode:  string(gatekeeper.ReasonMergeConflict),
			wantLabel: "merge conflict",
			wantGroup: GroupStuck,
			wantTone:  ToneDanger,
		},
		{
			name:      "a waiting digest item names the ask when the gate found nothing",
			report:    ptr(report(1, true)),
			items:     []escalator.Item{{ID: "hitl_question:proj:loop-1", Kind: escalator.KindWaiting, Reason: escalator.ReasonHITLQuestion, Link: testLinker{}.Loop("proj", 1), AgeSeconds: 600}},
			loops:     []storage.LoopRecord{activeLoop(1)},
			wantCode:  "eligible",
			wantLabel: "awaiting merge OK",
			wantGroup: GroupActionable,
			wantTone:  ToneActionable,
		},
		{
			name:      "without a gate report the snapshot's own conflict evidence is read",
			options:   payloadOptions{conflicts: true},
			wantCode:  string(gatekeeper.ReasonMergeConflict),
			wantLabel: "merge conflict",
			wantGroup: GroupActionable,
			wantTone:  ToneDanger,
		},
		{
			name:      "without a gate report failing checks are read off the summary",
			checks:    ptr("SUCCESS, FAILURE"),
			wantCode:  string(gatekeeper.ReasonCheckFailed),
			wantLabel: "CI failing",
			wantGroup: GroupActionable,
			wantTone:  ToneDanger,
		},
		{
			name:      "without a gate report unresolved threads are review debt",
			threads:   ptr(int64(3)),
			wantCode:  string(gatekeeper.ReasonUnresolvedReviewThread),
			wantLabel: "review debt",
			wantGroup: GroupActionable,
			wantTone:  ToneActionable,
		},
		{
			name:      "a draft is machine work, not a decision",
			options:   payloadOptions{draft: true},
			wantCode:  "draft",
			wantLabel: "draft",
			wantGroup: GroupMachine,
			wantTone:  ToneMachine,
		},
		{
			name:      "nothing blocking reads as ready",
			wantCode:  "clear",
			wantLabel: "ready",
			wantGroup: GroupActionable,
			wantTone:  ToneActionable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			record := snapshot(t, 1, testCase.options)
			record.UnresolvedThreadCount = testCase.threads
			record.ReviewState = testCase.reviewState
			record.ChecksSummary = testCase.checks

			input := Input{
				Now:       testNow,
				Snapshots: []storage.PullRequestSnapshotRecord{record},
				Loops:     testCase.loops,
				Escalator: escalator.Snapshot{Items: testCase.items},
				Links:     testLinker{},
			}
			if testCase.report != nil {
				input.Reports = []gatekeeper.Report{*testCase.report}
			}

			row, group := rowFor(t, Classify(input), 1)
			if row.Blocker.Code != testCase.wantCode {
				t.Errorf("blocker code = %q, want %q", row.Blocker.Code, testCase.wantCode)
			}
			if row.Blocker.Label != testCase.wantLabel {
				t.Errorf("blocker label = %q, want %q", row.Blocker.Label, testCase.wantLabel)
			}
			if group != testCase.wantGroup {
				t.Errorf("group = %v, want %v", group, testCase.wantGroup)
			}
			if row.Blocker.Tone != testCase.wantTone {
				t.Errorf("tone = %q, want %q", row.Blocker.Tone, testCase.wantTone)
			}
		})
	}
}

func TestClassifyBucketsEscalatorItemsThatMatchNoPullRequest(t *testing.T) {
	t.Parallel()

	board := Classify(Input{
		Now:   testNow,
		Links: testLinker{},
		Escalator: escalator.Snapshot{Items: []escalator.Item{
			{ID: "hitl_question:proj:loop-9", Kind: escalator.KindWaiting, Reason: escalator.ReasonHITLQuestion, Stage: "worker", Title: "worker loop #9 is waiting on a human", Link: testLinker{}.Loop("proj", 9), AgeSeconds: 900},
			{ID: "circuit_breaker:proj:loop-4", Kind: escalator.KindStuck, Reason: escalator.ReasonCircuitBreaker, Stage: "fixer", Title: "fixer loop #4 tripped its circuit breaker", Link: testLinker{}.Loop("proj", 4), AgeSeconds: 7200},
			{ID: "triage_confirmation:proj:acme/widgets:12", Kind: escalator.KindWaiting, Reason: escalator.ReasonTriageConfirmation, Stage: "triager", Title: "Confirm triage for acme/widgets#12", Link: testLinker{}.Issue("proj", "acme/widgets", 12), AgeSeconds: 60},
		}},
	})

	if got := sectionFor(board, GroupActionable).Count(); got != 2 {
		t.Fatalf("actionable count = %d, want 2", got)
	}
	if got := sectionFor(board, GroupStuck).Count(); got != 1 {
		t.Fatalf("stuck count = %d, want 1", got)
	}
	if got := sectionFor(board, GroupStuck).Rows[0].Ref; got != "loop #4" {
		t.Fatalf("stuck row ref = %q, want loop #4", got)
	}
	if got := sectionFor(board, GroupStuck).Rows[0].Blocker.Label; got != "circuit breaker" {
		t.Fatalf("stuck row chip = %q, want circuit breaker", got)
	}
	// The issue-scoped item has no loop link, so it falls back to its stage.
	refs := map[string]bool{}
	for _, row := range sectionFor(board, GroupActionable).Rows {
		refs[row.Ref] = true
	}
	if !refs["triager"] || !refs["loop #9"] {
		t.Fatalf("actionable refs = %v, want triager and loop #9", refs)
	}
}

func TestClassifyAttachesLoopScopedItemToItsPullRequest(t *testing.T) {
	t.Parallel()

	board := Classify(Input{
		Now:       testNow,
		Snapshots: []storage.PullRequestSnapshotRecord{snapshot(t, 7, payloadOptions{})},
		Loops:     []storage.LoopRecord{activeLoop(7)},
		Links:     testLinker{},
		Escalator: escalator.Snapshot{Items: []escalator.Item{
			{ID: "circuit_breaker:proj:loop-7", Kind: escalator.KindStuck, Reason: escalator.ReasonCircuitBreaker, Link: testLinker{}.Loop("proj", 7), AgeSeconds: 300},
		}},
	})

	if got := board.Total(); got != 1 {
		t.Fatalf("row count = %d, want 1 (the item must fold into its pull request row)", got)
	}
	row, group := rowFor(t, board, 7)
	if group != GroupStuck {
		t.Fatalf("group = %v, want stuck", group)
	}
	if row.Blocker.Label != "circuit breaker" {
		t.Fatalf("chip = %q, want circuit breaker", row.Blocker.Label)
	}
}

func TestClassifyMachineGroupOrdersByDiffSizeAscending(t *testing.T) {
	t.Parallel()

	small := snapshot(t, 1, payloadOptions{additions: 12, deletions: 3})
	large := snapshot(t, 2, payloadOptions{additions: 900, deletions: 400})
	unsized := snapshot(t, 3, payloadOptions{})
	unsized.CapturedAt = iso(testNow.Add(-9 * time.Hour))

	board := Classify(Input{
		Now:       testNow,
		Snapshots: []storage.PullRequestSnapshotRecord{large, unsized, small},
		Reports: []gatekeeper.Report{
			report(1, false, gatekeeper.ReasonMergeConflict),
			report(2, false, gatekeeper.ReasonMergeConflict),
			report(3, false, gatekeeper.ReasonMergeConflict),
		},
		Loops: []storage.LoopRecord{activeLoop(1), activeLoop(2), activeLoop(3)},
		Links: testLinker{},
	})

	section := sectionFor(board, GroupMachine)
	got := make([]string, 0, len(section.Rows))
	for _, row := range section.Rows {
		got = append(got, row.Ref)
	}
	want := []string{"#1", "#2", "#3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("machine order = %v, want %v (smallest diff first, unsized last)", got, want)
	}
	if label := section.Rows[1].Diff.Label(); label != "+900" {
		t.Fatalf("diff label = %q, want +900", label)
	}
	if section.Rows[2].Diff.Known {
		t.Fatalf("row without a captured diff must not report a size")
	}
}

func TestClassifyKeepsOnlyTheLatestOpenSnapshotPerPullRequest(t *testing.T) {
	t.Parallel()

	stale := snapshot(t, 1, payloadOptions{conflicts: true})
	stale.ID = "stale"
	stale.CapturedAt = iso(testNow.Add(-8 * time.Hour))
	fresh := snapshot(t, 1, payloadOptions{})
	fresh.ID = "fresh"
	fresh.CapturedAt = iso(testNow.Add(-15 * time.Minute))
	merged := snapshot(t, 2, payloadOptions{state: "MERGED"})
	closed := snapshot(t, 3, payloadOptions{state: "CLOSED"})

	board := Classify(Input{
		Now:       testNow,
		Snapshots: []storage.PullRequestSnapshotRecord{stale, fresh, merged, closed},
		Links:     testLinker{},
	})

	if got := board.Total(); got != 1 {
		t.Fatalf("row count = %d, want 1 open pull request", got)
	}
	row, _ := rowFor(t, board, 1)
	if row.Blocker.Code != "clear" {
		t.Fatalf("blocker = %q, want the fresh snapshot's verdict", row.Blocker.Code)
	}
}

func TestClassifyHighlightsOnlyStateThatMovedInsideTheRefreshWindow(t *testing.T) {
	t.Parallel()

	moved := snapshot(t, 1, payloadOptions{})
	moved.CapturedAt = iso(testNow.Add(-3 * time.Second))
	settled := snapshot(t, 2, payloadOptions{})
	settled.CapturedAt = iso(testNow.Add(-5 * time.Minute))

	board := Classify(Input{
		Now:       testNow,
		Snapshots: []storage.PullRequestSnapshotRecord{moved, settled},
		Links:     testLinker{},
	})

	if row, _ := rowFor(t, board, 1); !row.Changed {
		t.Fatalf("row 1 changed = false, want true within the %s window", RefreshInterval)
	}
	if row, _ := rowFor(t, board, 2); row.Changed {
		t.Fatalf("row 2 changed = true, want false outside the window")
	}
}

func TestClassifyEmptyInputRendersThreeEmptyGroups(t *testing.T) {
	t.Parallel()

	board := Classify(Input{Now: testNow})

	if len(board.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(board.Sections))
	}
	for index, group := range Groups {
		if board.Sections[index].Group != group {
			t.Fatalf("section %d = %v, want %v", index, board.Sections[index].Group, group)
		}
		if board.Sections[index].Count() != 0 {
			t.Fatalf("section %v is not empty", group)
		}
	}
	if board.GeneratedAt.IsZero() {
		t.Fatalf("GeneratedAt must be stamped even for an empty board")
	}
}

func TestClassifySurvivesUnreadableSnapshotPayload(t *testing.T) {
	t.Parallel()

	record := snapshot(t, 1, payloadOptions{})
	record.PayloadJSON = ptr("{not json")

	board := Classify(Input{Now: testNow, Snapshots: []storage.PullRequestSnapshotRecord{record}, Links: testLinker{}})
	if board.Total() != 1 {
		t.Fatalf("row count = %d, want the row to survive an unreadable payload", board.Total())
	}
}

func sectionFor(board Board, group Group) Section {
	for _, section := range board.Sections {
		if section.Group == group {
			return section
		}
	}
	return Section{Group: group}
}

func TestCompactCountAndRelativeAge(t *testing.T) {
	t.Parallel()

	counts := []struct {
		value int
		want  string
	}{{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1k"}, {1234, "1.2k"}, {9999, "10k"}, {12345, "12k"}, {1500000, "1.5m"}}
	for _, testCase := range counts {
		if got := compactCount(testCase.value); got != testCase.want {
			t.Errorf("compactCount(%d) = %q, want %q", testCase.value, got, testCase.want)
		}
	}

	ages := []struct {
		value time.Duration
		want  string
	}{{0, "1s"}, {45 * time.Second, "45s"}, {12 * time.Minute, "12m"}, {3 * time.Hour, "3h"}, {50 * time.Hour, "2d"}, {800 * 24 * time.Hour, "2y"}}
	for _, testCase := range ages {
		if got := relativeAge(testCase.value); got != testCase.want {
			t.Errorf("relativeAge(%s) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}
