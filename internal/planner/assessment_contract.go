package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// AssessmentSchemaVersion is the only assessment wire format accepted by the
// pre-spec policy. A future format must be introduced deliberately rather than
// guessed from optional fields.
const AssessmentSchemaVersion = "looper.assessment.v1"

// AssessmentBinding identifies the immutable inputs the daemon supplied to an
// assessment. It is checked against daemon-fetched Issue content and the
// prepared worktree base before policy can consume model-produced evidence.
type AssessmentBinding struct {
	Repo        string `json:"repo"`
	IssueNumber int64  `json:"issueNumber"`
	IssueDigest string `json:"issueDigest"`
	BaseSHA     string `json:"baseSha"`
}

// NewAssessmentBinding validates the daemon-owned inputs for one assessment.
func NewAssessmentBinding(repo string, issueNumber int64, issueTitle, issueBody, baseSHA string) (AssessmentBinding, error) {
	binding := AssessmentBinding{
		Repo:        strings.TrimSpace(repo),
		IssueNumber: issueNumber,
		IssueDigest: IssueContentDigest(issueTitle, issueBody),
		BaseSHA:     strings.TrimSpace(baseSHA),
	}
	if err := binding.validate(); err != nil {
		return AssessmentBinding{}, err
	}
	return binding, nil
}

