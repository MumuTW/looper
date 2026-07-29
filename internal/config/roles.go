package config

import (
	"sort"
	"strconv"
	"strings"
)

// WorkSource is the kind of remote object a discovery role scans for work.
// It is the real axis along which discovery settings differ: planner and
// worker watch issues; reviewer, fixer, and Gatekeeper watch pull requests.
// Role names carry no discovery semantics of their own.
type WorkSource string

const (
	WorkSourceIssue       WorkSource = "issue"
	WorkSourcePullRequest WorkSource = "pull_request"
	// RoleGatekeeper is a compiled-in policy role. It has no agent binding and
	// discovers every open pull request from the source rather than a label.
	RoleGatekeeper = "gatekeeper"
)

// AuthorFilter scopes pull-request discovery by who opened the PR.
type AuthorFilter string

const (
	AuthorFilterCurrentUser AuthorFilter = "current_user"
	AuthorFilterAny         AuthorFilter = "any"
)

// RoleDiscoveryConfig describes how one coding role decides that a remote
// issue or pull request is work for it.
//
// Fields are split by the WorkSource they apply to. A field belonging to the
// other source is not silently ignored: ValidateRoleDiscovery rejects it at
// load time, so "configured but inert" is a startup error rather than a
// behaviour that quietly never happens.
type RoleDiscoveryConfig struct {
	Enabled bool       `json:"enabled"`
	Source  WorkSource `json:"source"`

	// Label gating applies to both sources. Empty Labels means no label
	// requirement; LabelMode decides whether all or any must be present.
	Labels    []string  `json:"labels"`
	LabelMode LabelMode `json:"labelMode"`

	// Issue-source only.
	RequireAssigneeCurrentUser bool `json:"requireAssigneeCurrentUser,omitempty"`
	// PlaneAssigneeID scopes discovery on a Plane task-source project to
	// work-items assigned to this Plane member UUID. Plane assignees are
	// UUIDs (not GitHub logins), so RequireAssigneeCurrentUser cannot route
	// them; set this per person instead. Empty = label-only discovery.
	PlaneAssigneeID string `json:"planeAssigneeId,omitempty"`

	// Pull-request-source only.
	IncludeDrafts        bool         `json:"includeDrafts,omitempty"`
	AuthorFilter         AuthorFilter `json:"authorFilter,omitempty"`
	RequireReviewRequest bool         `json:"requireReviewRequest,omitempty"`
	EnableSelfReview     bool         `json:"enableSelfReview,omitempty"`
}

// CodingRoleConfig is the per-role configuration consumed by the canonical
// registry. It is deliberately limited to behavior shared by the compiled
// planner, worker, reviewer, and fixer runners. Runner-specific behavior
// (reviewer auto-merge, publish mode, spec-review labels) stays in that
// runner's named section.
type CodingRoleConfig struct {
	Discovery    RoleDiscoveryConfig `json:"discovery"`
	Instructions string              `json:"instructions,omitempty"`
	Agent        *RoleAgentConfig    `json:"agent,omitempty"`

	// Priority orders this role's discovery lane within a scheduler tick.
	// Lower runs first. Order is load-bearing rather than cosmetic: a claim
	// phase runs between consecutive lanes, so an earlier role gets first
	// call on the free run slots. A new role declares where it belongs
	// instead of inheriting whatever order a map happened to produce.
	//
	// The zero value is not a safe default: it sorts ahead of every shipped
	// role, so validation requires a positive priority.
	Priority int `json:"priority"`
}

// Lane priorities for the roles looper ships with. `roles.coding.<name>` may
// change a runner-backed role's priority. Triager and Coordinator are
// internal lanes, not coding-role registry entries.
const (
	PriorityTriager     = 5
	PriorityPlanner     = 10
	PriorityCoordinator = 20
	PriorityReviewer    = 30
	PriorityFixer       = 40
	PriorityGatekeeper  = 45
	PriorityWorker      = 50
)

// EffectiveCodingRoles returns the canonical role map, projecting legacy
// fields only for Config values assembled directly without Normalize.
func EffectiveCodingRoles(roles RoleConfigs) map[string]CodingRoleConfig {
	if len(roles.Coding) > 0 {
		return roles.Coding
	}
	return CodingRolesFromLegacy(roles)
}

// CodingRoleNames returns the configured coding role names ordered by lane
// priority, falling back to the name so equal priorities stay deterministic
// across runs rather than following Go's randomized map iteration.
func CodingRoleNames(roles RoleConfigs) []string {
	effective := EffectiveCodingRoles(roles)
	names := make([]string, 0, len(effective))
	for name := range effective {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := effective[names[i]], effective[names[j]]
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		return names[i] < names[j]
	})
	return names
}

