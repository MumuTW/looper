package agent

import (
	"strings"
	"testing"
)

func TestAppendCompletionInstruction(t *testing.T) {
	t.Parallel()

	prompt := AppendCompletionInstruction("do the work")
	for _, needle := range []string{
		"do the work",
		"When finished, print exactly one final line to stdout in this format:",
		`__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("prompt = %q, want %q", prompt, needle)
		}
	}
}

func TestAppendCompletionInstructionWithExampleUsesCallerContract(t *testing.T) {
	t.Parallel()

	prompt := AppendCompletionInstructionWithExample("fix the PR", `{"outcome":"completed","summary":"<one-sentence summary>"}`)
	if !strings.Contains(prompt, `__LOOPER_RESULT__={"outcome":"completed","summary":"<one-sentence summary>"}`) {
		t.Fatalf("prompt = %q, want fixer completion contract", prompt)
	}
}

func TestAppendFixerCompletionInstructionIncludesOutcomeContract(t *testing.T) {
	t.Parallel()

	prompt := AppendFixerCompletionInstruction("repair the pr")
	for _, needle := range []string{
		"repair the pr",
		`__LOOPER_RESULT__={"outcome":"completed","summary":"<one-sentence summary>"}`,
		`__LOOPER_RESULT__={"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
	} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("fixer prompt = %q, want %q", prompt, needle)
		}
	}
}
