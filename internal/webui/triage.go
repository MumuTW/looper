// Package webui serves the hypermedia operator UI under /ui/. Pages are
// rendered server-side from durable state and refreshed by swapping one content
// region with htmx; the package ships no application JavaScript of its own.
//
// It is a read surface. Nothing here starts, retries, or mutates work, and it
// derives no signal the roles do not already publish: the Escalator collector
// stays the authority for "waiting on a human" and "stuck", and the Gatekeeper
// gate report stays the authority for why a pull request cannot merge. What
// this package owns is the ordering — one primary blocker per row, three groups
// an operator reads top-down.
package webui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

// RefreshInterval is how often the board region re-polls itself. It is also the
// window that decides whether a row renders as freshly changed: the highlight
// is a pure function of how recently the underlying state moved, so no previous
// render has to be remembered between requests.
const RefreshInterval = 10 * time.Second

// Group is one of the three questions the page answers: what can I do, what is
// the machine doing, and what has stopped moving. Every row lands in exactly
// one, and the render order is fixed.
type Group int

const (
	GroupActionable Group = iota
	GroupMachine
	GroupStuck
)

// Groups is the fixed render order.
var Groups = []Group{GroupActionable, GroupMachine, GroupStuck}

// Title is the section heading.
func (g Group) Title() string {
	switch g {
	case GroupActionable:
		return "Actionable now"
	case GroupMachine:
		return "Machine working"
	default:
		return "Stuck — needs decision"
	}
}

// Slug is the group's stable identifier in element ids and CSS hooks.
func (g Group) Slug() string {
	switch g {
	case GroupActionable:
		return "actionable"
	case GroupMachine:
		return "machine"
	default:
		return "stuck"
	}
}

// Empty is the one-sentence empty state.
func (g Group) Empty() string {
	switch g {
	case GroupActionable:
		return "Nothing is waiting on you."
	case GroupMachine:
		return "No loop is working a pull request right now."
	default:
		return "Nothing has stopped moving."
	}
}

// Tone selects a chip tint. It normally follows the row's group accent so each
// section reads as one colour; hard provider failures override to danger so a
// red conflict stays red even while a fixer is repairing it.
type Tone string

const (
	ToneActionable Tone = "actionable"
	ToneMachine    Tone = "machine"
	ToneStuck      Tone = "stuck"
	ToneDanger     Tone = "danger"
)

// ownership answers who the blocker belongs to, which is what decides the
// group. Contested blockers belong to whoever is actually holding them: a
// machine when a loop is on the pull request, the operator when nothing is.
type ownership int

const (
	ownerHuman ownership = iota
	ownerMachine
	ownerHard
	ownerContested
)

// Blocker is the single reason a row is not moving. Code is the stable
// identifier (a Gatekeeper reason code, an Escalator reason, or a local code
// for signals read straight off the snapshot); Label is the operator's words.
type Blocker struct {
	Code  string
	Label string
	Tone  Tone

	// rank is the triage ladder. Lower wins when a row has several reasons.
	rank  int
	owner ownership
}

// The ladder, top-down. Gaps leave room for reason codes that arrive later
// without renumbering the ones an operator has already learned to read.
const (
	rankConflict    = 10
	rankCheckFailed = 20
	rankConvergence = 30
	rankEscalated   = 31
	rankBudget      = 40
	rankHold        = 41
	rankPolicy      = 42
	rankReviewDebt  = 50
	rankReviewMiss  = 55
	rankCheckWait   = 60
	rankEvidence    = 64
	rankDraft       = 70
	rankEligible    = 80
	rankWaiting     = 81
	rankClear       = 90
)

// Row is one triage line. Ref and Title carry the identity; the meta columns
// carry only what fits on one line at 13px without wrapping.
type Row struct {
	// Key is stable across refreshes so a swap replaces a row rather than
	// re-creating a different one in its place.
	Key     string
	Ref     string
	Title   string
	Link    string
	Blocker Blocker

	Threads    int64
	HasThreads bool
	Diff       DiffSize

	// Age is how long the state being shown has been the state, and ChangedAt
	// is when it became so.
	Age       time.Duration
	ChangedAt time.Time
	// Changed marks state that moved within the last refresh window, which is
	// what the row highlight animates.
	Changed bool

	// Summary marks the one muted row that stands in for a run of rows the
	// board folded away. It carries a count and no link: there is nothing to
	// open, because the point of the row is that these are not individually
	// actionable from here.
	Summary bool

	group Group
	// collapsible marks a row that is only ever one instance of a repeated
	// digest reason — a standalone escalator item. A row backed by a pull
	// request is never folded: it is a distinct thing an operator acts on.
	collapsible bool
	// sortSize is the rebase-queue key: smaller diffs merge first. Rows with no
	// measurable diff sort after every sized row.
	sortSize int
	hasSize  bool
}

