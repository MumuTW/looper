package reproducer

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/triager"
)

func TestReproducerCommitsAndRecordsAProvenFailure(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }"})

	result := fixture.discover()
	if result.Reproduced != 1 || result.Unreproducible != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want one reproduction", result)
	}
	status := fixture.status()
	if status.Record == nil {
		t.Fatalf("status = %#v, want a persisted Reproduction Record", status)
	}
	record := *status.Record
	if record.Command != "go test ./pkg -run TestCrash" || record.CommitSHA != "repro111" {
		t.Fatalf("record = %#v, want the proven command and its commit", record)
	}
	if len(record.Files) != 1 || record.Files[0].Path != "pkg/crash_test.go" || record.Files[0].SHA256 == "" {
		t.Fatalf("record files = %#v, want the reproduction file hashed at commit time", record.Files)
	}
	if !status.PlannerAllowed(testCandidateKey()) {
		t.Fatalf("status.PlannerAllowed(testCandidateKey()) = false, want Planner unblocked after a reproduction")
	}
	if len(fixture.git.commits) != 1 || !strings.Contains(fixture.git.commits[0].Message, reproduction.CommitMarker(record.IdempotencyKey)) {
		t.Fatalf("commits = %#v, want a single marked reproduction commit", fixture.git.commits)
	}
	// The manifest travels with the branch so later Roles read the record from
	// their own worktree instead of re-deriving it from the diff.
	manifest, present, err := reproduction.ReadManifest(fixture.worktree)
	if err != nil || !present {
		t.Fatalf("ReadManifest() = %v, %v", present, err)
	}
	if manifest.IdempotencyKey != record.IdempotencyKey || manifest.IssueNumber != testIssue {
		t.Fatalf("manifest = %#v, want it bound to the attempt and the Issue", manifest)
	}
}

// A command that passes before any fix proves nothing. Accepting it would make
// the whole gate vacuous, so it is rejected on observation rather than on the
// agent's description of what it wrote.
func TestReproducerRejectsACandidateThatPassesImmediately(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.proofPass = true
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "func TestCrash(t *testing.T) {}"})

	result := fixture.discover()
	if result.Reproduced != 0 || result.Unreproducible != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the candidate rejected", result)
	}
	status := fixture.status()
	if status.Record != nil {
		t.Fatalf("status.Record = %#v, want no Reproduction Record", status.Record)
	}
	if status.Unreproducible == nil || status.Unreproducible.Reason != reproduction.ReasonCommandPassedOnBase {
		t.Fatalf("status.Unreproducible = %#v, want %s", status.Unreproducible, reproduction.ReasonCommandPassedOnBase)
	}
	if status.PlannerAllowed(testCandidateKey()) {
		t.Fatalf("status.PlannerAllowed(testCandidateKey()) = true, want Planner still blocked")
	}
	if len(fixture.git.commits) != 0 {
		t.Fatalf("commits = %#v, want nothing committed for a non-reproduction", fixture.git.commits)
	}
}

// Replay after a crash between authoring and acknowledgement: the manifest is
// already committed on the branch, so the attempt adopts it instead of running
// the agent again and producing a second reproduction commit.
func TestReproducerAdoptsItsOwnCommittedWorkAfterACrash(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	report := fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraftNow("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }"})

	// Simulate the crashed run: the reproduction is committed and the manifest
	// is finalized on the branch, but no record was ever acknowledged.
	key := reproduction.IdempotencyKey(testProjectID, testRepo, testIssue, report.IdempotencyKey)
	hashes, err := reproduction.HashFiles(fixture.worktree, []string{"pkg/crash_test.go"})
	if err != nil {
		t.Fatalf("HashFiles() error = %v", err)
	}
	if err := reproduction.WriteManifest(fixture.worktree, reproduction.Manifest{
		Version: reproduction.ManifestVersion, Repo: testRepo, IssueNumber: testIssue,
		Command: "go test ./pkg -run TestCrash", Files: hashes, IdempotencyKey: key, BaseSHA: "base000",
		ExpectedFailure: reproduction.ExpectedFailure{Test: testSignatureTest, Message: testSignatureMessage},
	}); err != nil {
		t.Fatalf("WriteManifest() error = %v", err)
	}
	fixture.git.headSHA = "recovered1"
	fixture.git.headFiles = []string{"pkg/crash_test.go", reproduction.ManifestRelPath}

	result := fixture.discover()
	if result.Reproduced != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the committed reproduction adopted", result)
	}
	if fixture.agent.starts != 0 {
		t.Fatalf("agent starts = %d, want the crashed run's work adopted without re-authoring", fixture.agent.starts)
	}
	if len(fixture.git.commits) != 0 {
		t.Fatalf("commits = %#v, want no second reproduction commit", fixture.git.commits)
	}
	status := fixture.status()
	if status.Record == nil || status.Record.CommitSHA != "recovered1" {
		t.Fatalf("status.Record = %#v, want the already-committed reproduction acknowledged", status.Record)
	}
}