// NormalizeRoleName lowercases and trims a role name. Role names are map keys
// and appear in TOML section headers, so they are compared case-insensitively.
func NormalizeRoleName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// CodingRolesFromLegacy projects the named role structs onto the canonical
// registry. Normalize applies TOML-authored roles.coding.* fields afterwards;
// the registry, not the legacy fields, is then consumed by discovery, agent
// resolution, and custom instructions.
func CodingRolesFromLegacy(roles RoleConfigs) map[string]CodingRoleConfig {
	out := make(map[string]CodingRoleConfig, 5)

	out[CodingRolePlanner] = CodingRoleConfig{
		Priority:     PriorityPlanner,
		Instructions: roles.Planner.Instructions,
		Agent:        roles.Planner.Agent,
		Discovery: RoleDiscoveryConfig{
			Enabled:                    roles.Planner.AutoDiscovery,
			Source:                     WorkSourceIssue,
			Labels:                     append([]string(nil), roles.Planner.Triggers.Labels...),
			LabelMode:                  roles.Planner.Triggers.LabelMode,
			RequireAssigneeCurrentUser: roles.Planner.Triggers.RequireAssigneeCurrentUser,
			PlaneAssigneeID:            roles.Planner.Triggers.PlaneAssigneeID,
		},
	}

	out[CodingRoleWorker] = CodingRoleConfig{
		Priority:     PriorityWorker,
		Instructions: roles.Worker.Instructions,
		Agent:        roles.Worker.Agent,
		Discovery: RoleDiscoveryConfig{
			Enabled:                    roles.Worker.AutoDiscovery,
			Source:                     WorkSourceIssue,
			Labels:                     append([]string(nil), roles.Worker.Triggers.Labels...),
			LabelMode:                  roles.Worker.Triggers.LabelMode,
			RequireAssigneeCurrentUser: roles.Worker.Triggers.RequireAssigneeCurrentUser,
			PlaneAssigneeID:            roles.Worker.Triggers.PlaneAssigneeID,
		},
	}

	out[CodingRoleFixer] = CodingRoleConfig{
		Priority:     PriorityFixer,
		Instructions: roles.Fixer.Instructions,
		Agent:        roles.Fixer.Agent,
		Discovery: RoleDiscoveryConfig{
			Enabled:       roles.Fixer.AutoDiscovery,
			Source:        WorkSourcePullRequest,
			Labels:        append([]string(nil), roles.Fixer.Triggers.Labels...),
			LabelMode:     roles.Fixer.Triggers.LabelMode,
			IncludeDrafts: roles.Fixer.Triggers.IncludeDrafts,
			AuthorFilter:  AuthorFilter(roles.Fixer.Triggers.AuthorFilter),
		},
	}

	out[CodingRoleReviewer] = CodingRoleConfig{
		Priority:     PriorityReviewer,
		Instructions: roles.Reviewer.Instructions,
		Agent:        roles.Reviewer.Agent,
		Discovery: RoleDiscoveryConfig{
			Enabled:              roles.Reviewer.Discovery.AutoDiscovery,
			Source:               WorkSourcePullRequest,
			Labels:               append([]string(nil), roles.Reviewer.Discovery.Triggers.Labels...),
			LabelMode:            roles.Reviewer.Discovery.Triggers.LabelMode,
			IncludeDrafts:        roles.Reviewer.Discovery.Triggers.IncludeDrafts,
			RequireReviewRequest: roles.Reviewer.Discovery.Triggers.RequireReviewRequest,
			EnableSelfReview:     roles.Reviewer.Discovery.Triggers.EnableSelfReview,
		},
	}

	out[RoleGatekeeper] = compiledGatekeeperRole()

	return out
}

func compiledGatekeeperRole() CodingRoleConfig {
	return CodingRoleConfig{
		Priority: PriorityGatekeeper,
		Discovery: RoleDiscoveryConfig{
			Enabled:       true,
			Source:        WorkSourcePullRequest,
			Labels:        []string{},
			LabelMode:     LabelModeAll,
			IncludeDrafts: true,
		},
	}
}

// CodingRoleSource identifies the fixed work source implemented by each
// compiled agent runner. A schema entry cannot choose a different source:
// that would leave configuration without a compatible runner.
func CodingRoleSource(name string) (WorkSource, bool) {
	switch name {
	case CodingRolePlanner, CodingRoleWorker:
		return WorkSourceIssue, true
	case CodingRoleReviewer, CodingRoleFixer:
		return WorkSourcePullRequest, true
	default:
		return "", false
	}
}

