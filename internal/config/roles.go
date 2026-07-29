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

// CodingRoleConfig is the per-role configuration consumed by the discovery
// registry. Agent-free policy roles such as Gatekeeper leave Agent and
// Instructions empty.
//
// Behaviour that only one role's runner implements (reviewer auto-merge,
// publish mode, spec-review labels) deliberately stays out of this struct:
// it belongs to that runner's own config section. Putting it here would
// imply a custom role could switch it on, which no amount of configuration
// can make true.
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
	// role, so an unset Priority would silently claim the most consequential
	// position in the tick. Shipped roles always carry a compiled-in
	// priority (CodingRolesFromLegacy); a TOML-authored custom role must set
	// priority explicitly — resolveCodingRoles rejects the section
	// otherwise.
	Priority int `json:"priority"`
}

// Lane priorities for the roles looper ships with. Custom roles authored
// from TOML declare their own priority to sit between these (spaced by 10 so
// reordering means picking a number, not renumbering the block), and a
// shipped role's priority can be overridden with
// `[roles.coding.<name>].priority`. A role name with no compiled-in
// discoverer is skipped by the lane builder.
//
// Coordinator is listed here to keep the whole tick order readable in one
// place, but it does not travel through CodingRoleConfig.Priority like the
// others: it is not a coding role, so coordinatorLane applies it directly.
const (
	PriorityPlanner     = 10
	PriorityCoordinator = 20
	PriorityReviewer    = 30
	PriorityFixer       = 40
	PriorityGatekeeper  = 45
	PriorityWorker      = 50
)

// EffectiveCodingRoles returns the role map, projecting it from the legacy
// named fields when it is empty.
//
// Normalize populates the map, but a Config assembled directly — tests, and
// any caller that builds the struct rather than loading a file — would
// otherwise present zero roles, which reads as "discovery is off" instead of
// "this config never went through normalization". Silently running no lanes
// is a far worse failure than projecting on demand.
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

// CodingRolesFromLegacy projects the four named legacy role structs onto the
// role map. It is the base layer of the canonical registry: Normalize builds
// this projection first, then overlays the TOML-authored roles.coding.*
// sections onto it (resolveCodingRoles). Discovery filters, per-role
// instructions, and agent resolution for the shipped roles are still read
// from the named fields, so the projection is what keeps those consumers and
// the registry in agreement.
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

	out[RoleGatekeeper] = CodingRoleConfig{
		Priority: PriorityGatekeeper,
		Discovery: RoleDiscoveryConfig{
			Enabled:       true,
			Source:        WorkSourcePullRequest,
			Labels:        []string{},
			LabelMode:     LabelModeAll,
			IncludeDrafts: true,
		},
	}

	return out
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

// collectAuthoredCodingRoles merges the roles.coding maps from each config
// layer in order, later layers winning field-by-field per role. Keys are
// normalized with NormalizeRoleName; an empty key is a load-time error
// rather than a role no one can address.
func collectAuthoredCodingRoles(partials ...PartialConfig) (map[string]PartialCodingRoleConfig, []ValidationIssue) {
	var authored map[string]PartialCodingRoleConfig
	var issues []ValidationIssue
	for _, partial := range partials {
		if partial.Roles == nil {
			continue
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
		}
	}
	return authored, issues
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

// resolveCodingRoles builds the canonical registry: the legacy projection
// plus the TOML-authored roles.coding.* sections.
//
// Precedence rules:
//   - A shipped role name (planner, worker, reviewer, fixer) keeps taking
//     discovery, instructions, and agent from its legacy named section —
//     those consumers still read the named fields, so accepting them here
//     would configure something nothing reads. Only priority may be set, and
//     it overrides the compiled-in lane priority. Any other field is a
//     load-time error.
//   - "coordinator" is not a coding role and is rejected.
//   - Any other name authors a custom role. Priority is required (an unset
//     priority would sort ahead of every shipped role) and discovery.source
//     is required; the discovery block is checked by ValidateRoleDiscovery.
func resolveCodingRoles(legacy map[string]CodingRoleConfig, authored map[string]PartialCodingRoleConfig) (map[string]CodingRoleConfig, []ValidationIssue) {
	resolved := make(map[string]CodingRoleConfig, len(legacy)+len(authored))
	for name, role := range legacy {
		resolved[name] = role
	}

	var issues []ValidationIssue
	for _, name := range sortedKeys(authored) {
		role := authored[name]
		pathPrefix := "roles.coding." + name

		if name == "coordinator" {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix,
				Message: "is not a coding role; configure roles.coordinator.* instead",
			})
			continue
		}

		if _, shipped := resolved[name]; shipped {
			if role.Discovery != nil {
				issues = append(issues, ValidationIssue{
					Path:    pathPrefix + ".discovery",
					Message: shippedCodingRoleFieldMessage(name, "discovery"),
				})
			}
			if role.Instructions != nil {
				issues = append(issues, ValidationIssue{
					Path:    pathPrefix + ".instructions",
					Message: shippedCodingRoleFieldMessage(name, "instructions"),
				})
			}
			if role.Agent != nil {
				issues = append(issues, ValidationIssue{
					Path:    pathPrefix + ".agent",
					Message: shippedCodingRoleFieldMessage(name, "agent"),
				})
			}
			if role.Priority != nil {
				entry := resolved[name]
				entry.Priority = *role.Priority
				resolved[name] = entry
			}
			continue
		}

		if role.Priority == nil {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix + ".priority",
				Message: "is required for a TOML-authored coding role (it orders the discovery lane within a scheduler tick)",
			})
			continue
		}
		if role.Discovery == nil || role.Discovery.Source == nil {
			issues = append(issues, ValidationIssue{
				Path:    pathPrefix + ".discovery.source",
				Message: `is required for a TOML-authored coding role (must be "issue" or "pull_request")`,
			})
			continue
		}
		issues = append(issues, validatePartialRoleDiscovery(pathPrefix, role.Discovery)...)

		entry := CodingRoleConfig{Priority: *role.Priority}
		if role.Instructions != nil {
			entry.Instructions = *role.Instructions
		}
		if !isEmptyRoleAgentConfig(role.Agent) {
			entry.Agent = cloneRoleAgentConfig(role.Agent)
		}
		entry.Discovery = roleDiscoveryConfigFromPartial(role.Discovery)
		issues = append(issues, validateCodingRoleDiscoveryCommon(pathPrefix, entry.Discovery)...)
		resolved[name] = entry
	}

	if len(issues) > 0 {
		return nil, issues
	}
	return resolved, nil
}