// ThreadsLabel is the unit word next to the unresolved-thread count.
func (r Row) ThreadsLabel() string {
	if r.Threads == 1 {
		return "thread"
	}
	return "threads"
}

// Section is one rendered group with its rows.
type Section struct {
	Group Group
	Rows  []Row
	// Folded is how many rows the section stopped rendering individually, net
	// of the summary rows that replaced them. It exists so the count stays the
	// number of things that are stuck, not the number of lines drawn.
	Folded int
}

// Count is what the stat tile shows.
func (s Section) Count() int { return len(s.Rows) + s.Folded }

// Board is a full render of the triage page.
type Board struct {
	GeneratedAt time.Time
	Sections    []Section
	// Notices name data sources that could not be read for this render. The
	// page still renders; a missing source narrows the board, it never 500s.
	Notices []string
}

// Total is the number of rows across every group.
func (b Board) Total() int {
	total := 0
	for _, section := range b.Sections {
		total += section.Count()
	}
	return total
}

// PRKey identifies one pull request across the sources that mention it.
type PRKey struct {
	ProjectID string
	Repo      string
	Number    int64
}

type linkIdentity struct {
	ProjectID string
	Link      string
}

func newPRKey(projectID, repo string, number int64) PRKey {
	return PRKey{ProjectID: projectID, Repo: strings.ToLower(strings.TrimSpace(repo)), Number: number}
}

// Input is everything one render derives from. It is deliberately plain data:
// classification is the part worth testing, and it must be testable without a
// database.
type Input struct {
	Now time.Time
	// Snapshots holds the latest snapshot per pull request, in any order.
	Snapshots []storage.PullRequestSnapshotRecord
	// ActiveProjects is the daemon-owned project lifecycle projection. A nil
	// map keeps pure callers backwards-compatible; a non-nil map excludes
	// archived/removed projects from the board.
	ActiveProjects map[string]bool
	// Reports holds the latest merge-gate report per pull request.
	Reports []gatekeeper.Report
	Loops   []storage.LoopRecord
	Queue   []storage.QueueItemRecord
	// Escalator is the digest collector's snapshot. Its items are matched back
	// to rows by link identity, so the same Linker the collector was built with
	// must be passed here.
	Escalator escalator.Snapshot
	Links     escalator.Linker
	Notices   []string
}

// Classify turns durable state into the rendered board.
func Classify(in Input) Board {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	pullRequests := latestSnapshotPerPR(in.Snapshots, in.ActiveProjects)
	for _, loop := range in.Loops {
		if loop.Repo == nil || loop.PRNumber == nil || !domain.IsActiveLoopStatus(domain.LoopStatus(loop.Status)) {
			continue
		}
		if in.ActiveProjects != nil && !in.ActiveProjects[loop.ProjectID] {
			continue
		}
		key := newPRKey(loop.ProjectID, *loop.Repo, *loop.PRNumber)
		if _, exists := pullRequests[key]; exists {
			continue
		}
		capturedAt := loop.UpdatedAt
		if strings.TrimSpace(capturedAt) == "" {
			capturedAt = loop.CreatedAt
		}
		pullRequests[key] = storage.PullRequestSnapshotRecord{
			ID: loop.ID + ":unsnapshotted", ProjectID: loop.ProjectID, Repo: *loop.Repo,
			PRNumber: *loop.PRNumber, CapturedAt: capturedAt, CreatedAt: loop.CreatedAt,
		}
	}
	reports := latestReportPerPR(in.Reports)
	loopsByPR, loopLinks := indexLoops(in.Loops, in.Links)
	queueByPR := indexQueue(in.Queue)
	pullRequestLinks := prLinks(pullRequests, in.Links)

	// Escalator items reach a row through the link the collector already wrote
	// for them. A loop item lands on the pull request its loop is working.
	itemsByPR := map[PRKey][]escalator.Item{}
	standalone := make([]escalator.Item, 0, len(in.Escalator.Items))
	for _, item := range in.Escalator.Items {
		key, matched := PRKey{}, false
		if link := strings.TrimSpace(item.Link); link != "" {
			identity := linkIdentity{ProjectID: item.ProjectID, Link: link}
			if candidate, ok := pullRequestLinks[identity]; ok {
				key, matched = candidate, true
			} else if loop, ok := loopLinks[identity]; ok && loop.Repo != nil && loop.PRNumber != nil {
				candidate := newPRKey(loop.ProjectID, *loop.Repo, *loop.PRNumber)
				if _, known := pullRequests[candidate]; known {
					key, matched = candidate, true
				}
			}
		}
		if matched {
			itemsByPR[key] = append(itemsByPR[key], item)
			continue
		}
		standalone = append(standalone, item)
	}

	rows := make([]Row, 0, len(pullRequests)+len(standalone))
	for key, snapshot := range pullRequests {
		report := reportForSnapshot(reports[key], snapshot)
		if reportHasReason(report, gatekeeper.ReasonPullRequestNotOpen) {
			// Gatekeeper observed the PR closed after the last open snapshot.
			// The report is newer authority than the stale snapshot, so do not
			// resurrect the row until discovery captures a current open state.
			continue
		}
		rows = append(rows, pullRequestRow(now, key, snapshot, report, loopsByPR[key], queueByPR[key], itemsByPR[key], in.Links))
	}
	for _, item := range standalone {
		rows = append(rows, escalatorRow(now, item))
	}

	sections := make([]Section, 0, len(Groups))
	for _, group := range Groups {
		section := Section{Group: group, Rows: []Row{}}
		for _, row := range rows {
			if row.group == group {
				section.Rows = append(section.Rows, row)
			}
		}
		sortRows(group, section.Rows)
		total := len(section.Rows)
		section.Rows = collapseRepeats(section.Rows)
		section.Folded = total - len(section.Rows)
		sections = append(sections, section)
	}

	return Board{GeneratedAt: now, Sections: sections, Notices: in.Notices}
}