// ValidateRoleDiscovery reports every field set on d that does not apply to
// d.Source, plus an unknown-source error. pathPrefix is the config path of
// the owning role section (for example "roles.coding.auditor"), so each
// issue points at the exact key to fix. Reporting the offending field (rather
// than a bare "invalid config") is the point: a field that silently does
// nothing is the failure mode this model exists to remove.
func ValidateRoleDiscovery(pathPrefix string, d RoleDiscoveryConfig) []ValidationIssue {
	var issues []ValidationIssue
	switch d.Source {
	case WorkSourceIssue:
		for _, field := range prOnlyDiscoveryFieldsSet(d) {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix + ".discovery." + field,
				Message: `does not apply to source "issue"`,
			})
		}
	case WorkSourcePullRequest:
		for _, field := range issueOnlyDiscoveryFieldsSet(d) {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix + ".discovery." + field,
				Message: `does not apply to source "pull_request"`,
			})
		}
	default:
		issues = append(issues, ValidationIssue{
			Path:    pathPrefix + ".discovery.source",
			Message: `must be "issue" or "pull_request"`,
		})
	}
	return issues
}

// collectAuthoredCodingRoles merges each layer's shared legacy role fields
// followed by its roles.coding map. This gives roles.coding.* precedence over
// the named form in the same layer while preserving the normal file -> env ->
// CLI precedence across layers. The second result records instruction values
// still authored by roles.coding after that merge, so Normalize can retain its
// early safety validation without treating an overridden legacy instruction as
// a coding-section error. Keys are normalized with NormalizeRoleName; an empty
// key is a load-time error rather than a role no one can address.
func collectAuthoredCodingRoles(partials ...PartialConfig) (map[string]PartialCodingRoleConfig, map[string]struct{}, []ValidationIssue) {
	var authored map[string]PartialCodingRoleConfig
	var authoredInstructions map[string]struct{}
	var issues []ValidationIssue
	for _, partial := range partials {
		if partial.Roles == nil {
			continue
		}
		legacy := legacyCodingRoleOverrides(*partial.Roles)
		for _, name := range sortedKeys(legacy) {
			if authored == nil {
				authored = make(map[string]PartialCodingRoleConfig)
			}
			authored[name] = mergePartialCodingRoleConfig(authored[name], legacy[name])
			if legacy[name].Instructions != nil {
				delete(authoredInstructions, name)
			}
		}
		// A map can contain keys that become the same canonical role name
		// (for example Auditor and auditor). Iterating and merging those keys
		// would make the winning value depend on Go's randomized map order.
		// Reject the ambiguous layer instead, and iterate sorted keys so both
		// diagnostics and valid merges are deterministic.
		seenInLayer := make(map[string]string, len(partial.Roles.Coding))
		for _, rawName := range sortedKeys(partial.Roles.Coding) {
			role := partial.Roles.Coding[rawName]
			name := NormalizeRoleName(rawName)
			if name == "" {
				issues = append(issues, ValidationIssue{
					Path:    "roles.coding",
					Message: "role name must be a non-empty string",
				})
				continue
			}
			if previous, duplicate := seenInLayer[name]; duplicate && previous != rawName {
				issues = append(issues, ValidationIssue{
					Path:    "roles.coding." + name,
					Message: "role name is ambiguous after case-folding and trimming: " + previous + " and " + rawName,
				})
				continue
			}
			seenInLayer[name] = rawName
			if authored == nil {
				authored = make(map[string]PartialCodingRoleConfig)
			}
			authored[name] = mergePartialCodingRoleConfig(authored[name], role)
			if role.Instructions != nil {
				if authoredInstructions == nil {
					authoredInstructions = make(map[string]struct{})
				}
				authoredInstructions[name] = struct{}{}
			}
		}
	}
	return authored, authoredInstructions, issues
}

func mergePartialCodingRoleConfig(base, overlay PartialCodingRoleConfig) PartialCodingRoleConfig {
	if overlay.Priority != nil {
		base.Priority = overlay.Priority
	}
	if overlay.Instructions != nil {
		base.Instructions = overlay.Instructions
	}
	if overlay.Agent != nil {
		if base.Agent == nil {
			base.Agent = &RoleAgentConfig{}
		}
		if overlay.Agent.Profile != nil {
			base.Agent.Profile = overlay.Agent.Profile
		}
		if overlay.Agent.Vendor != nil {
			base.Agent.Vendor = overlay.Agent.Vendor
		}
		if overlay.Agent.Model != nil {
			base.Agent.Model = overlay.Agent.Model
		}
	}
	if overlay.Discovery != nil {
		base.Discovery = mergePartialRoleDiscoveryConfig(base.Discovery, overlay.Discovery)
	}
	return base
}

