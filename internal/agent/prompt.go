package agent

import "strings"

const (
	CompletionMarker       = "__LOOPER_RESULT__"
	CompletionMarkerPrefix = CompletionMarker + "="
)

// CompletionMarkerPayloads returns every completion-marker payload found in
// raw, ordered last-occurrence-first so callers can take the newest and skip
// echoed templates. The marker is matched anywhere in a line, not only at its
// start: agents routinely glue it onto the tail of a prose sentence
// ("...produce the result.__LOOPER_RESULT__={...}") instead of emitting it on
// its own line. Requiring a line start dropped those payloads entirely, which
// turned a successful agent run into a spurious retry.
func CompletionMarkerPayloads(raw string) []string {
	lines := strings.Split(raw, "\n")
	payloads := make([]string, 0, 2)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		idx := strings.LastIndex(line, CompletionMarkerPrefix)
		if idx < 0 {
			continue
		}
		payloads = append(payloads, strings.TrimSpace(line[idx+len(CompletionMarkerPrefix):]))
	}
	return payloads
}

func AppendCompletionInstruction(prompt string) string {
	return strings.Join([]string{
		prompt,
		"When finished, print exactly one final line to stdout in this format:",
		CompletionMarkerPrefix + `{"summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	}, "\n\n")
}

// AppendFixerCompletionInstruction is the fixer-specific completion contract.
//
// The fixer repair pipeline treats a top-level `outcome` as the authority for
// whether the repair completed or was blocked. The generic summary-only template
// would invite the agent to emit a syntactically valid marker carrying no
// outcome, which the fixer must then park for manual recovery rather than trust,
// so the fixer asks for the shape it actually requires.
func AppendFixerCompletionInstruction(prompt string) string {
	return strings.Join([]string{
		prompt,
		"When finished, print exactly one final line to stdout in this format:",
		CompletionMarkerPrefix + `{"outcome":"completed","summary":"<one-sentence summary>"}`,
		`When the repair could not be attempted, use outcome "blocked" and include a failure_kind of "retryable_transient", "retryable_after_resume", or "manual_intervention":`,
		CompletionMarkerPrefix + `{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	}, "\n\n")
}