// reportForSnapshot prevents a verdict about an older head from being applied
// to the current snapshot. A mismatched report is retained as an explicit stale
// evidence blocker; silently falling back to a clean snapshot would render an
// unevaluated head as ready.
func reportForSnapshot(report *gatekeeper.Report, snapshot storage.PullRequestSnapshotRecord) *gatekeeper.Report {
	if report == nil {
		return nil
	}
	observed, current := strings.TrimSpace(report.ObservedHeadSHA), strings.TrimSpace(snapshot.HeadSHA)
	if observed == "" || current == "" || strings.EqualFold(observed, current) {
		return report
	}
	stale := *report
	stale.Status = gatekeeper.StatusBlocked
	stale.Eligible = false
	stale.Reasons = []gatekeeper.Reason{{Code: gatekeeper.ReasonHeadStale}}
	return &stale
}

func reportHasReason(report *gatekeeper.Report, code gatekeeper.ReasonCode) bool {
	if report == nil {
		return false
	}
	for _, reason := range report.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

// sortRows puts the machine group in rebase-queue order — smallest diff first,
// because that is the order the conflicts clear in — and every other group in
// oldest-first order, because there the age is the complaint.
func sortRows(group Group, rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if group == GroupMachine {
			if left.hasSize != right.hasSize {
				return left.hasSize
			}
			if left.hasSize && left.sortSize != right.sortSize {
				return left.sortSize < right.sortSize
			}
		}
		if left.Blocker.rank != right.Blocker.rank {
			return left.Blocker.rank < right.Blocker.rank
		}
		if left.Age != right.Age {
			return left.Age > right.Age
		}
		return left.Key < right.Key
	})
}

// collapseExemplars is how many rows of one repeated digest reason stay
// visible before the rest fold into a single summary row. Three is enough to
// show that the entries differ only in which issue they name, and few enough
// that no reason can push the section it shares off the screen.
const collapseExemplars = 3

// collapseMinimum is the run length at which folding starts. Below it the
// summary row would replace about as many rows as it hides, which reads worse
// than simply showing them.
const collapseMinimum = 6

// collapseRepeats folds runs of one repeated digest reason into exemplars plus
// one summary row. It is a pure post-pass over an already-sorted slice: the
// input order picks the exemplars, so the section's own sort — oldest first —
// is what surfaces the longest-waiting entries.
func collapseRepeats(rows []Row) []Row {
	counts := map[string]int{}
	for _, row := range rows {
		if key, ok := collapseKey(row); ok {
			counts[key]++
		}
	}

	out := make([]Row, 0, len(rows))
	seen := map[string]int{}
	for _, row := range rows {
		key, ok := collapseKey(row)
		if !ok || counts[key] < collapseMinimum {
			out = append(out, row)
			continue
		}
		seen[key]++
		switch {
		case seen[key] <= collapseExemplars:
			out = append(out, row)
		case seen[key] == collapseExemplars+1:
			out = append(out, summaryRow(row, counts[key]-collapseExemplars))
		}
	}
	return out
}