func mergePartialRoleDiscoveryConfig(base, overlay *PartialRoleDiscoveryConfig) *PartialRoleDiscoveryConfig {
	if base == nil {
		base = &PartialRoleDiscoveryConfig{}
	}
	if overlay.Enabled != nil {
		base.Enabled = overlay.Enabled
	}
	if overlay.Source != nil {
		base.Source = overlay.Source
	}
	if overlay.Labels != nil {
		base.Labels = overlay.Labels
	}
	if overlay.LabelMode != nil {
		base.LabelMode = overlay.LabelMode
	}
	if overlay.RequireAssigneeCurrentUser != nil {
		base.RequireAssigneeCurrentUser = overlay.RequireAssigneeCurrentUser
	}
	if overlay.PlaneAssigneeID != nil {
		base.PlaneAssigneeID = overlay.PlaneAssigneeID
	}
	if overlay.IncludeDrafts != nil {
		base.IncludeDrafts = overlay.IncludeDrafts
	}
	if overlay.AuthorFilter != nil {
		base.AuthorFilter = overlay.AuthorFilter
	}
	if overlay.RequireReviewRequest != nil {
		base.RequireReviewRequest = overlay.RequireReviewRequest
	}
	if overlay.EnableSelfReview != nil {
		base.EnableSelfReview = overlay.EnableSelfReview
	}
	return base
}

// resolveCodingRoles builds the canonical registry: legacy named role sections
// are the compatibility base, and roles.coding.<shipped-runner> overlays the
// same runtime fields. Entries without a compiled runner are rejected rather
// than accepted as configuration that no production consumer can execute.
func resolveCodingRoles(legacy map[string]CodingRoleConfig, authored map[string]PartialCodingRoleConfig) (map[string]CodingRoleConfig, []ValidationIssue) {
	resolved := make(map[string]CodingRoleConfig, len(legacy)+len(authored))
	for name, role := range legacy {
		resolved[name] = cloneCodingRoleConfig(role)
	}

	var issues []ValidationIssue
	for _, name := range sortedKeys(authored) {
		role := authored[name]
		pathPrefix := "roles.coding." + name

		if name == RoleGatekeeper {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix,
				Message: "is a compiled-in policy role and cannot be configured",
			})
			continue
		}
		if name == "coordinator" {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix,
				Message: "is not a coding role; configure roles.coordinator.* instead",
			})
			continue
		}
		source, runnerBacked := CodingRoleSource(name)
		if !runnerBacked {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix,
				Message: "has no compiled runner; roles.coding supports only planner, worker, reviewer, and fixer",
			})
			continue
		}

		entry := resolved[name]
		issues = append(issues, validatePartialRoleDiscovery(pathPrefix, source, role.Discovery)...)
		entry = applyPartialCodingRoleConfig(entry, role)
		entry.Discovery.Source = source
		if entry.Priority <= 0 {
			issues = append(issues, ValidationIssue{Path: pathPrefix + ".priority", Message: "must be a positive integer"})
		}
		issues = append(issues, validateCodingRoleDiscoveryCommon(pathPrefix, entry.Discovery)...)
		resolved[name] = entry
	}

	if len(issues) > 0 {
		return nil, issues
	}
	return resolved, nil
}

func applyPartialCodingRoleConfig(base CodingRoleConfig, partial PartialCodingRoleConfig) CodingRoleConfig {
	base = cloneCodingRoleConfig(base)
	if partial.Priority != nil {
		base.Priority = *partial.Priority
	}
	if partial.Instructions != nil {
		base.Instructions = *partial.Instructions
	}
	if partial.Agent != nil {
		mergeRoleAgentConfig(&base.Agent, partial.Agent)
	}
	if partial.Discovery == nil {
		return base
	}
	discovery := partial.Discovery
	if discovery.Enabled != nil {
		base.Discovery.Enabled = *discovery.Enabled
	}
	if discovery.Source != nil {
		base.Discovery.Source = *discovery.Source
	}
	if discovery.Labels != nil {
		base.Discovery.Labels = cloneStrings(*discovery.Labels)
	}
	if discovery.LabelMode != nil {
		base.Discovery.LabelMode = *discovery.LabelMode
	}
	if discovery.RequireAssigneeCurrentUser != nil {
		base.Discovery.RequireAssigneeCurrentUser = *discovery.RequireAssigneeCurrentUser
	}
	if discovery.PlaneAssigneeID != nil {
		base.Discovery.PlaneAssigneeID = *discovery.PlaneAssigneeID
	}
	if discovery.IncludeDrafts != nil {
		base.Discovery.IncludeDrafts = *discovery.IncludeDrafts
	}
	if discovery.AuthorFilter != nil {
		base.Discovery.AuthorFilter = *discovery.AuthorFilter
	}
	if discovery.RequireReviewRequest != nil {
		base.Discovery.RequireReviewRequest = *discovery.RequireReviewRequest
	}
	if discovery.EnableSelfReview != nil {
		base.Discovery.EnableSelfReview = *discovery.EnableSelfReview
	}
	return base
}