// IssueContentDigest deliberately covers exactly the Issue text the assessor
// receives. The issue number/repository live separately in AssessmentBinding.
func IssueContentDigest(title, body string) string {
	sum := sha256.Sum256([]byte(title + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func (b AssessmentBinding) validate() error {
	if strings.TrimSpace(b.Repo) == "" {
		return fmt.Errorf("assessment binding repo is required")
	}
	if b.IssueNumber <= 0 {
		return fmt.Errorf("assessment binding issue number must be positive")
	}
	if len(b.IssueDigest) != sha256.Size*2 {
		return fmt.Errorf("assessment binding issue digest must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(b.IssueDigest); err != nil {
		return fmt.Errorf("assessment binding issue digest must be a SHA-256 hex digest")
	}
	if !isFullGitObjectID(b.BaseSHA) {
		return fmt.Errorf("assessment binding base SHA must be a full hexadecimal Git object ID")
	}
	return nil
}

// isFullGitObjectID accepts the full SHA-1 and SHA-256 object ID encodings
// used by Git repositories. Abbreviated names are not stable bindings.
func isFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// AssessmentSurface names a repository contract surface. It is evidence, not
// a permission: the configured policy decides which surfaces require a human.
type AssessmentSurface string

const (
	AssessmentSurfacePublicAPI  AssessmentSurface = "public_api"
	AssessmentSurfaceConfig     AssessmentSurface = "config"
	AssessmentSurfaceCLI        AssessmentSurface = "cli"
	AssessmentSurfaceStorage    AssessmentSurface = "storage"
	AssessmentSurfaceWireFormat AssessmentSurface = "wire_format"
)

// AssessmentRecommendation is an assertion the deterministic policy verifies
// against the supplied evidence. It never authorizes mutation by itself.
type AssessmentRecommendation string

const (
	AssessmentProceed  AssessmentRecommendation = "proceed"
	AssessmentEscalate AssessmentRecommendation = "escalate"
)

// Assessment is the strict, presence-aware record emitted by a read-only
// assessor. All fields are required on the wire, including empty arrays and an
// empty decisionRequest for a proceeding assessment.
type Assessment struct {
	SchemaVersion      string                   `json:"schemaVersion"`
	Binding            AssessmentBinding        `json:"binding"`
	AffectedFiles      []string                 `json:"affectedFiles"`
	Surfaces           []AssessmentSurface      `json:"surfaces"`
	AuthorityQuestions []string                 `json:"authorityQuestions"`
	Recommendation     AssessmentRecommendation `json:"recommendation"`
	DecisionRequest    string                   `json:"decisionRequest"`
}

// AssessmentPolicy is operator-authored policy translated by later Planner
// configuration wiring. Keeping it data-only makes the policy deterministic
// and prevents the assessor from inventing a reason to grant itself access.
type AssessmentPolicy struct {
	MaxAffectedFiles            int
	EscalateSurfaces            []AssessmentSurface
	EscalateOnAuthorityQuestion bool
}

type AssessmentCriterion struct {
	Name     string   `json:"name"`
	Evidence []string `json:"evidence"`
}

type AssessmentDisposition string

const (
	AssessmentDispositionProceed  AssessmentDisposition = "proceed"
	AssessmentDispositionEscalate AssessmentDisposition = "escalate"
)

// AssessmentDecision is deterministic policy output. A later lifecycle layer
// may persist it in the existing planner checkpoint, but this contract creates
// no new durable state on its own.
type AssessmentDecision struct {
	Disposition AssessmentDisposition `json:"disposition"`
	Criteria    []AssessmentCriterion `json:"criteria"`
}

// ParseAssessment rejects partial, unknown, malformed, and ambiguous wire
// data before it can reach the policy. In particular, zero values cannot stand
// in for omitted fields.
func ParseAssessment(raw []byte) (Assessment, error) {
	type wireBinding struct {
		Repo        *string `json:"repo"`
		IssueNumber *int64  `json:"issueNumber"`
		IssueDigest *string `json:"issueDigest"`
		BaseSHA     *string `json:"baseSha"`
	}
	type wireAssessment struct {
		SchemaVersion      *string                   `json:"schemaVersion"`
		Binding            *wireBinding              `json:"binding"`
		AffectedFiles      *[]string                 `json:"affectedFiles"`
		Surfaces           *[]AssessmentSurface      `json:"surfaces"`
		AuthorityQuestions *[]string                 `json:"authorityQuestions"`
		Recommendation     *AssessmentRecommendation `json:"recommendation"`
		DecisionRequest    *string                   `json:"decisionRequest"`
	}
	if err := rejectDuplicateAssessmentFields(raw); err != nil {
		return Assessment{}, err
	}
	var wire wireAssessment
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Assessment{}, fmt.Errorf("decode assessment: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Assessment{}, fmt.Errorf("assessment must contain exactly one JSON object")
	}
	if wire.SchemaVersion == nil || wire.Binding == nil || wire.Binding.Repo == nil || wire.Binding.IssueNumber == nil || wire.Binding.IssueDigest == nil || wire.Binding.BaseSHA == nil || wire.AffectedFiles == nil || wire.Surfaces == nil || wire.AuthorityQuestions == nil || wire.Recommendation == nil || wire.DecisionRequest == nil {
		return Assessment{}, fmt.Errorf("assessment is incomplete: every contract field is required")
	}
	assessment := Assessment{
		SchemaVersion: *wire.SchemaVersion,
		Binding:       AssessmentBinding{Repo: *wire.Binding.Repo, IssueNumber: *wire.Binding.IssueNumber, IssueDigest: *wire.Binding.IssueDigest, BaseSHA: *wire.Binding.BaseSHA},
		AffectedFiles: append([]string(nil), (*wire.AffectedFiles)...), Surfaces: append([]AssessmentSurface(nil), (*wire.Surfaces)...),
		AuthorityQuestions: append([]string(nil), (*wire.AuthorityQuestions)...), Recommendation: *wire.Recommendation, DecisionRequest: *wire.DecisionRequest,
	}
	if err := assessment.validate(); err != nil {
		return Assessment{}, err
	}
	return assessment, nil
}

// rejectDuplicateAssessmentFields rejects duplicate names before decoding into
// structs, because encoding/json otherwise retains only the last occurrence.
func rejectDuplicateAssessmentFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateJSONObjectFields(decoder, "assessment", assessmentWireFields, true); err != nil {
		return err
	}
	return nil
}

var assessmentWireFields = map[string]bool{
	"schemaVersion":      true,
	"binding":            true,
	"affectedFiles":      true,
	"surfaces":           true,
	"authorityQuestions": true,
	"recommendation":     true,
	"decisionRequest":    true,
}

var assessmentBindingWireFields = map[string]bool{
	"repo":        true,
	"issueNumber": true,
	"issueDigest": true,
	"baseSha":     true,
}

func rejectDuplicateJSONObjectFields(decoder *json.Decoder, label string, allowedFields map[string]bool, checkBinding bool) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("%s must be a JSON object", label)
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode %s field name: %w", label, err)
		}
		field, ok := token.(string)
		if !ok {
			return fmt.Errorf("decode %s field name", label)
		}
		// encoding/json accepts case-insensitive struct-field matches. Require
		// the one canonical spelling so differently cased duplicates cannot
		// silently overwrite one another during the later struct decode.
		if !allowedFields[field] {
			return fmt.Errorf("%s contains noncanonical or unknown field %q", label, field)
		}
		if seen[field] {
			return fmt.Errorf("%s repeats field %q", label, field)
		}
		seen[field] = true
		if checkBinding && field == "binding" {
			if err := rejectDuplicateJSONObjectFields(decoder, "assessment binding", assessmentBindingWireFields, false); err != nil {
				return err
			}
			continue
		}
		var discard json.RawMessage
		if err := decoder.Decode(&discard); err != nil {
			return fmt.Errorf("decode %s field %q: %w", label, field, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	return nil
}

func (a *Assessment) validate() error {
	if a.SchemaVersion != AssessmentSchemaVersion {
		return fmt.Errorf("unsupported assessment schema version %q", a.SchemaVersion)
	}
	if err := a.Binding.validate(); err != nil {
		return err
	}
	files, err := canonicalAssessmentStrings(a.AffectedFiles, "affected file", true)
	if err != nil {
		return err
	}
	for _, file := range files {
		clean := path.Clean(file)
		if path.IsAbs(file) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("assessment affected file %q must be a relative repository path", file)
		}
	}
	a.AffectedFiles = files
	surfaces := make([]AssessmentSurface, 0, len(a.Surfaces))
	seenSurfaces := map[AssessmentSurface]bool{}
	for _, surface := range a.Surfaces {
		surface = AssessmentSurface(strings.ToLower(strings.TrimSpace(string(surface))))
		switch surface {
		case AssessmentSurfacePublicAPI, AssessmentSurfaceConfig, AssessmentSurfaceCLI, AssessmentSurfaceStorage, AssessmentSurfaceWireFormat:
		default:
			return fmt.Errorf("unsupported assessment surface %q", surface)
		}
		if seenSurfaces[surface] {
			return fmt.Errorf("assessment repeats surface %q", surface)
		}
		seenSurfaces[surface] = true
		surfaces = append(surfaces, surface)
	}
	sort.Slice(surfaces, func(i, j int) bool { return surfaces[i] < surfaces[j] })
	a.Surfaces = surfaces
	questions, err := canonicalAssessmentStrings(a.AuthorityQuestions, "authority question", false)
	if err != nil {
		return err
	}
	a.AuthorityQuestions = questions
	a.DecisionRequest = strings.TrimSpace(a.DecisionRequest)
	switch a.Recommendation {
	case AssessmentProceed, AssessmentEscalate:
	default:
		return fmt.Errorf("unsupported assessment recommendation %q", a.Recommendation)
	}
	return nil
}

func canonicalAssessmentStrings(values []string, label string, paths bool) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("assessment %s must not be empty", label)
		}
		if paths {
			value = path.Clean(value)
		}
		if seen[value] {
			return nil, fmt.Errorf("assessment repeats %s %q", label, value)
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

// EvaluateAssessment enforces the binding and applies the operator-authored
// policy. A recommendation inconsistent with the fired criteria is rejected:
// model prose or confidence cannot create a path to mutation.
func EvaluateAssessment(expected AssessmentBinding, policy AssessmentPolicy, assessment Assessment) (AssessmentDecision, error) {
	if err := expected.validate(); err != nil {
		return AssessmentDecision{}, fmt.Errorf("expected assessment binding: %w", err)
	}
	if err := assessment.validate(); err != nil {
		return AssessmentDecision{}, err
	}
	if expected != assessment.Binding {
		return AssessmentDecision{}, fmt.Errorf("assessment binding no longer matches current issue or base")
	}
	if policy.MaxAffectedFiles < 0 {
		return AssessmentDecision{}, fmt.Errorf("assessment policy max affected files must not be negative")
	}
	criteria := make([]AssessmentCriterion, 0)
	if policy.MaxAffectedFiles > 0 && len(assessment.AffectedFiles) > policy.MaxAffectedFiles {
		criteria = append(criteria, AssessmentCriterion{Name: "affected_files", Evidence: []string{fmt.Sprintf("assessment lists %d files, exceeding configured maximum %d", len(assessment.AffectedFiles), policy.MaxAffectedFiles)}})
	}
	configuredSurfaces := map[AssessmentSurface]bool{}
	for _, surface := range policy.EscalateSurfaces {
		surface = AssessmentSurface(strings.ToLower(strings.TrimSpace(string(surface))))
		switch surface {
		case AssessmentSurfacePublicAPI, AssessmentSurfaceConfig, AssessmentSurfaceCLI, AssessmentSurfaceStorage, AssessmentSurfaceWireFormat:
		default:
			return AssessmentDecision{}, fmt.Errorf("assessment policy has unsupported surface %q", surface)
		}
		configuredSurfaces[surface] = true
	}
	for _, surface := range assessment.Surfaces {
		if configuredSurfaces[surface] {
			criteria = append(criteria, AssessmentCriterion{Name: "surface_" + string(surface), Evidence: []string{"assessment identifies " + string(surface)}})
		}
	}
	if policy.EscalateOnAuthorityQuestion && len(assessment.AuthorityQuestions) > 0 {
		criteria = append(criteria, AssessmentCriterion{Name: "authority_question", Evidence: append([]string(nil), assessment.AuthorityQuestions...)})
	}
	sort.Slice(criteria, func(i, j int) bool { return criteria[i].Name < criteria[j].Name })
	if len(criteria) == 0 {
		if assessment.Recommendation != AssessmentProceed || assessment.DecisionRequest != "" {
			return AssessmentDecision{}, fmt.Errorf("assessment recommends escalation without a configured criterion")
		}
		return AssessmentDecision{Disposition: AssessmentDispositionProceed, Criteria: []AssessmentCriterion{}}, nil
	}
	if assessment.Recommendation != AssessmentEscalate {
		return AssessmentDecision{}, fmt.Errorf("assessment recommendation contradicts configured escalation criteria")
	}
	if assessment.DecisionRequest == "" {
		return AssessmentDecision{}, fmt.Errorf("assessment escalation requires a specific decision request")
	}
	return AssessmentDecision{Disposition: AssessmentDispositionEscalate, Criteria: criteria}, nil
}
