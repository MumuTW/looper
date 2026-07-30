package reviewsubmit

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Contract coverage for issue #557: review submit must keep valid inline comments
// when the full PR diff exceeds the generic shell capture limit by building
// path-targeted base/head authority from the prepared local checkout.
func TestReviewSubmitOrchestrationPreservesInlineCommentsWhenFullDiffExceedsCaptureLimit(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedReviewSubmitLargeRepo(t, repo)
	payloadPath, configPath, submitLog, ghPath := writeReviewSubmitHarness(t, repo, baseSHA, headSHA, "truncated")

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-large head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{
				"body":     "late change needs attention",
				"severity": "blocking",
				"path":     "target/late.go",
				"line":     targetLine,
				"side":     "RIGHT",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := os.WriteFile(payloadPath, raw, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	opts := reviewSubmitTestOptions(t, payloadPath, configPath, repo, stdout, stderr)
	opts.CommitID = headSHA

	if err := runTrustedForTest(context.Background(), opts); err != nil {
		t.Fatalf("Run() error = %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"submitted"`) || !strings.Contains(stdout.String(), "true") {
		t.Fatalf("stdout = %q, want submitted true", stdout.String())
	}

	submitted := readLastReviewSubmitPayload(t, submitLog)
	comments, _ := submitted["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("outgoing GitHub review comments = %#v, want original inline comment", submitted)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "target/late.go" || int64(comment["line"].(float64)) != targetLine || comment["side"] != "RIGHT" {
		t.Fatalf("comment = %#v, want resolvable target/late.go RIGHT %d", comment, targetLine)
	}
	if body, _ := comment["body"].(string); !strings.Contains(body, "<!-- looper:review-item severity=blocking -->") {
		t.Fatalf("comment body = %q, want persisted severity authority", body)
	}
	// A non-empty comments[] entry is the GitHub contract for a resolvable review thread.
	if submitted["commit_id"] != headSHA {
		t.Fatalf("commit_id = %#v, want %s", submitted["commit_id"], headSHA)
	}
	_ = ghPath
}

func TestReviewSubmitOrchestrationPreservesLeftDeletedInlineComment(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, deletedLine := seedReviewSubmitDeletedRepo(t, repo)
	payloadPath, configPath, submitLog, _ := writeReviewSubmitHarness(t, repo, baseSHA, headSHA, "truncated")

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-left head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{"body": "deleted line issue", "severity": "blocking", "path": "removed.go", "line": deletedLine, "side": "LEFT"},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := os.WriteFile(payloadPath, raw, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	opts := reviewSubmitTestOptions(t, payloadPath, configPath, repo, stdout, stderr)
	opts.CommitID = headSHA

	if err := runTrustedForTest(context.Background(), opts); err != nil {
		t.Fatalf("Run() error = %v\nstderr=%s", err, stderr.String())
	}
	submitted := readLastReviewSubmitPayload(t, submitLog)
	comments, _ := submitted["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want LEFT deleted-line comment", submitted)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "removed.go" || int64(comment["line"].(float64)) != deletedLine || comment["side"] != "LEFT" {
		t.Fatalf("comment = %#v, want removed.go LEFT %d", comment, deletedLine)
	}
}
