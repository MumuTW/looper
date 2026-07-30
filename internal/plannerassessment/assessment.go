// Package plannerassessment provides a credential-free, read-only execution
// profile for Planner's pre-spec scope assessment. The assessor runs inside an
// OS-enforced sandbox with no forge/Git/SSH credentials and a read-only
// filesystem; only a disposable temp root is writable. Its structured stdout
// output is the sole input to a deterministic escalation policy. The daemon,
// not the assessor, retains all mutation authority.
package plannerassessment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/processsandbox"
)

// Surface kinds the assessor may flag as affected by the change.
const (
	SurfacePublicAPI     = "public_api"
	SurfaceConfigSchema  = "config_schema"
	SurfaceCLISurface    = "cli_surface"
	SurfaceStorageSchema = "storage_schema"
	SurfaceWireFormat    = "wire_format"
)

// validPublicSurfaces is the closed set of surfaces the assessor may report.
var validPublicSurfaces = map[string]bool{
	SurfacePublicAPI:     true,
	SurfaceConfigSchema:  true,
	SurfaceCLISurface:    true,
	SurfaceStorageSchema: true,
	SurfaceWireFormat:    true,
}

// ScopeAssessment is the strict structured output the assessor emits. Every
// boolean flag must agree with its detail/evidence field; absence or
// contradiction fails closed.
type ScopeAssessment struct {
	EstimatedFilesTouched    int      `json:"estimatedFilesTouched"`
	EstimatedPackagesTouched int      `json:"estimatedPackagesTouched"`
	FilesEvidence            []string `json:"filesEvidence"`
	PackagesEvidence         []string `json:"packagesEvidence"`
	PublicSurfaceChange      bool     `json:"publicSurfaceChange"`
	PublicSurfaces           []string `json:"publicSurfaces"`
	PublicSurfaceEvidence    string   `json:"publicSurfaceEvidence"`
	ADRConflict              bool     `json:"adrConflict"`
	ConflictingADR           string   `json:"conflictingADR"`
	ADRConflictEvidence      string   `json:"adrConflictEvidence"`
	UnauthorizedDecision     bool     `json:"unauthorizedDecision"`
	DecisionRequired         string   `json:"decisionRequired"`
	Rationale                string   `json:"rationale"`
}

// Validate checks that the assessment is internally consistent: required
// booleans agree with their evidence fields, blast-radius estimates are
// non-negative, and enum values are recognized. A model emission that fails
// any check is treated as malformed and fails closed — it never reaches the
// policy gate.
func (a ScopeAssessment) Validate() error {
	if a.EstimatedFilesTouched < 0 {
		return fmt.Errorf("estimatedFilesTouched must not be negative")
	}
	if a.EstimatedPackagesTouched < 0 {
		return fmt.Errorf("estimatedPackagesTouched must not be negative")
	}
	if a.PublicSurfaceChange && len(a.PublicSurfaces) == 0 {
		return fmt.Errorf("publicSurfaceChange is true but no publicSurfaces listed")
	}
	if !a.PublicSurfaceChange && len(a.PublicSurfaces) > 0 {
		return fmt.Errorf("publicSurfaceChange is false but publicSurfaces present")
	}
	for _, surface := range a.PublicSurfaces {
		if !validPublicSurfaces[surface] {
			return fmt.Errorf("unsupported public surface %q", surface)
		}
	}
	if a.ADRConflict && strings.TrimSpace(a.ConflictingADR) == "" {
		return fmt.Errorf("adrConflict is true but no conflictingAdr listed")
	}
	if !a.ADRConflict && strings.TrimSpace(a.ConflictingADR) != "" {
		return fmt.Errorf("adrConflict is false but conflictingAdr present")
	}
	if a.UnauthorizedDecision && strings.TrimSpace(a.DecisionRequired) == "" {
		return fmt.Errorf("unauthorizedDecision is true but no decisionRequired listed")
	}
	if !a.UnauthorizedDecision && strings.TrimSpace(a.DecisionRequired) != "" {
		return fmt.Errorf("unauthorizedDecision is false but decisionRequired present")
	}
	return nil
}

// ParseScopeAssessment decodes and validates a scope assessment from raw model
// output. Presence-aware: integer/boolean zero values from absent keys are
// rejected by JSON decoding into typed fields — the caller must supply every
// field. Malformed or contradictory assessments return a non-nil error so the
// caller can fail closed.
func ParseScopeAssessment(raw []byte) (ScopeAssessment, error) {
	var assessment ScopeAssessment
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&assessment); err != nil {
		return ScopeAssessment{}, fmt.Errorf("decode scope assessment: %w", err)
	}
	if err := assessment.Validate(); err != nil {
		return ScopeAssessment{}, fmt.Errorf("validate scope assessment: %w", err)
	}
	return assessment, nil
}

// EscalationPolicy is a per-project configurable gate. Each criterion fires
// independently when its threshold is met; any fired criterion triggers
// escalation. Zero-valued blast-radius thresholds disable that criterion.
type EscalationPolicy struct {
	MaxFilesTouched        int  `json:"maxFilesTouched"`
	MaxPackagesTouched     int  `json:"maxPackagesTouched"`
	OnPublicSurfaceChange  bool `json:"onPublicSurfaceChange"`
	OnADRConflict          bool `json:"onADRConflict"`
	OnUnauthorizedDecision bool `json:"onUnauthorizedDecision"`
}

