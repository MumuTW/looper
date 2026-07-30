package plannerassessment

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// BuildScopeAssessmentPrompt renders the read-only assessment prompt. The agent
// is instructed to observe only: it reports blast radius, affected surfaces,
// ADR conflicts, and unauthorized decisions. It never decides whether to stop;
// that authority belongs to the deterministic policy. The prompt binds the
// assessment to the Issue content and base SHA so drift retires it.
func BuildScopeAssessmentPrompt(repo string, issueNumber int64, title, body, specPath, baseBranch, baseSHA string) string {
	var b strings.Builder
	b.WriteString("You are a pre-specification scope assessor for an autonomous coding system.\n\n")
	b.WriteString("Your ONLY job is to observe the repository and report structured facts about what the change would touch.\n")
	b.WriteString("You do NOT decide whether the change proceeds. A deterministic policy makes that decision from your report.\n")
	b.WriteString("You cannot modify the repository, push commits, open pull requests, or mutate any external state.\n\n")
	b.WriteString(fmt.Sprintf("Repository: %s\n", repo))
	b.WriteString(fmt.Sprintf("Base branch: %s\n", baseBranch))
	b.WriteString(fmt.Sprintf("Base SHA: %s\n", baseSHA))
	b.WriteString(fmt.Sprintf("Issue number: %d\n", issueNumber))
	b.WriteString(fmt.Sprintf("Issue title: %s\n", title))
	b.WriteString(fmt.Sprintf("Issue body:\n%s\n\n", body))
	b.WriteString(fmt.Sprintf("Spec path: %s\n\n", specPath))
	b.WriteString("Explore the repository to estimate the change's blast radius and surface.\n")
	b.WriteString("Look at: the issue scope, files that would be modified, packages affected, public APIs, ")
	b.WriteString("configuration schemas, CLI surfaces, storage schemas, wire formats, ")
	b.WriteString("and any docs/adr/ decisions that would be contradicted.\n\n")
	b.WriteString("Return ONE strict JSON object on stdout and NO other text.\n")
	b.WriteString("Every boolean MUST agree with its evidence field. Omit no fields.\n\n")
	b.WriteString(`{"estimatedFilesTouched":0,"estimatedPackagesTouched":0,"filesEvidence":["string"],"packagesEvidence":["string"],"publicSurfaceChange":false,"publicSurfaces":["public_api|config_schema|cli_surface|storage_schema|wire_format"],"publicSurfaceEvidence":"string","adrConflict":false,"conflictingAdr":"string","adrConflictEvidence":"string","unauthorizedDecision":false,"decisionRequired":"string","rationale":"string"}`)
	b.WriteString("\n\nWhere:\n")
	b.WriteString("- estimatedFilesTouched: integer count of files the change would touch (>= 0)\n")
	b.WriteString("- estimatedPackagesTouched: integer count of packages affected (>= 0)\n")
	b.WriteString("- filesEvidence: concrete file paths supporting the file estimate\n")
	b.WriteString("- packagesEvidence: package names supporting the package estimate\n")
	b.WriteString("- publicSurfaceChange: true ONLY if the change alters a public API, config schema, CLI surface, storage schema, or wire format\n")
	b.WriteString("- publicSurfaces: list of affected surfaces (must be non-empty when publicSurfaceChange=true, empty when false)\n")
	b.WriteString("- publicSurfaceEvidence: human-readable justification\n")
	b.WriteString("- adrConflict: true ONLY if the change contradicts or supersedes a recorded docs/adr/ decision\n")
	b.WriteString("- conflictingAdr: path to the ADR (must be non-empty when adrConflict=true, empty when false)\n")
	b.WriteString("- adrConflictEvidence: how the change conflicts\n")
	b.WriteString("- unauthorizedDecision: true ONLY if the Issue requires a naming, design, or boundary decision the agent is not authorized to make\n")
	b.WriteString("- decisionRequired: what decision is needed (must be non-empty when unauthorizedDecision=true, empty when false)\n")
	b.WriteString("- rationale: brief summary of observations\n")
	return b.String()
}

// ContentHash returns a stable hash of Issue content for drift detection.
// When the title or body changes, the hash changes, retiring any prior
// assessment and triggering reassessment.
func ContentHash(title, body string) string {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte("\x00"))
	h.Write([]byte(body))
	return fmt.Sprintf("%x", h.Sum(nil))
}
