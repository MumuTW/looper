package reproduction

import (
	"context"
	"strings"
	"testing"
)

func TestDecodeDraftRejectsIncompleteOrUnknownShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing command": `{"version":1,"files":["a_test.go"]}`,
		"missing files":   `{"version":1,"command":"go test ./..."}`,
		"empty files":     `{"version":1,"command":"go test ./...","files":["  "]}`,
		"wrong version":   `{"version":2,"command":"go test ./...","files":["a_test.go"]}`,
		"unknown field":   `{"version":1,"command":"go test ./...","files":["a_test.go"],"confidence":0.9}`,
	}
	for name, payload := range cases {
		if _, err := DecodeDraft([]byte(payload)); err == nil {
			t.Fatalf("DecodeDraft(%s) error = nil, want rejection", name)
		}
	}
	draft, err := DecodeDraft([]byte(`{"version":1,"command":"go test ./pkg -run TestBug","files":["pkg/bug_test.go","pkg/bug_test.go"],"observedFailure":"want 1 got 0"}`))
	if err != nil {
		t.Fatalf("DecodeDraft() error = %v", err)
	}
	if len(draft.Files) != 1 || draft.Files[0] != "pkg/bug_test.go" {
		t.Fatalf("DecodeDraft() files = %#v, want deduplicated worktree-relative paths", draft.Files)
	}
}

// An agent-authored path is untrusted input: it names a file the daemon will
// read and hash, so escaping the worktree must be refused rather than resolved.
func TestHashFilesRejectsPathsOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "reproduction")
	for _, path := range []string{"../escape_test.go", "/etc/passwd"} {
		if _, err := HashFiles(root, []string{path}); err == nil {
			t.Fatalf("HashFiles(%q) error = nil, want rejection", path)
		}
	}
}

func TestWriteAndReadManifestRoundTrips(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "reproduction")
	hashes, err := HashFiles(root, []string{"pkg/bug_test.go"})
	if err != nil {
		t.Fatalf("HashFiles() error = %v", err)
	}
	written := Manifest{
		Version: ManifestVersion, Repo: "acme/looper", IssueNumber: 7,
		Command: "go test ./pkg -run TestBug", Files: hashes, IdempotencyKey: "reproduction:abc",
	}
	if err := WriteManifest(root, written); err != nil {
		t.Fatalf("WriteManifest() error = %v", err)
	}
	read, present, err := ReadManifest(root)
	if err != nil || !present {
		t.Fatalf("ReadManifest() = %v, %v, %v", read, present, err)
	}
	if read.Command != written.Command || read.IdempotencyKey != written.IdempotencyKey || len(read.Files) != 1 {
		t.Fatalf("ReadManifest() = %#v, want the written manifest", read)
	}
}

// A branch with no reproduction must leave every downstream gate inert, which
// is what keeps non-bug work on exactly today's path.
func TestReadManifestReportsAbsenceWithoutError(t *testing.T) {
	t.Parallel()
	_, present, err := ReadManifest(t.TempDir())
	if err != nil || present {
		t.Fatalf("ReadManifest() = %v, %v, want absent and no error", present, err)
	}
}

func TestPromptBlockNamesTheCommandAndFiles(t *testing.T) {
	t.Parallel()
	block := PromptBlock(Manifest{
		Version: ManifestVersion, Command: "go test ./pkg -run TestBug",
		Files: []FileHash{{Path: "pkg/bug_test.go", SHA256: "abc"}}, ObservedFailure: "want 1 got 0",
	})
	for _, want := range []string{"go test ./pkg -run TestBug", "pkg/bug_test.go", "want 1 got 0", ManifestRelPath} {
		if !strings.Contains(block, want) {
			t.Fatalf("PromptBlock() = %q, want it to contain %q", block, want)
		}
	}
	if PromptBlock(Manifest{}) != "" {
		t.Fatalf("PromptBlock() with no command = %q, want empty", PromptBlock(Manifest{}))
	}
}

func TestGateForLoopIsInertWithoutRepositories(t *testing.T) {
	t.Parallel()
	_, applies, err := GateForLoop(context.Background(), LoopGateInput{WorktreePath: t.TempDir()})
	if err != nil || applies {
		t.Fatalf("GateForLoop() = %v, %v, want inert", applies, err)
	}
}

func TestGovernedIssueNumberReadsJSONAndNativeNumbers(t *testing.T) {
	t.Parallel()
	if got := GovernedIssueNumber(map[string]any{LoopMetadataIssueKey: float64(12)}); got != 12 {
		t.Fatalf("GovernedIssueNumber(json) = %d, want 12", got)
	}
	if got := GovernedIssueNumber(map[string]any{LoopMetadataIssueKey: int64(12)}); got != 12 {
		t.Fatalf("GovernedIssueNumber(int64) = %d, want 12", got)
	}
	if got := GovernedIssueNumber(nil); got != 0 {
		t.Fatalf("GovernedIssueNumber(nil) = %d, want 0", got)
	}
}