// collapseKey names the run a row belongs to. Only standalone digest rows have
// one; everything else is rendered on its own line.
func collapseKey(row Row) (string, bool) {
	if !row.collapsible || row.Summary {
		return "", false
	}
	return row.Blocker.Code, true
}

// summaryRow stands in for the rows that were folded away. It keeps the group
// and the blocker code — so the section still sorts and reads as one thing —
// and drops the link, the age, and the meta columns, which described the
// exemplar rather than the run.
func summaryRow(exemplar Row, hidden int) Row {
	return Row{
		Key:     "more-" + exemplar.group.Slug() + "-" + slug(exemplar.Blocker.Code),
		Title:   summaryTitle(exemplar.Blocker.Code, hidden),
		Blocker: Blocker{Code: exemplar.Blocker.Code, Label: exemplar.Blocker.Label, Tone: exemplar.Blocker.Tone, rank: exemplar.Blocker.rank, owner: exemplar.Blocker.owner},
		Summary: true,
		group:   exemplar.group,
	}
}

func summaryTitle(code string, hidden int) string {
	return fmt.Sprintf("…and %d more %s", hidden, summaryPhrase(escalator.Reason(code)))
}

// summaryPhrase completes "…and N more ⟨phrase⟩" for a digest reason.
func summaryPhrase(reason escalator.Reason) string {
	switch reason {
	case escalator.ReasonTriageConfirmation:
		return "issues awaiting triage confirmation"
	case escalator.ReasonPlannerEscalation:
		return "items awaiting a decision"
	case escalator.ReasonHITLQuestion:
		return "items awaiting an answer"
	case escalator.ReasonReviewStall:
		return "pull requests with stalled reviews"
	case escalator.ReasonEligibleAdvisePR:
		return "pull requests awaiting a merge decision"
	case escalator.ReasonCircuitBreaker:
		return "items behind a tripped circuit breaker"
	case escalator.ReasonQueueRetries:
		return "items with retries exhausted"
	case escalator.ReasonTriageNotRouted:
		return "issues enrolled but never routed"
	case escalator.ReasonStalePRHead:
		return "pull requests with stale evidence"
	default:
		return "items: " + escalatorLabel(reason)
	}
}

func pullRequestRow(now time.Time, key PRKey, snapshot storage.PullRequestSnapshotRecord, report *gatekeeper.Report, loops []storage.LoopRecord, queue []storage.QueueItemRecord, items []escalator.Item, links escalator.Linker) Row {
	payload := decodeSnapshotPayload(snapshot.PayloadJSON)
	capturedAt := parseTimestamp(snapshot.CapturedAt)
	changedAt := capturedAt
	if report != nil {
		changedAt = later(changedAt, parseTimestamp(report.EvaluatedAt))
	}

	working := false
	for _, loop := range loops {
		if machineLoopStatus(loop.Status) {
			working = true
			changedAt = later(changedAt, parseTimestamp(loop.UpdatedAt))
		}
	}
	for _, item := range queue {
		if !queueSettled(item.Status) {
			working = true
			changedAt = later(changedAt, parseTimestamp(item.UpdatedAt))
		}
	}
	blocker := primaryBlocker(report, payload, snapshot, items)
	group := groupFor(blocker, working)
	stuck := false
	for _, item := range items {
		if item.Kind == escalator.KindStuck {
			stuck = true
			changedAt = later(changedAt, now.Add(-time.Duration(item.AgeSeconds)*time.Second))
		}
	}
	// A stuck digest item means the machine has already given up on this pull
	// request. That outranks whichever reason the chip names.
	if stuck {
		group = GroupStuck
	}

	title := strings.TrimSpace(derefString(snapshot.Title))
	if title == "" {
		title = fmt.Sprintf("%s#%d", snapshot.Repo, key.Number)
	}
	link := ""
	if links != nil {
		link = links.PullRequest(key.ProjectID, snapshot.Repo, snapshot.PRNumber)
	}

	row := Row{
		Key:       fmt.Sprintf("pr-%s-%s-%d", slug(key.ProjectID), slug(key.Repo), key.Number),
		Ref:       fmt.Sprintf("#%d", key.Number),
		Title:     title,
		Link:      link,
		Blocker:   blocker,
		Diff:      payload.diff,
		ChangedAt: changedAt,
		group:     group,
		sortSize:  payload.diff.Total(),
		hasSize:   payload.diff.Known,
	}
	if snapshot.UnresolvedThreadCount != nil && *snapshot.UnresolvedThreadCount > 0 {
		row.Threads, row.HasThreads = *snapshot.UnresolvedThreadCount, true
	}
	row.Blocker.Tone = toneFor(row.Blocker, group)
	applyAge(&row, now)
	return row
}

