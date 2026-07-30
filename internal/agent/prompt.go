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
//
// Only the framing marker is selected: the leftmost __LOOPER_RESULT__= on a
// line that is immediately followed by "{", since the payload is always a JSON
// object. A marker echoed inside its own JSON string value
// (`__LOOPER_RESULT__={"summary":"Fixed __LOOPER_RESULT__= parsing"}`) or inside
// a prose diagnostic ("warning: expected __LOOPER_RESULT__= JSON") is not
// followed by "{" and is skipped, so it can no longer shadow or invalidate the
// real completion. Scanning left-to-right within a line picks the framing
// marker rather than a marker nested inside the payload it introduces.
func CompletionMarkerPayloads(raw string) []string {
	lines := strings.Split(raw, "\n")
	payloads := make([]string, 0, 2)
	for i := len(lines) - 1; i >= 0; i-- {
		payload := completionMarkerPayloadOnLine(strings.TrimSpace(lines[i]))
		if payload == "" {
			continue
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// completionMarkerPayloadOnLine returns the JSON payload introduced by the
// framing completion marker on line — the leftmost __LOOPER_RESULT__=
// immediately followed by "{" — or "" when no framing marker is present.
func completionMarkerPayloadOnLine(line string) string {
	from := 0
	for {
		idx := strings.Index(line[from:], CompletionMarkerPrefix)
		if idx < 0 {
			return ""
		}
		idx += from
		payloadStart := idx + len(CompletionMarkerPrefix)
		if payloadStart < len(line) && line[payloadStart] == '{' {
			return strings.TrimSpace(line[payloadStart:])
		}
		from = idx + len(CompletionMarkerPrefix)
	}
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