func TestReproducerReplaysASettledIssueWithoutRework(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }"})

	fixture.discover()
	second := fixture.discover()
	if second.Reproduced != 0 || second.Skipped != 1 {
		t.Fatalf("second DiscoverIssues() = %#v, want the persisted reproduction reused", second)
	}
	if fixture.agent.starts != 1 || len(fixture.git.commits) != 1 {
		t.Fatalf("agent starts / commits = %d / %d, want exactly one reproduction", fixture.agent.starts, len(fixture.git.commits))
	}
}

// An agent turn that did not complete says nothing about reproducibility. It
// must not be recorded as "cannot reproduce", or a timeout would permanently
// stop an Issue that reproduces fine.
func TestReproducerLeavesAnIncompleteAgentTurnOpen(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.agent.status = "timeout"
	fixture.seedTriageReport(triager.ClassificationBug)

	result := fixture.discover()
	if result.Reproduced != 0 || result.Unreproducible != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the attempt left open", result)
	}
	status := fixture.status()
	if status.Settled(testCandidateKey()) {
		t.Fatalf("status = %#v, want nothing settled by an incomplete agent turn", status)
	}
	if !status.Attempted {
		t.Fatalf("status.Attempted = false, want the durable claim written before the agent ran")
	}
}

func TestReproducerPromptRefusesToInventAPassingTest(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{Attempted: []string{"ran the sample"}, ObservedInstead: "it worked"})

	fixture.discover()
	if len(fixture.agent.prompts) != 1 {
		t.Fatalf("agent prompts = %d, want one", len(fixture.agent.prompts))
	}
	prompt := fixture.agent.prompts[0]
	for _, want := range []string{"Do not fix the bug", "must exit non-zero", "a command that passes immediately is rejected", CannotReproduceRelPath} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

// The reproduction commit must carry exactly the declared reproduction files
// plus the manifest. An undeclared file swept into the commit is rejected
// rather than recorded as the authority, so reverting it later cannot make the
// command pass without fixing the bug.
func TestReproducerRejectsACommitWithUndeclaredFiles(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }"})
	// Simulate `git add -A` having swept an undeclared source edit into the
	// head commit alongside the declared artifacts.
	fixture.git.headFiles = []string{"pkg/crash_test.go", reproduction.ManifestRelPath, "src/exploratory.go"}

	result := fixture.discover()
	if result.Reproduced != 0 || result.Unreproducible != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the undeclared commit rejected", result)
	}
	status := fixture.status()
	if status.Record != nil {
		t.Fatalf("status.Record = %#v, want no authority recorded for an undeclared commit", status.Record)
	}
}

// A proof command that could not be executed (timeout, cancellation,
// infrastructure) is a transient condition. The Issue must be left open for the
// next tick rather than permanently parked for a human.
func TestReproducerRetriesAProofCommandErrorInsteadOfParking(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }"})
	fixture.proofResult = &reproduction.Result{Reason: reproduction.ReasonCommandError, Summary: "command timed out"}

	result := fixture.discover()
	if result.Reproduced != 0 || result.Unreproducible != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the attempt left open (skipped), not parked", result)
	}
	status := fixture.status()
	if status.Settled(testCandidateKey()) {
		t.Fatalf("status = %#v, want nothing settled by a transient command error", status)
	}
}
