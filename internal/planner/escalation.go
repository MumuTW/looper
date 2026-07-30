package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// EscalationEventType is the durable record of a Planner exit to human review.
// It mirrors triager's triage.report: the event log is the persistence, so this
// adds no table and no migration.
const EscalationEventType = "planner.escalation"

// EscalationResolutionEventType records the human's answer and how it was
// classified, so the audit trail covers both halves of the gate.
const EscalationResolutionEventType = "planner.escalation.resolved"

// escalationRecordVersion is stamped on every persisted record so a later
// schema change is readable rather than ambiguous.
const escalationRecordVersion = 1

// Public surface kinds the scope assessment may report. A value outside this
// set is rejected: an unrecognised surface would silently widen or narrow the
// criterion depending on how the policy happened to compare it.
const (
	SurfacePublicAPI     = "public_api"
	SurfaceConfigSchema  = "config_schema"
	SurfaceCLISurface    = "cli_surface"
	SurfaceStorageSchema = "storage_schema"
	SurfaceWireFormat    = "wire_format"
)

// Criterion identifiers persisted on the escalation record.
const (
	CriterionBlastRadiusFiles     = "blast_radius_files"
	CriterionBlastRadiusPackages  = "blast_radius_packages"
	CriterionPublicSurfaceChange  = "public_surface_change"
	CriterionADRConflict          = "adr_conflict"
	CriterionUnauthorizedDecision = "unauthorized_decision"
)

// ScopeAssessment is the Planner agent's structured, pre-spec report of what
// implementing the Issue would actually cost, produced after it has explored
// the repository. It is deliberately factual: the agent does not decide whether
// to escalate, it reports observations the daemon's configured thresholds judge.
//
// The shape and the strict decoding match triager.Decision on purpose — one
// idiom for "agent output that a deterministic policy consumes".
type ScopeAssessment struct {
	EstimatedFilesTouched    int      `json:"estimatedFilesTouched"`
	EstimatedPackagesTouched int      `json:"estimatedPackagesTouched"`
	FilesEvidence            []string `json:"filesEvidence"`
	PackagesEvidence         []string `json:"packagesEvidence"`
	PublicSurfaceChange      bool     `json:"publicSurfaceChange"`
	PublicSurfaces           []string `json:"publicSurfaces"`
	PublicSurfaceEvidence    string   `json:"publicSurfaceEvidence"`
	ADRConflict              bool     `json:"adrConflict"`
	ConflictingADR           string   `json:"conflictingAdr"`
	ADRConflictEvidence      string   `json:"adrConflictEvidence"`
	UnauthorizedDecision     bool     `json:"unauthorizedDecision"`
	DecisionRequired         string   `json:"decisionRequired"`
	Rationale                string   `json:"rationale"`
}

// EscalationPolicy is the deterministic, configurable side of the gate. It is
// resolved per project from roles.planner.escalation.
type EscalationPolicy struct {
	Enabled                bool `json:"enabled"`
	MaxFilesTouched        int  `json:"maxFilesTouched"`
	MaxPackagesTouched     int  `json:"maxPackagesTouched"`
	OnPublicSurfaceChange  bool `json:"onPublicSurfaceChange"`
	OnADRConflict          bool `json:"onAdrConflict"`
	OnUnauthorizedDecision bool `json:"onUnauthorizedDecision"`
}

func escalationPolicyFromConfig(cfg config.PlannerEscalationConfig) EscalationPolicy {
	return EscalationPolicy{
		Enabled:                cfg.Enabled,
		MaxFilesTouched:        cfg.MaxFilesTouched,
		MaxPackagesTouched:     cfg.MaxPackagesTouched,
		OnPublicSurfaceChange:  cfg.OnPublicSurfaceChange,
		OnADRConflict:          cfg.OnADRConflict,
		OnUnauthorizedDecision: cfg.OnUnauthorizedDecision,
	}
}

