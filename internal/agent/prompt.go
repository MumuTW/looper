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
