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

// AppendWorkerCompletionInstruction is the Worker-specific completion contract.
// A Worker may author a reproduction after its first capture, so the single
// template carries an explicit nullable reproduction slot rather than
// contradicting a separate prose requirement with the generic summary-only
// template.
func AppendWorkerCompletionInstruction(prompt string, reproductionOptional bool) string {
	if !reproductionOptional {
		return AppendCompletionInstruction(prompt)
	}
	return strings.Join([]string{
		prompt,
		"When finished, print exactly one final line to stdout in this format:",
		CompletionMarkerPrefix + `{"summary":"<one-sentence summary>","reproduction":null}`,
		`If you created .looper/reproducer.json because no reproduction contract was present when this run started, replace null with a "reproduction" object that exactly matches the committed manifest, including testPath, testName, testCommand, testSha256, and any issue scope. Leave it null when you did not create a new reproduction contract.`,
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
		`Use outcome "completed" only when every fix item is either fully addressed or validly declined. If the repair was attempted but could not be completed — an item is still unresolved, or required validation cannot pass — use outcome "blocked". Do not report "completed" for partial work.`,
		`A blocked outcome must include a failure_kind saying whether a retry could succeed at all:`,
		`- "retryable_transient": another attempt at the repair could succeed. Use this for anything a retry might clear, including an obstacle expected to pass on its own (a rate limit, a network or upstream error, a lock held elsewhere) and a working state that is wrong but recoverable (a stale checkout, a moved remote head, an installable missing dependency). Looper retries the repair; it does not re-run environment setup first, so say what you observed in the summary.`,
		`- "manual_intervention": no retry can succeed without a human decision, such as ambiguous or contradictory review feedback, a change that needs a product call, or credentials and permissions you do not have. This stops the loop, so do not use it for anything a retry could clear.`,
		CompletionMarkerPrefix + `{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	}, "\n\n")
}
