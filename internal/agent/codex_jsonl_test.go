package agent

import (
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestCodexJSONLTranslator(t *testing.T) {
	tr := newCodexJSONLTranslator()
	lines := []string{
		`{"type":"thread.started","thread_id":"th_abc123"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"type":"command_execution","id":"c1","command":"cat README.md"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","id":"c1","command":"cat README.md","output":"# Repo","exit_code":0}}`,
		`{"type":"item.started","item":{"type":"command_execution","id":"c2","command":"npm test"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","id":"c2","output":"fail","exit_code":1}}`,
		`not json — should be ignored`,
		`{"type":"unknown.event"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Done: added LICENSE."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10}}`,
	}
	for _, l := range lines {
		tr.ingestLine(l)
	}

	if tr.threadID != "th_abc123" {
		t.Fatalf("threadID = %q", tr.threadID)
	}
	if !tr.terminal {
		t.Fatalf("expected terminal after turn.completed")
	}
	if tr.finalText != "Done: added LICENSE." {
		t.Fatalf("finalText = %q", tr.finalText)
	}
	if len(tr.tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %+v", len(tr.tools), tr.tools)
	}
	if tr.tools[0].Status != "done" || tr.tools[1].Status != "error" {
		t.Fatalf("statuses = %q, %q", tr.tools[0].Status, tr.tools[1].Status)
	}
	// c2's completion omitted the command; it should be preserved from item.started.
	if tr.tools[1].Command != "npm test" {
		t.Fatalf("c2 command not preserved: %q", tr.tools[1].Command)
	}

	lines2 := tr.recentToolLines(5)
	if len(lines2) != 2 || lines2[0] != "✅ cat README.md" || lines2[1] != "❌ npm test" {
		t.Fatalf("recentToolLines = %v", lines2)
	}
}

func TestCodexJSONLTranslatorPreservesCompletionPayload(t *testing.T) {
	t.Parallel()

	tr := newCodexJSONLTranslator()
	tr.ingestLine(`{"type":"item.completed","item":{"type":"agent_message","text":"__LOOPER_RESULT__={\"outcome\":\"completed\",\"summary\":\"done\"}"}}`)
	completion := parseCompletion(tr.combinedText(), "")
	if completion.ParseStatus != "parsed" || completion.CompletionPayload != `{"outcome":"completed","summary":"done"}` {
		t.Fatalf("completion = %#v, want translated structured payload", completion)
	}
}

func TestFinalMessageExtractsCodexJSONLAndPreservesPlainText(t *testing.T) {
	jsonl := "{\"type\":\"thread.started\",\"thread_id\":\"th_1\"}\n" +
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"action\\\":\\\"route\\\"}\"}}\n" +
		"{\"type\":\"turn.completed\"}"
	if got := FinalMessage(jsonl); got != `{"action":"route"}` {
		t.Fatalf("FinalMessage(JSONL) = %q", got)
	}
	if got := FinalMessage(`{"action":"route"}`); got != `{"action":"route"}` {
		t.Fatalf("FinalMessage(plain) = %q", got)
	}
}

func TestResolveCodexArgsJSONFlag(t *testing.T) {
	base := ExecutorConfig{Vendor: "codex"}
	// off by default → no --json
	got := resolveCodexArgs(base, []string{"-c", "model=gpt-5.4"}, "do it")
	if containsArg(got, "--json") {
		t.Fatalf("--json should be absent by default: %v", got)
	}
	// on → --json present, after exec, prompt still last
	on := base
	on.LiveToolEvents = true
	got = resolveCodexArgs(on, []string{"-c", "model=gpt-5.4"}, "do it")
	if got[0] != "exec" || !containsArg(got, "--json") || got[len(got)-1] != "do it" {
		t.Fatalf("expected exec … --json … prompt: %v", got)
	}
}

func TestResolveCodexArgs_ReasoningEffort(t *testing.T) {
	base := ExecutorConfig{Vendor: "codex"}
	effort := config.ReasoningEffortHigh
	base.ReasoningEffort = &effort
	got := resolveCodexArgs(base, []string{}, "do it")
	found := false
	for i, arg := range got {
		if arg == "-c" && i+1 < len(got) && got[i+1] == "model_reasoning_effort=high" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("resolveCodexArgs() = %v, want -c model_reasoning_effort=high", got)
	}
}

func TestResolveCodexArgs_ReasoningEffortNone(t *testing.T) {
	base := ExecutorConfig{Vendor: "codex"}
	effort := config.ReasoningEffortNone
	base.ReasoningEffort = &effort
	got := resolveCodexArgs(base, []string{}, "do it")
	for _, arg := range got {
		if strings.HasPrefix(arg, "model_reasoning_effort=") {
			t.Fatalf("resolveCodexArgs() = %v, should not forward the none sentinel", got)
		}
	}
}

func TestResolveCodexArgs_ReasoningEffortUnset(t *testing.T) {
	base := ExecutorConfig{Vendor: "codex"}
	got := resolveCodexArgs(base, []string{}, "do it")
	for _, arg := range got {
		if strings.HasPrefix(arg, "model_reasoning_effort=") {
			t.Fatalf("resolveCodexArgs() = %v, should not contain reasoning effort", got)
		}
	}
}

func TestResolveCodexArgs_ReasoningEffortWithStdinPrompt(t *testing.T) {
	effort := config.ReasoningEffortVeryHigh
	got := resolveCodexArgs(ExecutorConfig{Vendor: "codex", ReasoningEffort: &effort}, []string{"-"}, "ignored")
	if !containsArg(got, "model_reasoning_effort=xhigh") {
		t.Fatalf("resolveCodexArgs() = %v, want xhigh reasoning effort", got)
	}
	for i, arg := range got {
		if arg == "-" && i > 0 && got[i-1] == "model_reasoning_effort=xhigh" {
			return
		}
	}
	t.Fatalf("resolveCodexArgs() = %v, want reasoning effort before stdin marker", got)
}

func TestResolveCodexNativeResumeArgs_ReasoningEffort(t *testing.T) {
	effort := config.ReasoningEffortVeryHigh
	got := resolveCodexNativeResumeArgs(ExecutorConfig{Vendor: "codex", ReasoningEffort: &effort}, nil, "", "thread-1", "continue")
	if !containsArg(got, "model_reasoning_effort=xhigh") {
		t.Fatalf("resolveCodexNativeResumeArgs() = %v, want xhigh reasoning effort", got)
	}
}

func TestCleanShellWrapper(t *testing.T) {
	cases := map[string]string{
		"/bin/zsh -lc 'gh api repos/x'":   "gh api repos/x",
		`bash -c "npm test"`:              "npm test",
		"gh pr create --title x":          "gh pr create --title x",
		"/usr/bin/sh -lc 'cat README.md'": "cat README.md",
	}
	for in, want := range cases {
		if got := cleanShellWrapper(in); got != want {
			t.Fatalf("cleanShellWrapper(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestExtractCodexThreadID(t *testing.T) {
	blob := `{"type":"thread.started","thread_id":"019f2d12-279e-7a73"}
{"type":"item.started","item":{"type":"command_execution","id":"c1","command":"ls"}}`
	if got := extractCodexThreadID(blob); got != "019f2d12-279e-7a73" {
		t.Fatalf("extractCodexThreadID = %q; want the thread id", got)
	}
	if got := extractCodexThreadID(`{"type":"item.started"}`); got != "" {
		t.Fatalf("extractCodexThreadID(no thread.started) = %q; want empty", got)
	}
	if got := extractCodexThreadID("not json\n"); got != "" {
		t.Fatalf("extractCodexThreadID(garbage) = %q; want empty", got)
	}
}
