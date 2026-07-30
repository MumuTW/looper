package agent

import (
	"strings"
	"testing"
)

func TestCompletionMarkerPayloadsTakesLastOccurrenceFirst(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		CompletionMarkerPrefix + `{"summary":"first"}`,
		"prose before the marker." + CompletionMarkerPrefix + `{"summary":"glued"}`,
		"   ",
	}, "\n")

	payloads := CompletionMarkerPayloads(raw)
	want := []string{`{"summary":"glued"}`, `{"summary":"first"}`}
	if len(payloads) != len(want) {
		t.Fatalf("CompletionMarkerPayloads() = %#v, want %#v", payloads, want)
	}
	for i, payload := range payloads {
		if payload != want[i] {
			t.Fatalf("CompletionMarkerPayloads()[%d] = %q, want %q", i, payload, want[i])
		}
	}
}

func TestCompletionMarkerPayloadsReturnsEmptyWithoutMarker(t *testing.T) {
	t.Parallel()

	if payloads := CompletionMarkerPayloads("no marker here\njust prose\n"); len(payloads) != 0 {
		t.Fatalf("CompletionMarkerPayloads() = %#v, want empty", payloads)
	}
}

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