func shippedCodingRoleFieldMessage(name, field string) string {
	if name == RoleGatekeeper {
		return "does not apply to the shipped gatekeeper role; its policy is compiled in (only priority may be set here)"
	}
	return "does not apply to the shipped " + name + " role; configure roles." + name + "." + field + " instead (only priority may be set here)"
}

func roleDiscoveryConfigFromPartial(partial *PartialRoleDiscoveryConfig) RoleDiscoveryConfig {
	// Match the established role-trigger default: omitted labelMode means all.
	// Leaving the zero value would make an otherwise minimal custom role fail
	// validation despite the field being documented as optional.
	discovery := RoleDiscoveryConfig{LabelMode: LabelModeAll}
	if partial.Enabled != nil {
		discovery.Enabled = *partial.Enabled
	}
	if partial.Source != nil {
		discovery.Source = *partial.Source
	}
	if partial.Labels != nil {
		discovery.Labels = append([]string(nil), (*partial.Labels)...)
	}
	if partial.LabelMode != nil {
		discovery.LabelMode = *partial.LabelMode
	}
	if partial.RequireAssigneeCurrentUser != nil {
		discovery.RequireAssigneeCurrentUser = *partial.RequireAssigneeCurrentUser
	}
	if partial.PlaneAssigneeID != nil {
		discovery.PlaneAssigneeID = *partial.PlaneAssigneeID
	}
	if partial.IncludeDrafts != nil {
		discovery.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.AuthorFilter != nil {
		discovery.AuthorFilter = *partial.AuthorFilter
	}
	if partial.RequireReviewRequest != nil {
		discovery.RequireReviewRequest = *partial.RequireReviewRequest
	}
	if partial.EnableSelfReview != nil {
		discovery.EnableSelfReview = *partial.EnableSelfReview
	}
	return discovery
}

// validatePartialRoleDiscovery checks field presence before pointer values are
// collapsed into RoleDiscoveryConfig. This is necessary for explicit zero
// values: includeDrafts=false is still a pull-request-only field and must not
// be silently accepted on an issue-source role.
func validatePartialRoleDiscovery(pathPrefix string, d *PartialRoleDiscoveryConfig) []ValidationIssue {
	if d == nil || d.Source == nil {
		return nil
	}
	var fields []string
	switch *d.Source {
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
	default:
		return []ValidationIssue{{
			Path:    pathPrefix + ".discovery.source",
			Message: `must be "issue" or "pull_request"`,
		}}
	}
	issues := make([]ValidationIssue, 0, len(fields))
	for _, field := range fields {
		issues = append(issues, ValidationIssue{
			Path:    pathPrefix + ".discovery." + field,
			Message: "does not apply to source " + strconv.Quote(string(*d.Source)),
		})
	}
	return issues
}

// validateCodingRoleDiscoveryCommon validates the source-independent trigger
// contract plus enum values that are meaningful only when present. Source
// applicability is handled from the partial form during Normalize and by
// ValidateRoleDiscovery for Config values assembled directly.
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
