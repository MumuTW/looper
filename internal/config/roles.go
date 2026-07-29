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
}

// CodingRoleNames returns the configured coding role names in a stable order
// so scheduler lanes, validation errors, and logs stay deterministic.
func CodingRoleNames(roles RoleConfigs) []string {
	names := make([]string, 0, len(roles.Coding))
	for name := range roles.Coding {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NormalizeRoleName lowercases and trims a role name. Role names are map keys
// and appear in TOML section headers, so they are compared case-insensitively.
func NormalizeRoleName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// CodingRolesFromLegacy projects the four named legacy role structs onto the
// role map. It is the single bridge between the old shape and the new one:
// while both exist, normalization calls this so cfg.Roles.Coding is the only
// thing consumers need to read.
func CodingRolesFromLegacy(roles RoleConfigs) map[string]CodingRoleConfig {
	out := make(map[string]CodingRoleConfig, 4)

	out[CodingRolePlanner] = CodingRoleConfig{
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