func escalatorRow(now time.Time, item escalator.Item) Row {
	group := GroupActionable
	if item.Kind == escalator.KindStuck {
		group = GroupStuck
	}
	blocker := escalatorBlocker(item)
	row := Row{
		Key:         "item-" + slug(item.ID),
		Ref:         escalatorRef(item),
		Title:       item.Title,
		Link:        item.Link,
		Blocker:     blocker,
		ChangedAt:   now.Add(-time.Duration(item.AgeSeconds) * time.Second),
		group:       group,
		collapsible: true,
	}
	row.Blocker.Tone = toneFor(row.Blocker, group)
	applyAge(&row, now)
	return row
}

func applyAge(row *Row, now time.Time) {
	if row.ChangedAt.IsZero() {
		return
	}
	row.Age = now.Sub(row.ChangedAt)
	if row.Age < 0 {
		row.Age = 0
	}
	row.Changed = row.Age <= RefreshInterval
}

func groupFor(blocker Blocker, working bool) Group {
	switch blocker.owner {
	case ownerHard:
		return GroupStuck
	case ownerMachine:
		return GroupMachine
	case ownerHuman:
		if working && blocker.rank == rankClear {
			return GroupMachine
		}
		return GroupActionable
	default:
		if working {
			return GroupMachine
		}
		return GroupActionable
	}
}

// toneFor keeps one accent per group so a section reads as a single colour,
// except for the two blockers an operator must never mistake for progress.
func toneFor(blocker Blocker, group Group) Tone {
	if blocker.Tone == ToneDanger {
		return ToneDanger
	}
	switch group {
	case GroupActionable:
		return ToneActionable
	case GroupMachine:
		return ToneMachine
	default:
		return ToneStuck
	}
}

// primaryBlocker walks the triage ladder over every source that can name a
// reason and keeps the highest-ranked one.
func primaryBlocker(report *gatekeeper.Report, payload snapshotPayload, snapshot storage.PullRequestSnapshotRecord, items []escalator.Item) Blocker {
	best := Blocker{Code: "clear", Label: "ready", rank: rankClear, owner: ownerHuman}
	consider := func(candidate Blocker) {
		if candidate.rank < best.rank {
			best = candidate
		}
	}

	if report != nil {
		if report.Eligible {
			if strings.EqualFold(strings.TrimSpace(report.Mode), "auto") {
				consider(Blocker{Code: "eligible_auto", Label: "auto merge pending", rank: rankEligible, owner: ownerMachine})
			} else {
				consider(Blocker{Code: "eligible", Label: "awaiting merge OK", rank: rankEligible, owner: ownerHuman})
			}
		}
		for _, reason := range report.Reasons {
			if candidate, ok := gatekeeperBlocker(reason.Code); ok {
				consider(candidate)
			}
		}
	} else {
		// No gate report yet: read what the snapshot itself already proves.
		if payload.conflicts {
			consider(blockerConflict())
		}
		if checksFailing(snapshot.ChecksSummary) {
			consider(blockerCheckFailed())
		}
		if snapshot.UnresolvedThreadCount != nil && *snapshot.UnresolvedThreadCount > 0 {
			consider(Blocker{Code: "unresolved_review_thread", Label: "review debt", rank: rankReviewDebt, owner: ownerContested})
		}
		if strings.EqualFold(derefString(snapshot.ReviewState), "CHANGES_REQUESTED") {
			consider(Blocker{Code: "review_changes_requested", Label: "changes requested", rank: rankReviewDebt, owner: ownerContested})
		}
		if strings.EqualFold(derefString(snapshot.ReviewState), "REVIEW_REQUIRED") {
			consider(Blocker{Code: string(gatekeeper.ReasonReviewRequired), Label: "needs review", rank: rankReviewMiss, owner: ownerHuman})
		}
		if checksPending(snapshot.ChecksSummary) {
			consider(Blocker{Code: "required_check_pending", Label: "CI running", rank: rankCheckWait, owner: ownerMachine})
		}
		if strings.EqualFold(derefString(snapshot.ReviewState), "APPROVED") {
			consider(Blocker{Code: "review_approved", Label: "review approved", rank: rankEligible, owner: ownerHuman})
		}
	}
	if payload.draft {
		consider(Blocker{Code: "draft", Label: "draft", rank: rankDraft, owner: ownerMachine})
	}
	for _, item := range items {
		consider(escalatorBlocker(item))
	}
	return best
}

