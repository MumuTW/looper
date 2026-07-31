package fixer

import (
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/labels"
)

func summaryFixture() PullRequestSummary {
	return PullRequestSummary{
		Number: 42, State: "OPEN", HeadSHA: "head-1",
		UpdatedAt: "2026-07-30T09:00:00Z", Labels: []string{"a", "b"},
	}
}

// UpdatedAt is the only field that moves when a reviewer comments without pushing,
// so a provider that cannot supply it must never skip — otherwise the fingerprint
// would miss the very trigger this lane exists for. An empty fingerprint is how
// that refusal is expressed.
func TestExaminationFingerprintIsEmptyWithoutUpdatedAt(t *testing.T) {
	t.Parallel()
	pr := summaryFixture()
	pr.UpdatedAt = ""
	if got := examinationFingerprint(pr); got != "" {
		t.Fatalf("fingerprint without UpdatedAt = %q, want empty so skipping is disabled", got)
	}
	if examinationFingerprint(summaryFixture()) == "" {
		t.Fatal("fingerprint with UpdatedAt = empty, want a value")
	}
}

// Label order comes from the forge and is not guaranteed stable; an unstable
// fingerprint would silently disable skipping rather than fail loudly.
func TestExaminationFingerprintIgnoresLabelOrder(t *testing.T) {
	t.Parallel()
	first := summaryFixture()
	second := summaryFixture()
	second.Labels = []string{"b", "a"}
	if examinationFingerprint(first) != examinationFingerprint(second) {
		t.Fatal("fingerprint changed with label order; skipping would never engage")
	}
}

func TestExaminationFingerprintChangesWithEveryObservableField(t *testing.T) {
	t.Parallel()
	base := examinationFingerprint(summaryFixture())
	for _, testCase := range []struct {
		name   string
		mutate func(*PullRequestSummary)
	}{
		{name: "head sha", mutate: func(p *PullRequestSummary) { p.HeadSHA = "head-2" }},
		{name: "updated at", mutate: func(p *PullRequestSummary) { p.UpdatedAt = "2026-07-30T10:00:00Z" }},
		{name: "state", mutate: func(p *PullRequestSummary) { p.State = "CLOSED" }},
		{name: "draft", mutate: func(p *PullRequestSummary) { p.IsDraft = true }},
		{name: "labels", mutate: func(p *PullRequestSummary) { p.Labels = []string{"a", "b", labels.HoldGlobal} }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			changed := summaryFixture()
			testCase.mutate(&changed)
			if examinationFingerprint(changed) == base {
				t.Fatalf("fingerprint unchanged after %s changed; a stale clean result would be reused", testCase.name)
			}
		})
	}
}

func TestSkipCleanExamination(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	fingerprint := examinationFingerprint(summaryFixture())
	clean := func(fp string, examinedAt time.Time) cleanExamination {
		return cleanExamination{Repo: "acme/looper", PRNumber: 42, Fingerprint: fp, ExaminedAt: examinedAt.Format(time.RFC3339Nano)}
	}

	for _, testCase := range []struct {
		name        string
		previous    cleanExamination
		hasPrevious bool
		want        bool
	}{
		{name: "unchanged and fresh", previous: clean(fingerprint, now.Add(-time.Minute)), hasPrevious: true, want: true},
		{name: "no previous examination", hasPrevious: false, want: false},
		{name: "previous had no fingerprint", previous: clean("", now.Add(-time.Minute)), hasPrevious: true, want: false},
		{name: "fingerprint changed", previous: clean("something-else", now.Add(-time.Minute)), hasPrevious: true, want: false},
		{name: "unparseable timestamp", previous: cleanExamination{Fingerprint: fingerprint, ExaminedAt: "not-a-time"}, hasPrevious: true, want: false},
		// Literal durations rather than maxCleanSkipAge ± something: expressing these
		// in terms of the constant under test makes them pass for any value of it.
		{name: "29 minutes old", previous: clean(fingerprint, now.Add(-29*time.Minute)), hasPrevious: true, want: true},
		{name: "31 minutes old", previous: clean(fingerprint, now.Add(-31*time.Minute)), hasPrevious: true, want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := skipCleanExamination(testCase.previous, testCase.hasPrevious, fingerprint, now); got != testCase.want {
				t.Fatalf("skipCleanExamination() = %t, want %t", got, testCase.want)
			}
		})
	}
}