// DefaultEscalationPolicy returns the documented defaults.
func DefaultEscalationPolicy() EscalationPolicy {
	return EscalationPolicy{
		MaxFilesTouched:        10,
		MaxPackagesTouched:     3,
		OnPublicSurfaceChange:  true,
		OnADRConflict:          true,
		OnUnauthorizedDecision: true,
	}
}

// Criterion is a human-readable escalation reason surfaced to the operator.
type Criterion struct {
	Name      string `json:"name"`
	Threshold string `json:"threshold"`
	Actual    string `json:"actual"`
	Evidence  string `json:"evidence,omitempty"`
}

// EvaluateResult is the outcome of policy evaluation: which criteria fired
// and the structured record bound to the assessment.
type EvaluateResult struct {
	Escalated bool             `json:"escalated"`
	Criteria  []Criterion      `json:"criteria"`
	Record    EscalationRecord `json:"record"`
}

// EscalationRecord is the durable structured record bound to Issue content
// and base SHA. It captures what fired, the evidence, and what the human is
// being asked to decide.
type EscalationRecord struct {
	Policy     EscalationPolicy `json:"policy"`
	Assessment ScopeAssessment  `json:"assessment"`
	Criteria   []Criterion      `json:"criteria"`
	// BoundTo ties the record to a specific Issue and repository state so a
	// later change retires and reassesses rather than reusing a stale decision.
	IssueNumber      int64  `json:"issueNumber"`
	Repo             string `json:"repo"`
	IssueContentHash string `json:"issueContentHash"`
	BaseSHA          string `json:"baseSHA"`
}

// Evaluate applies deterministic policy to the assessment. The model never
// decides to escalate; the policy does. A criterion fires only when its knob
// is enabled and the threshold is exceeded. Blast-radius thresholds of 0
// disable that criterion.
func Evaluate(policy EscalationPolicy, assessment ScopeAssessment, issueNumber int64, repo, contentHash, baseSHA string) EvaluateResult {
	var criteria []Criterion

	if policy.MaxFilesTouched > 0 && assessment.EstimatedFilesTouched > policy.MaxFilesTouched {
		criteria = append(criteria, Criterion{
			Name:      "blast-radius-files",
			Threshold: fmt.Sprintf(">%d files", policy.MaxFilesTouched),
			Actual:    fmt.Sprintf("%d files", assessment.EstimatedFilesTouched),
			Evidence:  strings.Join(assessment.FilesEvidence, ", "),
		})
	}
	if policy.MaxPackagesTouched > 0 && assessment.EstimatedPackagesTouched > policy.MaxPackagesTouched {
		criteria = append(criteria, Criterion{
			Name:      "blast-radius-packages",
			Threshold: fmt.Sprintf(">%d packages", policy.MaxPackagesTouched),
			Actual:    fmt.Sprintf("%d packages", assessment.EstimatedPackagesTouched),
			Evidence:  strings.Join(assessment.PackagesEvidence, ", "),
		})
	}
	if policy.OnPublicSurfaceChange && assessment.PublicSurfaceChange {
		criteria = append(criteria, Criterion{
			Name:     "public-surface-change",
			Actual:   strings.Join(assessment.PublicSurfaces, ", "),
			Evidence: assessment.PublicSurfaceEvidence,
		})
	}
	if policy.OnADRConflict && assessment.ADRConflict {
		criteria = append(criteria, Criterion{
			Name:     "adr-conflict",
			Actual:   assessment.ConflictingADR,
			Evidence: assessment.ADRConflictEvidence,
		})
	}
	if policy.OnUnauthorizedDecision && assessment.UnauthorizedDecision {
		criteria = append(criteria, Criterion{
			Name:   "unauthorized-decision",
			Actual: assessment.DecisionRequired,
		})
	}

	return EvaluateResult{
		Escalated: len(criteria) > 0,
		Criteria:  criteria,
		Record: EscalationRecord{
			Policy:           policy,
			Assessment:       assessment,
			Criteria:         criteria,
			IssueNumber:      issueNumber,
			Repo:             repo,
			IssueContentHash: contentHash,
			BaseSHA:          baseSHA,
		},
	}
}

// Profile configures a credential-free, read-only assessment execution.
type Profile struct {
	// TempRoot is the only writable directory in the sandbox.
	TempRoot string
	// CWD is the read-only working directory (the prepared worktree).
	CWD string
	// AgentCommand is the vendor binary to invoke (e.g. "codex").
	AgentCommand string
	// AgentArgs are passed verbatim to the binary.
	AgentArgs []string
	// Prompt is the assessment prompt carrying Issue context.
	Prompt string
	// Timeout bounds the assessment run.
	Timeout time.Duration
}

// Run executes the assessment inside the credential-free sandbox. The
// returned assessment is parsed and validated; any sandbox or decode error
// fails closed so the caller can treat it as a non-escalable failure.
func Run(ctx context.Context, profile Profile) (ScopeAssessment, error) {
	sandboxProfile := processsandbox.ReadOnlyProfile(
		nil, // no extra read roots beyond the implicit ones
		nil, // no network egress
	)
	result, err := processsandbox.Run(ctx, processsandbox.Options{
		CWD:     profile.CWD,
		Command: profile.AgentCommand,
		Args:    profile.AgentArgs,
		Environment: processsandbox.ToolEnvironment{
			PrependPath: []string{},
		},
		Timeout: profile.Timeout,
		Profile: sandboxProfile,
	})
	if err != nil {
		return ScopeAssessment{}, fmt.Errorf("sandboxed assessment execution failed: %w", err)
	}
	// The assessor emits one JSON object on stdout; anything else is malformed.
	return ParseScopeAssessment([]byte(result.Stdout))
}