func blockerConflict() Blocker {
	return Blocker{Code: string(gatekeeper.ReasonMergeConflict), Label: "merge conflict", Tone: ToneDanger, rank: rankConflict, owner: ownerContested}
}

func blockerCheckFailed() Blocker {
	return Blocker{Code: string(gatekeeper.ReasonCheckFailed), Label: "CI failing", Tone: ToneDanger, rank: rankCheckFailed, owner: ownerContested}
}

func gatekeeperBlocker(code gatekeeper.ReasonCode) (Blocker, bool) {
	switch code {
	case gatekeeper.ReasonMergeConflict:
		return blockerConflict(), true
	case gatekeeper.ReasonCheckFailed:
		return blockerCheckFailed(), true
	case gatekeeper.ReasonCheckCancelled:
		return Blocker{Code: string(code), Label: "CI cancelled", Tone: ToneDanger, rank: rankCheckFailed + 1, owner: ownerContested}, true
	case gatekeeper.ReasonReviewerConvergence:
		return Blocker{Code: string(code), Label: "convergence stuck", rank: rankConvergence, owner: ownerHard}, true
	case gatekeeper.ReasonDiffBudgetExceeded:
		return Blocker{Code: string(code), Label: "oversized", rank: rankBudget, owner: ownerHard}, true
	case gatekeeper.ReasonHold:
		return Blocker{Code: string(code), Label: "on hold", rank: rankHold, owner: ownerHard}, true
	case gatekeeper.ReasonProjectPolicyDenied:
		return Blocker{Code: string(code), Label: "policy denied", rank: rankPolicy, owner: ownerHard}, true
	case gatekeeper.ReasonReviewChangesRequested:
		return Blocker{Code: string(code), Label: "changes requested", rank: rankReviewDebt, owner: ownerContested}, true
	case gatekeeper.ReasonUnresolvedReviewThread:
		return Blocker{Code: string(code), Label: "review debt", rank: rankReviewDebt + 1, owner: ownerContested}, true
	case gatekeeper.ReasonReviewRequired:
		return Blocker{Code: string(code), Label: "needs review", rank: rankReviewMiss, owner: ownerHuman}, true
	case gatekeeper.ReasonCheckPending:
		return Blocker{Code: string(code), Label: "CI running", rank: rankCheckWait, owner: ownerMachine}, true
	case gatekeeper.ReasonCheckMissing:
		return Blocker{Code: string(code), Label: "CI not reported", rank: rankCheckWait + 1, owner: ownerMachine}, true
	case gatekeeper.ReasonCodexReviewMissing:
		return Blocker{Code: string(code), Label: "review pending", rank: rankCheckWait + 2, owner: ownerMachine}, true
	case gatekeeper.ReasonMergeabilityNotClean:
		return Blocker{Code: string(code), Label: "mergeability unclear", rank: rankCheckWait + 3, owner: ownerMachine}, true
	case gatekeeper.ReasonHeadStale, gatekeeper.ReasonProviderStateUnavailable, gatekeeper.ReasonProviderStateAmbiguous:
		return Blocker{Code: string(code), Label: "evidence refreshing", rank: rankEvidence, owner: ownerMachine}, true
	case gatekeeper.ReasonPullRequestDraft:
		return Blocker{Code: string(code), Label: "draft", rank: rankDraft, owner: ownerMachine}, true
	default:
		label := strings.ReplaceAll(strings.TrimSpace(string(code)), "_", " ")
		if label == "" {
			label = "blocked"
		}
		return Blocker{Code: string(code), Label: label, rank: rankReviewMiss, owner: ownerContested}, true
	}
}

func escalatorBlocker(item escalator.Item) Blocker {
	rank, owner := rankWaiting, ownerHuman
	if item.Kind == escalator.KindStuck {
		rank, owner = rankEscalated, ownerHard
	}
	return Blocker{Code: string(item.Reason), Label: escalatorLabel(item.Reason), rank: rank, owner: owner}
}

func escalatorLabel(reason escalator.Reason) string {
	switch reason {
	case escalator.ReasonTriageConfirmation:
		return "confirm triage"
	case escalator.ReasonPlannerEscalation:
		return "needs decision"
	case escalator.ReasonHITLQuestion:
		return "answer needed"
	case escalator.ReasonReviewStall:
		return "review stalled"
	case escalator.ReasonEligibleAdvisePR:
		return "awaiting merge OK"
	case escalator.ReasonCircuitBreaker:
		return "circuit breaker"
	case escalator.ReasonQueueRetries:
		return "retries exhausted"
	case escalator.ReasonTriageNotRouted:
		return "never routed"
	case escalator.ReasonStalePRHead:
		return "stale evidence"
	default:
		return strings.ReplaceAll(string(reason), "_", " ")
	}
}

