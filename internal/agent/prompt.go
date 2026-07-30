package agent

import "strings"

const (
	CompletionMarker       = "__LOOPER_RESULT__"
	CompletionMarkerPrefix = CompletionMarker + "="
)

func AppendCompletionInstruction(prompt string) string {
	return AppendCompletionInstructionWithExample(prompt, `{"summary":"<one-sentence summary>"}`)
}

func AppendCompletionInstructionWithExample(prompt, example string) string {
	return strings.Join([]string{
		prompt,
		"When finished, print exactly one final line to stdout in this format:",
		CompletionMarkerPrefix + example,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	}, "\n\n")
}

// AppendFixerCompletionInstruction is the fixer-specific completion contract.
// The fixer repair pipeline requires a top-level `outcome` authority before it
// advances; the generic template's summary-only example would otherwise invite
// the agent to emit a syntactically valid marker that the fixer rejects for
// lacking the required structured outcome.
func AppendFixerCompletionInstruction(prompt string) string {
	return strings.Join([]string{
		prompt,
		"When finished, print exactly one final line to stdout in this format:",
		CompletionMarkerPrefix + `{"outcome":"completed","summary":"<one-sentence summary>"}`,
		"When the repair could not be attempted, use outcome \"blocked\" and include a failure_kind of \"retryable_transient\", \"retryable_after_resume\", or \"manual_intervention\":",
		CompletionMarkerPrefix + `{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	}, "\n\n")
}