// FiredCriterion is one configured criterion the assessment tripped, with the
// threshold it was measured against and the evidence the agent supplied.
type FiredCriterion struct {
	Criterion string   `json:"criterion"`
	Threshold string   `json:"threshold,omitempty"`
	Observed  string   `json:"observed,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
}

// EscalationRecord is the persisted structured record of one escalation: which
// criteria fired, the supporting evidence, and the decision requested from the
// human.
type EscalationRecord struct {
	Version           int              `json:"version"`
	ProjectID         string           `json:"projectId"`
	Repo              string           `json:"repo"`
	IssueNumber       int64            `json:"issueNumber"`
	LoopID            string           `json:"loopId"`
	RunID             string           `json:"runId"`
	Policy            EscalationPolicy `json:"policy"`
	Assessment        ScopeAssessment  `json:"assessment"`
	Criteria          []FiredCriterion `json:"criteria"`
	DecisionRequested string           `json:"decisionRequested"`
	Options           []string         `json:"options"`
	CreatedAt         string           `json:"createdAt"`
}

// Options offered to the human. The first authorizes spec authoring, the second
// settles the Issue without one.
const (
	EscalationOptionProceed = "proceed: authorize Planner to write the spec"
	EscalationOptionStop    = "stop: settle this Issue without a spec"
)

// decodeScopeAssessment parses the agent's strict-JSON assessment. Unknown
// fields and trailing content are rejected so a drifting prompt fails loudly
// rather than silently assessing zero blast radius.
func decodeScopeAssessment(raw string) (ScopeAssessment, error) {
	var assessment ScopeAssessment
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&assessment); err != nil {
		return ScopeAssessment{}, fmt.Errorf("decode planner scope assessment: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ScopeAssessment{}, fmt.Errorf("decode planner scope assessment: trailing content")
	}
	assessment.FilesEvidence = compactStrings(assessment.FilesEvidence)
	assessment.PackagesEvidence = compactStrings(assessment.PackagesEvidence)
	assessment.PublicSurfaces = compactStrings(assessment.PublicSurfaces)
	assessment.PublicSurfaceEvidence = strings.TrimSpace(assessment.PublicSurfaceEvidence)
	assessment.ConflictingADR = strings.TrimSpace(assessment.ConflictingADR)
	assessment.ADRConflictEvidence = strings.TrimSpace(assessment.ADRConflictEvidence)
	assessment.DecisionRequired = strings.TrimSpace(assessment.DecisionRequired)
	assessment.Rationale = strings.TrimSpace(assessment.Rationale)
	if err := validateScopeAssessment(assessment); err != nil {
		return ScopeAssessment{}, err
	}
	return assessment, nil
}

func validateScopeAssessment(assessment ScopeAssessment) error {
	if assessment.EstimatedFilesTouched < 0 || assessment.EstimatedPackagesTouched < 0 {
		return fmt.Errorf("decode planner scope assessment: blast radius estimates must not be negative")
	}
	if assessment.Rationale == "" {
		return fmt.Errorf("decode planner scope assessment: rationale is required")
	}
	for _, surface := range assessment.PublicSurfaces {
		switch surface {
		case SurfacePublicAPI, SurfaceConfigSchema, SurfaceCLISurface, SurfaceStorageSchema, SurfaceWireFormat:
		default:
			return fmt.Errorf("decode planner scope assessment: unsupported public surface %q", surface)
		}
	}
	return nil
}

// evaluateEscalationPolicy is the authority boundary: the model reported facts,
// this decides. It returns every criterion that fired, in a stable order, so the
// persisted record and the human-facing brief list all of them rather than the
// first one hit.
func evaluateEscalationPolicy(policy EscalationPolicy, assessment ScopeAssessment) []FiredCriterion {
	if !policy.Enabled {
		return nil
	}
	fired := make([]FiredCriterion, 0, 5)
	if policy.MaxFilesTouched > 0 && assessment.EstimatedFilesTouched > policy.MaxFilesTouched {
		fired = append(fired, FiredCriterion{
			Criterion: CriterionBlastRadiusFiles,
			Threshold: fmt.Sprintf("maxFilesTouched=%d", policy.MaxFilesTouched),
			Observed:  fmt.Sprintf("%d files", assessment.EstimatedFilesTouched),
			Evidence:  cloneStrings(assessment.FilesEvidence),
		})
	}
	if policy.MaxPackagesTouched > 0 && assessment.EstimatedPackagesTouched > policy.MaxPackagesTouched {
		fired = append(fired, FiredCriterion{
			Criterion: CriterionBlastRadiusPackages,
			Threshold: fmt.Sprintf("maxPackagesTouched=%d", policy.MaxPackagesTouched),
			Observed:  fmt.Sprintf("%d packages", assessment.EstimatedPackagesTouched),
			Evidence:  cloneStrings(assessment.PackagesEvidence),
		})
	}
	if policy.OnPublicSurfaceChange && assessment.PublicSurfaceChange {
		surfaces := cloneStrings(assessment.PublicSurfaces)
		sort.Strings(surfaces)
		fired = append(fired, FiredCriterion{
			Criterion: CriterionPublicSurfaceChange,
			Threshold: "onPublicSurfaceChange=true",
			Observed:  strings.Join(surfaces, ", "),
			Evidence:  compactStrings([]string{assessment.PublicSurfaceEvidence}),
		})
	}
	if policy.OnADRConflict && assessment.ADRConflict {
		fired = append(fired, FiredCriterion{
			Criterion: CriterionADRConflict,
			Threshold: "onAdrConflict=true",
			Observed:  assessment.ConflictingADR,
			Evidence:  compactStrings([]string{assessment.ADRConflictEvidence}),
		})
	}
	if policy.OnUnauthorizedDecision && assessment.UnauthorizedDecision {
		fired = append(fired, FiredCriterion{
			Criterion: CriterionUnauthorizedDecision,
			Threshold: "onUnauthorizedDecision=true",
			Observed:  assessment.DecisionRequired,
			Evidence:  compactStrings([]string{assessment.DecisionRequired}),
		})
	}
	if len(fired) == 0 {
		return nil
	}
	return fired
}

// buildEscalationQuestion states the specific decision the human is being asked
// to make, naming every criterion that fired.
func buildEscalationQuestion(repo string, issueNumber int64, criteria []FiredCriterion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Authorize Planner to write a spec for %s#%d? Repository exploration tripped %d escalation criteri%s before spec authoring:", repo, issueNumber, len(criteria), pluralSuffix(len(criteria)))
	for _, criterion := range criteria {
		fmt.Fprintf(&b, "\n- %s (%s)", criterion.Criterion, criterion.Threshold)
		if strings.TrimSpace(criterion.Observed) != "" {
			fmt.Fprintf(&b, ": %s", criterion.Observed)
		}
	}
	return b.String()
}

func pluralSuffix(count int) string {
	if count == 1 {
		return "on"
	}
	return "a"
}

// escalationConsequences spells out what each option does, so the ask card can
// be answered without reading this code.
func escalationConsequences() map[string]string {
	return map[string]string{
		EscalationOptionProceed: "Planner resumes in the same loop and authors the spec; your answer text is carried into the spec prompt as guidance.",
		EscalationOptionStop:    "Planner settles the Issue without a spec. The loop completes and discovery does not re-plan it.",
	}
}

// escalationRejectionTokens are the answers that settle the Issue. Anything else
// — including free-form guidance — authorizes Planner to proceed, because a
// human who types instructions is directing the work, not stopping it.
var escalationRejectionTokens = map[string]struct{}{
	"stop": {}, "reject": {}, "no": {}, "close": {}, "decline": {},
	"abort": {}, "cancel": {}, "deny": {}, "drop": {},
}

// classifyEscalationAnswer maps a human answer onto the two outcomes. It is
// deterministic and token-based rather than a second model call.
func classifyEscalationAnswer(answer string) bool {
	normalized := strings.ToLower(strings.TrimSpace(answer))
	if normalized == "" {
		return false
	}
	normalized = strings.TrimLeft(normalized, "-*> \t")
	first := normalized
	if index := strings.IndexAny(first, " \t:,.;!\n"); index >= 0 {
		first = first[:index]
	}
	_, rejected := escalationRejectionTokens[first]
	return !rejected
}

// buildScopeAssessmentPrompt asks for facts, not a verdict. The escalation
// decision is not delegated to the agent — see evaluateEscalationPolicy.
func buildScopeAssessmentPrompt(repo string, issue *checkpointIssue) string {
	return fmt.Sprintf(`You are Looper's Planner performing a PRE-SPEC scope assessment. Explore the repository in your working directory to answer. Do NOT write a spec, do NOT modify or create any file, do NOT commit, do NOT push.

Return one strict JSON object and no prose.
Schema:
{"estimatedFilesTouched":0,"estimatedPackagesTouched":0,"filesEvidence":["string"],"packagesEvidence":["string"],"publicSurfaceChange":false,"publicSurfaces":["public_api|config_schema|cli_surface|storage_schema|wire_format"],"publicSurfaceEvidence":"string","adrConflict":false,"conflictingAdr":"string","adrConflictEvidence":"string","unauthorizedDecision":false,"decisionRequired":"string","rationale":"string"}

Rules:
- estimatedFilesTouched / estimatedPackagesTouched are your best estimate of the implementation's blast radius, based on what you actually read. filesEvidence / packagesEvidence list the concrete paths behind those numbers.
- publicSurfaceChange is true only when implementing this Issue alters a public API, a config schema, a CLI surface, a storage schema, or a wire format. publicSurfaces names which; leave it empty when false.
- adrConflict is true only when the change contradicts or supersedes a decision recorded under docs/adr/. conflictingAdr must name that file path.
- unauthorizedDecision is true only when the Issue's stated goal cannot be satisfied without a decision Planner is not authorized to make — naming a new Authority, changing a Role boundary, or choosing between two incompatible designs. decisionRequired states that decision.
- rationale is required: one or two sentences on what you read and why the numbers are what they are.
- You are reporting observations. You do NOT decide whether this Issue stops for a human; a configured policy does.

Repository: %s
Issue: #%d %s

%s`, repo, issue.IssueNumber, strings.TrimSpace(issue.Title), strings.TrimSpace(issue.Body))
}

// appendEscalationAuthorization carries a human authorization into the spec
// prompt. Without it the human's guidance would be recorded and then dropped,
// and the agent would author the spec as if nobody had answered.
func appendEscalationAuthorization(prompt string, scope *checkpointScope) string {
	if scope == nil || !scope.HumanAuthorized {
		return prompt
	}
	guidance := strings.TrimSpace(scope.AuthorizedGuidance)
	if guidance == "" {
		return prompt
	}
	return prompt + fmt.Sprintf("\n\n---\nHUMAN AUTHORIZATION: this Issue was escalated for human review before spec authoring and a human authorized you to proceed. Their decision: %s\nWrite the spec within that decision; do not re-litigate it.\n---", guidance)
}

// compactStrings drops empty/whitespace-only entries while preserving order.
func compactStrings(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			compacted = append(compacted, trimmed)
		}
	}
	if len(compacted) == 0 {
		return nil
	}
	return compacted
}