// escalatorRef labels a standalone digest row with the thing it points at,
// which for loop-scoped items is the loop and otherwise the stage that raised it.
func escalatorRef(item escalator.Item) string {
	if index := strings.LastIndex(item.Link, "/loops/"); index >= 0 {
		return "loop #" + item.Link[index+len("/loops/"):]
	}
	if stage := strings.TrimSpace(item.Stage); stage != "" {
		return stage
	}
	return "item"
}

// DiffSize is the changed-line count read out of the snapshot's unified diff.
// The Gatekeeper diff-budget evidence is not a substitute: it records changed
// files and deletions but no additions, so mixing the two would produce a
// rebase queue ordered on two different scales.
type DiffSize struct {
	Additions int
	Deletions int
	Truncated bool
	Known     bool
}

// Total is the sort key: the whole change, not just what it adds.
func (d DiffSize) Total() int { return d.Additions + d.Deletions }

// Label is the compact meta-column form, e.g. "+1.2k".
func (d DiffSize) Label() string {
	if !d.Known {
		return ""
	}
	prefix := "+"
	if d.Truncated {
		prefix = "+≥"
	}
	return prefix + compactCount(d.Additions)
}

// Detail is the title-attribute expansion of Label.
func (d DiffSize) Detail() string {
	if !d.Known {
		return ""
	}
	detail := fmt.Sprintf("%d added, %d removed", d.Additions, d.Deletions)
	if d.Truncated {
		detail += " (diff truncated at capture)"
	}
	return detail
}

type snapshotPayload struct {
	draft     bool
	conflicts bool
	diff      DiffSize
}

// decodeSnapshotPayload reads the capture payload written by the GitHub
// gateway: {"detail": <PullRequestDetail>, "diff": "<unified diff>"}. The
// detail is marshalled without json tags on most fields, so both the Go field
// spelling and the lower-camel spelling are accepted.
func decodeSnapshotPayload(raw *string) snapshotPayload {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return snapshotPayload{}
	}
	var parsed struct {
		Detail    map[string]any `json:"detail"`
		Diff      string         `json:"diff"`
		Truncated bool           `json:"diffTruncated"`
	}
	if err := json.Unmarshal([]byte(*raw), &parsed); err != nil {
		return snapshotPayload{}
	}
	out := snapshotPayload{
		draft:     payloadBool(parsed.Detail, "isDraft", "IsDraft"),
		conflicts: payloadBool(parsed.Detail, "hasConflicts", "HasConflicts"),
	}
	if !out.conflicts && strings.EqualFold(payloadString(parsed.Detail, "mergeStateStatus", "MergeableState"), "DIRTY") {
		out.conflicts = true
	}
	if parsed.Diff != "" {
		out.diff = countDiff(parsed.Diff)
		out.diff.Truncated = parsed.Truncated
	}
	return out
}

// countDiff counts changed lines in a unified diff. It is a scan, not a parse:
// the file headers (+++/---) are the only ambiguity worth excluding.
func countDiff(diff string) DiffSize {
	size := DiffSize{Known: true}
	for len(diff) > 0 {
		line := diff
		if index := strings.IndexByte(diff, '\n'); index >= 0 {
			line, diff = diff[:index], diff[index+1:]
		} else {
			diff = ""
		}
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			size.Additions++
		case strings.HasPrefix(line, "-"):
			size.Deletions++
		}
	}
	return size
}

func payloadBool(detail map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := detail[key].(bool); ok && value {
			return true
		}
	}
	return false
}

