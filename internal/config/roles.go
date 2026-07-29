package config

import (
	"sort"
	"strings"
)

// WorkSource is the kind of remote object a coding role scans for work.
// It is the real axis along which discovery settings differ: planner and
// worker watch issues, reviewer and fixer watch pull requests. Role names
// carry no discovery semantics of their own.
type WorkSource string

const (
	WorkSourceIssue       WorkSource = "issue"
	WorkSourcePullRequest WorkSource = "pull_request"
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

// CodingRoleConfig is the per-role configuration common to every coding role.
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
	// role, so an unset Priority silently claims the most consequential
	// position in the tick. Every construction path fills it today
	// (CodingRolesFromLegacy), which is what keeps that unreachable. Whoever
	// makes roles authorable from TOML has to close it — reject an unset
	// priority, or default it to the tail — before an omitted field can mean
	// "run first".
	Priority int `json:"priority"`
}

// Lane priorities for the roles looper ships with. These are the only lane
// priorities in play: roles cannot yet be authored from configuration (see
// RoleConfigs.Coding), and a role name with no compiled-in discoverer is
// skipped, so the set below is closed. Spaced by 10 so reordering means
// editing one constant rather than renumbering the block.
//
// Coordinator is listed here to keep the whole tick order readable in one
// place, but it does not travel through CodingRoleConfig.Priority like the
// others: it is not a coding role, so coordinatorLane applies it directly.
const (
	PriorityPlanner     = 10
	PriorityCoordinator = 20
	PriorityReviewer    = 30
	PriorityFixer       = 40
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
// role map. It is the single bridge between the old shape and the new one:
// while both exist, normalization calls this so cfg.Roles.Coding is populated
// for the consumers that have already moved onto it. That is currently the
// discovery-lane builder alone, which reads role names and Priority; every
// other consumer still reads the named fields, so the projection is a
// migration step and not yet the authority.
func CodingRolesFromLegacy(roles RoleConfigs) map[string]CodingRoleConfig {
	out := make(map[string]CodingRoleConfig, 4)

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

	return out
}

// ValidateRoleDiscovery reports every field set on d that does not apply to
// d.Source, plus an unknown-source error. Returning the offending field names
// (rather than a bare "invalid config") is the point: a field that silently
// does nothing is the failure mode this model exists to remove.
func ValidateRoleDiscovery(role string, d RoleDiscoveryConfig) []string {
	var issues []string
	switch d.Source {
	case WorkSourceIssue:
		for _, field := range prOnlyDiscoveryFieldsSet(d) {
			issues = append(issues, "roles."+role+".discovery."+field+
				` does not apply to source "issue"`)
		}
	case WorkSourcePullRequest:
		for _, field := range issueOnlyDiscoveryFieldsSet(d) {
			issues = append(issues, "roles."+role+".discovery."+field+
				` does not apply to source "pull_request"`)
		}
	default:
		issues = append(issues, "roles."+role+
			`.discovery.source must be "issue" or "pull_request"`)
	}
	return issues
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
