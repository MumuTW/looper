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

func TestCompletionMarkerPayloadsSelectsFramingMarkerNotMarkerInsideJSON(t *testing.T) {
	t.Parallel()

	// A legitimate JSON payload whose summary text mentions the protocol token.
	// The marker echoed inside the JSON string value is not followed by "{", so
	// the framing marker (the leftmost one) must be selected instead.
	raw := CompletionMarkerPrefix + `{"summary":"Fixed __LOOPER_RESULT__= parsing"}`

	payloads := CompletionMarkerPayloads(raw)
	want := []string{`{"summary":"Fixed __LOOPER_RESULT__= parsing"}`}
	if len(payloads) != len(want) {
		t.Fatalf("CompletionMarkerPayloads() = %#v, want %#v", payloads, want)
	}
	if payloads[0] != want[0] {
		t.Fatalf("CompletionMarkerPayloads()[0] = %q, want %q", payloads[0], want[0])
	}
}

func TestCompletionMarkerPayloadsSkipsProseDiagnosticNotFollowedByJSON(t *testing.T) {
	t.Parallel()

	// A stderr-style diagnostic mentioning the marker is not a framing marker
	// (not followed by "{") and must not shadow the real completion on stdout.
	raw := strings.Join([]string{
		CompletionMarkerPrefix + `{"summary":"done"}`,
		"warning: expected " + CompletionMarkerPrefix + " JSON",
	}, "\n")

	payloads := CompletionMarkerPayloads(raw)
	want := []string{`{"summary":"done"}`}
	if len(payloads) != len(want) {
		t.Fatalf("CompletionMarkerPayloads() = %#v, want %#v", payloads, want)
	}
	if payloads[0] != want[0] {
		t.Fatalf("CompletionMarkerPayloads()[0] = %q, want %q", payloads[0], want[0])
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