func payloadString(detail map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := detail[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// checksFailing and checksPending read the comma-joined check conclusions the
// GitHub gateway stores on the snapshot. They exist only for pull requests the
// Gatekeeper has not evaluated yet; a gate report always outranks them.
func checksFailing(summary *string) bool {
	lower := strings.ToLower(derefString(summary))
	return strings.Contains(lower, "failure") || strings.Contains(lower, "failed") ||
		strings.Contains(lower, "error") || strings.Contains(lower, "cancel") ||
		strings.Contains(lower, "timed_out") || strings.Contains(lower, "action_required")
}

func checksPending(summary *string) bool {
	lower := strings.ToLower(derefString(summary))
	return strings.Contains(lower, "pending") || strings.Contains(lower, "in_progress") ||
		strings.Contains(lower, "queued") || strings.Contains(lower, "expected")
}

// latestSnapshotPerPR keeps the newest capture per pull request and drops
// everything that is not an open pull request.
func latestSnapshotPerPR(records []storage.PullRequestSnapshotRecord, activeProjects map[string]bool) map[PRKey]storage.PullRequestSnapshotRecord {
	out := map[PRKey]storage.PullRequestSnapshotRecord{}
	for _, record := range records {
		if activeProjects != nil && !activeProjects[record.ProjectID] {
			continue
		}
		if !openPullRequest(record) {
			continue
		}
		key := newPRKey(record.ProjectID, record.Repo, record.PRNumber)
		if existing, ok := out[key]; ok && !parseTimestamp(record.CapturedAt).After(parseTimestamp(existing.CapturedAt)) {
			continue
		}
		out[key] = record
	}
	return out
}

// openPullRequest treats an unstated state as open: a snapshot exists because
// a role was working the pull request, and hiding it on missing evidence would
// silently shrink the board.
func openPullRequest(record storage.PullRequestSnapshotRecord) bool {
	var parsed struct {
		Detail struct {
			State       string `json:"State"`
			StateAlt    string `json:"state"`
			MergedAt    string `json:"MergedAt"`
			MergedAtAlt string `json:"mergedAt"`
		} `json:"detail"`
	}
	if record.PayloadJSON == nil || json.Unmarshal([]byte(*record.PayloadJSON), &parsed) != nil {
		return true
	}
	if strings.TrimSpace(parsed.Detail.MergedAt) != "" || strings.TrimSpace(parsed.Detail.MergedAtAlt) != "" {
		return false
	}
	state := parsed.Detail.State
	if state == "" {
		state = parsed.Detail.StateAlt
	}
	if strings.TrimSpace(state) == "" {
		return true
	}
	return strings.EqualFold(state, "OPEN")
}

func latestReportPerPR(reports []gatekeeper.Report) map[PRKey]*gatekeeper.Report {
	out := map[PRKey]*gatekeeper.Report{}
	for index := range reports {
		report := reports[index]
		key := newPRKey(report.ProjectID, report.Repo, report.PRNumber)
		if existing, ok := out[key]; ok && !parseTimestamp(report.EvaluatedAt).After(parseTimestamp(existing.EvaluatedAt)) {
			continue
		}
		out[key] = &report
	}
	return out
}

func indexLoops(records []storage.LoopRecord, links escalator.Linker) (map[PRKey][]storage.LoopRecord, map[linkIdentity]storage.LoopRecord) {
	byPR := map[PRKey][]storage.LoopRecord{}
	byLink := map[linkIdentity]storage.LoopRecord{}
	for _, loop := range records {
		if loop.Repo != nil && loop.PRNumber != nil {
			key := newPRKey(loop.ProjectID, *loop.Repo, *loop.PRNumber)
			byPR[key] = append(byPR[key], loop)
		}
		if links != nil {
			if link := strings.TrimSpace(links.Loop(loop.ProjectID, loop.Seq)); link != "" {
				byLink[linkIdentity{ProjectID: loop.ProjectID, Link: link}] = loop
			}
		}
	}
	return byPR, byLink
}

func indexQueue(records []storage.QueueItemRecord) map[PRKey][]storage.QueueItemRecord {
	out := map[PRKey][]storage.QueueItemRecord{}
	for _, item := range records {
		if item.ProjectID == nil || item.Repo == nil || item.PRNumber == nil {
			continue
		}
		key := newPRKey(*item.ProjectID, *item.Repo, *item.PRNumber)
		out[key] = append(out[key], item)
	}
	return out
}

func prLinks(pullRequests map[PRKey]storage.PullRequestSnapshotRecord, links escalator.Linker) map[linkIdentity]PRKey {
	out := make(map[linkIdentity]PRKey, len(pullRequests))
	if links == nil {
		return out
	}
	for key, snapshot := range pullRequests {
		if link := strings.TrimSpace(links.PullRequest(key.ProjectID, snapshot.Repo, key.Number)); link != "" {
			out[linkIdentity{ProjectID: key.ProjectID, Link: link}] = key
		}
	}
	return out
}

func queueSettled(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "manual_intervention":
		return true
	default:
		return false
	}
}

// machineLoopStatus excludes lifecycle-active states that deliberately stop
// automatic progress. A paused or human-held loop can still own a PR, but it
// must not make the board claim that the machine is advancing it.
func machineLoopStatus(status string) bool {
	switch domain.LoopStatus(status) {
	case domain.LoopStatusIdle, domain.LoopStatusQueued, domain.LoopStatusRunning, domain.LoopStatusWaiting:
		return true
	default:
		return false
	}
}

func parseTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func later(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// slug reduces an identifier to what is safe in an element id.
func slug(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	return builder.String()
}
