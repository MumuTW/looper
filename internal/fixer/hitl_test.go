package fixer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/storage"
)

func TestNormalizeReplyActionAcceptsNeedsHuman(t *testing.T) {
	if got := normalizeReplyAction("needs_human"); got != string(replyActionNeedsHuman) {
		t.Fatalf("normalizeReplyAction(needs_human) = %q", got)
	}
	if got := normalizeReplyAction("NEEDS_HUMAN"); got != string(replyActionNeedsHuman) {
		t.Fatalf("normalizeReplyAction case-insensitive = %q", got)
	}
	action, ok := parseReplyAction("needs_human")
	if !ok || action != string(replyActionNeedsHuman) {
		t.Fatalf("parseReplyAction(needs_human) = (%q, %v)", action, ok)
	}
	if got := canonicalizeReplyAction("needs_human"); got != string(replyActionNeedsHuman) {
		t.Fatalf("canonicalizeReplyAction(needs_human) = %q", got)
	}
	if got := normalizeNativeRepairAction("needs_human"); got != string(replyActionNeedsHuman) {
		t.Fatalf("normalizeNativeRepairAction(needs_human) = %q", got)
	}
	// deferred remains valid for Forgejo native path
	if got := normalizeNativeRepairAction("deferred"); got != "deferred" {
		t.Fatalf("normalizeNativeRepairAction(deferred) = %q", got)
	}
}

func TestFixerPromptAuthorityOrderAndNoBlindObey(t *testing.T) {
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	cfg.Instructions.Enabled = false
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Body: "please rename"}}
	detail := &checkpointDetail{HeadSHA: "abc", Title: "Keep dark mode", Body: "Ship dark mode toggle"}

	// HITL off: no needs_human vocabulary; may still authorize push when allowAutoPush.
	off, _ := buildFixerPrompt("project_1", cfg, "acme/looper", 42, detail, items, true, false, config.DefaultDisclosureConfig(), "codex", "gpt")
	if strings.Contains(off, "when in doubt, implement the requested change") {
		t.Fatal("prompt must not contain blind-obey guidance")
	}
	if !strings.Contains(off, "Authority order") {
		t.Fatal("prompt must contain authority order")
	}
	if !strings.Contains(off, "documented project rules") {
		t.Fatal("prompt must name documented project rules, not vague stable norms")
	}
	if strings.Contains(off, "AGENTS.md / stable norms") {
		t.Fatal("authority tier must not use vague stable norms")
	}
	if strings.Contains(off, "needs_human") {
		t.Fatal("HITL-off prompt must not advertise needs_human")
	}
	if !strings.Contains(off, "Commit and push") {
		t.Fatal("HITL-off with allowAutoPush should still authorize agent push")
	}

	// HITL on: needs_human allowed; agent push forced off by caller (allowAutoPush=false).
	on, _ := buildFixerPrompt("project_1", cfg, "acme/looper", 42, detail, items, false, true, config.DefaultDisclosureConfig(), "codex", "gpt")
	if !strings.Contains(on, "needs_human") {
		t.Fatal("HITL-on prompt must allow needs_human")
	}
	if strings.Contains(on, "Commit and push") {
		t.Fatal("HITL-on repair prompt must not authorize agent push")
	}
}

func TestFixerHITLPromptInstructionAppendedWhenEnabled(t *testing.T) {
	if !strings.Contains(hitlPromptInstruction, "AUTHORITY ORDER") {
		t.Fatal("hitl instruction missing authority order")
	}
	if !strings.Contains(hitlPromptInstruction, hitl.AskSentinelRelPath) {
		t.Fatal("hitl instruction missing ask.json path")
	}
	if !strings.Contains(hitlPromptInstruction, "needs_human") {
		t.Fatal("hitl instruction missing needs_human")
	}
	if strings.Contains(hitlPromptInstruction, "AGENTS.md / stable norms") {
		t.Fatal("hitl instruction authority tier must not use vague stable norms")
	}
	if strings.Contains(hitlPromptInstruction, "when in doubt, implement") {
		t.Fatal("hitl instruction must not reintroduce blind obey")
	}
}

func TestReadAskSentinelFailClosedAndDeferredDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	path := filepath.Join(dir, hitl.AskSentinelRelPath)
	if err := os.WriteFile(path, []byte(`{"question":"Keep PR intent or follow reviewer?","options":["keep PR intent","follow reviewer"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	ask, err := hitl.ReadAskSentinel(dir)
	if err != nil {
		t.Fatalf("ReadAskSentinel error = %v", err)
	}
	if ask == nil || ask.Question == "" || len(ask.Options) != 2 {
		t.Fatalf("ask = %#v", ask)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("ReadAskSentinel must not delete the file")
	}
	hitl.RemoveAskSentinel(dir)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("RemoveAskSentinel must delete the file")
	}

	// Malformed → error (fail closed)
	if err := os.WriteFile(path, []byte(`{not-json`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if _, err := hitl.ReadAskSentinel(dir); err == nil {
		t.Fatal("malformed ask must error")
	}
	// Missing options → error
	if err := os.WriteFile(path, []byte(`{"question":"only q"}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if _, err := hitl.ReadAskSentinel(dir); err == nil {
		t.Fatal("ask without options must error")
	}
}

func TestDetectHumanAskFromNeedsHumanReply(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	runner := New(Options{
		Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, HITLEnabled: true,
	})
	input := stepInput{
		Project:  storage.ProjectRecord{ID: "project_1"},
		Loop:     storage.LoopRecord{ID: "loop_x", Seq: 1},
		Repo:     "acme/looper",
		PRNumber: 9,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "h1"},
			FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Body: "remove feature"}},
		},
	}
	replies := []replyExplanationEntry{{
		FixItemID: "c1", ThreadID: "t1", Action: string(replyActionNeedsHuman),
		Explanation: "Reviewer wants to remove dark mode but PR intent keeps it.",
	}}
	awaiting, err := runner.detectHumanAsk(ctx, input, t.TempDir(), "exec-1", replies)
	if err != nil {
		t.Fatalf("detectHumanAsk error = %v", err)
	}
	if awaiting == nil {
		t.Fatal("expected awaitingHumanError from needs_human reply")
	}
	if !strings.Contains(awaiting.question, "acme/looper#9") && !strings.Contains(awaiting.recommendation, "dark mode") {
		t.Fatalf("brief = %#v", awaiting)
	}
	if awaiting.headSHA != "h1" || awaiting.reviewThreadID != "t1" {
		t.Fatalf("fingerprints = head=%q thread=%q", awaiting.headSHA, awaiting.reviewThreadID)
	}
}

func TestDetectHumanAskDisabledWhenHITLOff(t *testing.T) {
	runner := New(Options{HITLEnabled: false})
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".looper"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, hitl.AskSentinelRelPath), []byte(`{"question":"q","options":["a"]}`), 0o644)
	awaiting, err := runner.detectHumanAsk(context.Background(), stepInput{}, dir, "e", []replyExplanationEntry{
		{Action: string(replyActionNeedsHuman), Explanation: "x"},
	})
	if err != nil || awaiting != nil {
		t.Fatalf("HITL off must ignore ask; got (%#v, %v)", awaiting, err)
	}
}
