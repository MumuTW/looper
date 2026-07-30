package planner

import (
	"strings"
	"testing"
)

// completeAssessmentJSON supplies every schema key with agreeing values. Tests
// mutate one key at a time so each rejection is attributable.
const completeAssessmentJSON = `{"estimatedFilesTouched":3,"estimatedPackagesTouched":1,"filesEvidence":["a.go"],"packagesEvidence":["internal/a"],"publicSurfaceChange":false,"publicSurfaces":[],"publicSurfaceEvidence":"","adrConflict":false,"conflictingAdr":"","adrConflictEvidence":"","unauthorizedDecision":false,"decisionRequired":"","rationale":"read three files"}`

func TestDecodeScopeAssessmentRequiresEverySchemaKey(t *testing.T) {
	t.Parallel()

	if _, err := decodeScopeAssessment(completeAssessmentJSON); err != nil {
		t.Fatalf("decodeScopeAssessment(complete) error = %v", err)
	}

	// The exact payload from the review: syntactically valid, semantically empty.
	// Absent keys must not decode to "zero blast radius, nothing fires".
	partial := `{"rationale":"read the repository"}`
	assessment, err := decodeScopeAssessment(partial)
	if err == nil {
		t.Fatalf("decodeScopeAssessment(partial) = %#v, want rejection", assessment)
	}
	for _, key := range []string{"estimatedFilesTouched", "publicSurfaceChange", "adrConflict", "unauthorizedDecision"} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error does not name the missing key %s: %v", key, err)
		}
	}

	for _, key := range scopeAssessmentSchemaKeys {
		raw := removeJSONKey(t, completeAssessmentJSON, key)
		if _, err := decodeScopeAssessment(raw); err == nil {
			t.Fatalf("decodeScopeAssessment without %q = accepted, want rejection", key)
		}
	}
}

func TestDecodeScopeAssessmentRejectsContradictoryFlags(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]string{
		"public surface evidence without the flag": {
			"publicSurfaces": `["public_api"]`,
		},
		"public surface prose without the flag": {
			"publicSurfaceEvidence": `"changes the wire format"`,
		},
		"public surface flag without evidence": {
			"publicSurfaceChange": `true`,
		},
		"adr path without the flag": {
			"conflictingAdr": `"docs/adr/0001-x.md"`,
		},
		"adr prose without the flag": {
			"adrConflictEvidence": `"supersedes the decision"`,
		},
		"adr flag without a path": {
			"adrConflict": `true`,
		},
		"decision text without the flag": {
			"decisionRequired": `"name a new Authority"`,
		},
		"decision flag without text": {
			"unauthorizedDecision": `true`,
		},
	}

	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := completeAssessmentJSON
			for key, value := range overrides {
				raw = replaceJSONKey(t, raw, key, value)
			}
			if assessment, err := decodeScopeAssessment(raw); err == nil {
				t.Fatalf("decodeScopeAssessment(%s) = %#v, want rejection", name, assessment)
			}
		})
	}
}

func TestDecodeScopeAssessmentAcceptsAgreeingFlags(t *testing.T) {
	t.Parallel()

	raw := replaceJSONKey(t, completeAssessmentJSON, "publicSurfaceChange", `true`)
	raw = replaceJSONKey(t, raw, "publicSurfaces", `["public_api","cli_surface"]`)
	raw = replaceJSONKey(t, raw, "publicSurfaceEvidence", `"adds a flag"`)
	raw = replaceJSONKey(t, raw, "adrConflict", `true`)
	raw = replaceJSONKey(t, raw, "conflictingAdr", `"docs/adr/0001-x.md"`)
	raw = replaceJSONKey(t, raw, "adrConflictEvidence", `"supersedes it"`)
	raw = replaceJSONKey(t, raw, "unauthorizedDecision", `true`)
	raw = replaceJSONKey(t, raw, "decisionRequired", `"name a new Authority"`)

	assessment, err := decodeScopeAssessment(raw)
	if err != nil {
		t.Fatalf("decodeScopeAssessment(agreeing) error = %v", err)
	}
	if !assessment.PublicSurfaceChange || len(assessment.PublicSurfaces) != 2 || !assessment.ADRConflict || !assessment.UnauthorizedDecision {
		t.Fatalf("assessment = %#v", assessment)
	}

	// Unknown surfaces are still rejected once every key is supplied.
	unknown := replaceJSONKey(t, raw, "publicSurfaces", `["everything"]`)
	if _, err := decodeScopeAssessment(unknown); err == nil {
		t.Fatal("decodeScopeAssessment(unknown surface) = accepted, want rejection")
	}
}

// TestIncompleteAssessmentCannotBypassAnEnabledGate is the end of the chain the
// review named: a partial assessment must never reach evaluateEscalationPolicy,
// because there it would read as "no criterion fired".
func TestIncompleteAssessmentCannotBypassAnEnabledGate(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"rationale":"read the repository"}`,
		`{"estimatedFilesTouched":400,"rationale":"huge"}`,
		removeJSONKey(t, completeAssessmentJSON, "publicSurfaceChange"),
	} {
		assessment, err := decodeScopeAssessment(raw)
		if err == nil {
			t.Fatalf("decodeScopeAssessment(%s) accepted; policy would then see %v fired criteria", raw, len(evaluateEscalationPolicy(fullEscalationPolicy(), assessment)))
		}
	}
}

// removeJSONKey deletes one top-level key from a flat JSON object literal.
func removeJSONKey(t *testing.T, raw, key string) string {
	t.Helper()
	start := strings.Index(raw, `"`+key+`":`)
	if start < 0 {
		t.Fatalf("key %q not present in %s", key, raw)
	}
	end := valueEnd(raw, start+len(key)+3)
	if end < len(raw) && raw[end] == ',' {
		end++
	} else if start > 0 && raw[start-1] == ',' {
		start--
	}
	return raw[:start] + raw[end:]
}

// replaceJSONKey swaps one top-level key's value in a flat JSON object literal.
func replaceJSONKey(t *testing.T, raw, key, value string) string {
	t.Helper()
	start := strings.Index(raw, `"`+key+`":`)
	if start < 0 {
		t.Fatalf("key %q not present in %s", key, raw)
	}
	valueStart := start + len(key) + 3
	return raw[:valueStart] + value + raw[valueEnd(raw, valueStart):]
}

// valueEnd returns the index just past the JSON value starting at index i.
func valueEnd(raw string, i int) int {
	depth, inString, escaped := 0, false, false
	for ; i < len(raw); i++ {
		switch {
		case escaped:
			escaped = false
		case raw[i] == '\\' && inString:
			escaped = true
		case raw[i] == '"':
			inString = !inString
		case inString:
		case raw[i] == '[':
			depth++
		case raw[i] == ']':
			depth--
		case depth == 0 && (raw[i] == ',' || raw[i] == '}'):
			return i
		}
	}
	return i
}