func cloneCodingRoleConfig(role CodingRoleConfig) CodingRoleConfig {
	role.Discovery.Labels = cloneStrings(role.Discovery.Labels)
	role.Agent = cloneRoleAgentConfig(role.Agent)
	return role
}

// validatePartialRoleDiscovery checks field presence before pointer values are
// collapsed. This catches explicit false settings on the wrong source, and
// source mismatches before a runner can receive an inert policy.
func validatePartialRoleDiscovery(pathPrefix string, expectedSource WorkSource, d *PartialRoleDiscoveryConfig) []ValidationIssue {
	if d == nil {
		return nil
	}
	var issues []ValidationIssue
	if d.Source != nil && *d.Source != expectedSource {
		issues = append(issues, ValidationIssue{
			Path:    pathPrefix + ".discovery.source",
			Message: "must be " + strconv.Quote(string(expectedSource)) + " for this compiled runner",
		})
	}
	source := expectedSource
	var fields []string
	switch source {
	case WorkSourceIssue:
		if d.IncludeDrafts != nil {
			fields = append(fields, "includeDrafts")
		}
		if d.AuthorFilter != nil {
			fields = append(fields, "authorFilter")
		}
		if d.RequireReviewRequest != nil {
			fields = append(fields, "requireReviewRequest")
		}
		if d.EnableSelfReview != nil {
			fields = append(fields, "enableSelfReview")
		}
	case WorkSourcePullRequest:
		if d.RequireAssigneeCurrentUser != nil {
			fields = append(fields, "requireAssigneeCurrentUser")
		}
		if d.PlaneAssigneeID != nil {
			fields = append(fields, "planeAssigneeId")
		}
	}
	for _, field := range fields {
		issues = append(issues, ValidationIssue{
			Path:    pathPrefix + ".discovery." + field,
			Message: "does not apply to source " + strconv.Quote(string(source)),
		})
	}
	return issues
}

// validateCodingRoleDiscoveryCommon validates source-independent trigger
// fields after an authored overlay is applied.
func validateCodingRoleDiscoveryCommon(pathPrefix string, d RoleDiscoveryConfig) []ValidationIssue {
	var issues []ValidationIssue
	validateLabelTriggers(d.Labels, d.LabelMode, pathPrefix+".discovery", &issues)
	if d.Source == WorkSourcePullRequest && d.AuthorFilter != "" && !isValidRoleAuthorFilter(d.AuthorFilter) {
		issues = append(issues, ValidationIssue{
			Path:    pathPrefix + ".discovery.authorFilter",
			Message: `must be "current_user" or "any" when set`,
		})
	}
	return issues
}

func isValidRoleAuthorFilter(filter AuthorFilter) bool {
	switch filter {
	case AuthorFilterCurrentUser, AuthorFilterAny:
		return true
	default:
		return false
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// issueOnlyDiscoveryFields and prOnlyDiscoveryFields name the fields rejected
// for the opposite WorkSource. Kept as data so validation messages and the
// field list cannot drift apart.
func issueOnlyDiscoveryFieldsSet(d RoleDiscoveryConfig) []string {
	var set []string
	if d.RequireAssigneeCurrentUser {
		set = append(set, "requireAssigneeCurrentUser")
	}
	if strings.TrimSpace(d.PlaneAssigneeID) != "" {
		set = append(set, "planeAssigneeId")
	}
	return set
}

func prOnlyDiscoveryFieldsSet(d RoleDiscoveryConfig) []string {
	var set []string
	if d.IncludeDrafts {
		set = append(set, "includeDrafts")
	}
	if strings.TrimSpace(string(d.AuthorFilter)) != "" {
		set = append(set, "authorFilter")
	}
	if d.RequireReviewRequest {
		set = append(set, "requireReviewRequest")
	}
	if d.EnableSelfReview {
		set = append(set, "enableSelfReview")
	}
	return set
}
