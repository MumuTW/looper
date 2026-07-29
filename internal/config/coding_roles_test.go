package config

import (
	"strings"
	"testing"
)

func mustNormalize(t *testing.T, partials ...PartialConfig) Config {
	t.Helper()
	cfg, err := Normalize(t.TempDir(), partials...)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	return cfg
}

func mustDecodeTOML(t *testing.T, raw string) PartialConfig {
	t.Helper()
	partial, err := DecodePartialConfigFile("config.toml", []byte(raw))
	if err != nil {
		t.Fatalf("DecodePartialConfigFile() error = %v", err)
	}
	return partial
}

func normalizeIssuePaths(err error) []string {
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		return nil
	}
	paths := make([]string, 0, len(validationErr.Issues))
	for _, issue := range validationErr.Issues {
		paths = append(paths, issue.Path)
	}
	return paths
}

func hasIssuePath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

// A TOML-authored custom role lands in the canonical registry with its
// discovery, instructions, agent, and priority intact, and takes its place in
// the priority-ordered lane list.
func TestNormalizeTomlAuthoredCodingRole(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 25
instructions = "audit the diff"

[roles.coding.auditor.discovery]
enabled = true
source = "issue"
labels = ["looper:audit"]
labelMode = "any"
requireAssigneeCurrentUser = true

[roles.coding.auditor.agent]
profile = "fast"
`)

	cfg := mustNormalize(t, partial)

	auditor, ok := cfg.Roles.Coding["auditor"]
	if !ok {
		t.Fatalf("auditor missing from registry; got %v", CodingRoleNames(cfg.Roles))
	}
	if auditor.Priority != 25 {
		t.Errorf("auditor priority = %d, want 25", auditor.Priority)
	}
	if auditor.Instructions != "audit the diff" {
		t.Errorf("auditor instructions = %q", auditor.Instructions)
	}
	if auditor.Agent == nil || auditor.Agent.Profile == nil || *auditor.Agent.Profile != "fast" {
		t.Errorf("auditor agent = %#v, want profile fast", auditor.Agent)
	}
	if !auditor.Discovery.Enabled || auditor.Discovery.Source != WorkSourceIssue {
		t.Errorf("auditor discovery = %+v", auditor.Discovery)
	}
	if auditor.Discovery.LabelMode != LabelModeAny {
		t.Errorf("auditor labelMode = %q, want %q", auditor.Discovery.LabelMode, LabelModeAny)
	}
	if !auditor.Discovery.RequireAssigneeCurrentUser {
		t.Errorf("auditor requireAssigneeCurrentUser was not carried over")
	}

	// Priority 25 sits between planner (10) and reviewer (30).
	wantOrder := []string{"planner", "auditor", "reviewer", "fixer", "worker"}
	gotOrder := CodingRoleNames(cfg.Roles)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("lane order = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("lane order = %v, want %v", gotOrder, wantOrder)
		}
	}
}

// Without any roles.coding.* section the registry is exactly the legacy
// projection — the legacy named fields keep working unchanged.
func TestNormalizeLegacyRolesRegistryUnchanged(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.worker]
autoDiscovery = true
instructions = "implement carefully"

[roles.worker.triggers]
labels = ["looper:worker-ready"]
labelMode = "all"
`)

	cfg := mustNormalize(t, partial)
	want := CodingRolesFromLegacy(cfg.Roles)

	if len(cfg.Roles.Coding) != len(want) {
		t.Fatalf("registry has %d roles, want %d", len(cfg.Roles.Coding), len(want))
	}
	for name, wantRole := range want {
		gotRole, ok := cfg.Roles.Coding[name]
		if !ok {
			t.Fatalf("role %q missing from registry", name)
		}
		if gotRole.Priority != wantRole.Priority ||
			gotRole.Instructions != wantRole.Instructions ||
			gotRole.Discovery.Source != wantRole.Discovery.Source ||
			gotRole.Discovery.Enabled != wantRole.Discovery.Enabled {
			t.Errorf("role %q = %+v, want %+v", name, gotRole, wantRole)
		}
	}
	if !cfg.Roles.Coding[CodingRoleWorker].Discovery.Enabled {
		t.Errorf("worker autoDiscovery did not reach the registry")
	}
}

// For a shipped role, roles.coding.<name>.priority overrides the compiled-in
// lane priority; every other registry field keeps coming from the legacy
// named section.
func TestNormalizeCodingRolePriorityOverridesShippedRole(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.worker.triggers]
labels = ["looper:worker-ready"]

[roles.coding.worker]
priority = 5
`)

	cfg := mustNormalize(t, partial)

	worker := cfg.Roles.Coding[CodingRoleWorker]
	if worker.Priority != 5 {
		t.Fatalf("worker priority = %d, want 5", worker.Priority)
	}
	if len(worker.Discovery.Labels) != 1 || worker.Discovery.Labels[0] != "looper:worker-ready" {
		t.Errorf("worker discovery labels = %v, want the legacy trigger labels", worker.Discovery.Labels)
	}
	if got := CodingRoleNames(cfg.Roles); got[0] != CodingRoleWorker {
		t.Errorf("first lane = %q, want worker after the priority override", got[0])
	}
}

// A shipped role's non-priority fields in roles.coding.* are rejected: those
// consumers still read the legacy named section, so accepting them would
// configure something nothing reads.
func TestNormalizeRejectsShippedRoleNonPriorityFields(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.worker]
priority = 5
instructions = "ignored"

[roles.coding.worker.discovery]
enabled = true
`)

	_, err := Normalize(t.TempDir(), partial)
	if err == nil {
		t.Fatal("Normalize() error = nil, want a validation error")
	}
	paths := normalizeIssuePaths(err)
	if !hasIssuePath(paths, "roles.coding.worker.instructions") {
		t.Errorf("issue paths = %v, want roles.coding.worker.instructions", paths)
	}
	if !hasIssuePath(paths, "roles.coding.worker.discovery") {
		t.Errorf("issue paths = %v, want roles.coding.worker.discovery", paths)
	}
}

// An unset priority must not default to zero: zero sorts ahead of every
// shipped role, silently claiming the first lane.
func TestNormalizeRejectsCustomRoleWithoutPriority(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.auditor.discovery]
source = "issue"
`)

	_, err := Normalize(t.TempDir(), partial)
	if !hasIssuePath(normalizeIssuePaths(err), "roles.coding.auditor.priority") {
		t.Fatalf("issue paths = %v, want roles.coding.auditor.priority (err = %v)", normalizeIssuePaths(err), err)
	}
}

func TestNormalizeRejectsCustomRoleWithoutSource(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60
`)

	_, err := Normalize(t.TempDir(), partial)
	if !hasIssuePath(normalizeIssuePaths(err), "roles.coding.auditor.discovery.source") {
		t.Fatalf("issue paths = %v, want roles.coding.auditor.discovery.source (err = %v)", normalizeIssuePaths(err), err)
	}
}

// ValidateRoleDiscovery is wired into normalization: fields belonging to the
// other work source are load-time errors, not inert settings.
func TestNormalizeRejectsCrossSourceDiscoveryFields(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "issue"
includeDrafts = true
`)

	_, err := Normalize(t.TempDir(), partial)
	if !hasIssuePath(normalizeIssuePaths(err), "roles.coding.auditor.discovery.includeDrafts") {
		t.Fatalf("issue paths = %v, want roles.coding.auditor.discovery.includeDrafts (err = %v)", normalizeIssuePaths(err), err)
	}
}

func TestNormalizeRejectsCoordinatorAsCodingRole(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.coordinator]
priority = 15
`)

	_, err := Normalize(t.TempDir(), partial)
	if !hasIssuePath(normalizeIssuePaths(err), "roles.coding.coordinator") {
		t.Fatalf("issue paths = %v, want roles.coding.coordinator (err = %v)", normalizeIssuePaths(err), err)
	}
}

// The registry is global; a project-scoped roles.coding.* section would be
// configured but inert.
func TestNormalizeRejectsProjectCodingRoles(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[[projects]]
id = "demo"
name = "Demo"
repoPath = "/repos/demo"

[projects.roles.coding.auditor]
priority = 60
`)

	_, err := Normalize(t.TempDir(), partial)
	paths := normalizeIssuePaths(err)
	found := false
	for _, path := range paths {
		if strings.HasPrefix(path, "projects[0].roles.coding") {
			found = true
		}
	}
	if !found {
		t.Fatalf("issue paths = %v, want projects[0].roles.coding (err = %v)", paths, err)
	}
}

// Config layers merge field-by-field per authored role: a later layer
// overrides the fields it sets and inherits the rest.
func TestNormalizeCodingRolesMergeAcrossLayers(t *testing.T) {
	t.Parallel()

	base := mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "issue"
labels = ["looper:audit"]
`)
	override := mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 15
`)

	cfg := mustNormalize(t, base, override)

	auditor := cfg.Roles.Coding["auditor"]
	if auditor.Priority != 15 {
		t.Errorf("auditor priority = %d, want 15 from the later layer", auditor.Priority)
	}
	if auditor.Discovery.Source != WorkSourceIssue {
		t.Errorf("auditor source = %q, want issue from the earlier layer", auditor.Discovery.Source)
	}
	if len(auditor.Discovery.Labels) != 1 || auditor.Discovery.Labels[0] != "looper:audit" {
		t.Errorf("auditor labels = %v, want the earlier layer's labels", auditor.Discovery.Labels)
	}
}

// Role names are normalized case-insensitively, matching how the legacy
// sections and map keys compare.
func TestNormalizeCodingRoleNameCaseInsensitive(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.Auditor]
priority = 60

[roles.coding.Auditor.discovery]
source = "issue"
`)

	cfg := mustNormalize(t, partial)
	if _, ok := cfg.Roles.Coding["auditor"]; !ok {
		t.Fatalf("registry keys = %v, want a normalized auditor entry", CodingRoleNames(cfg.Roles))
	}
}

// Registry changes are restart-bound: Roles.Coding is not serialized, so the
// JSON diff cannot see it — the explicit guard keeps an edit to roles.coding.*
// from applying silently neither hot nor via restart.
func TestRestartRequiredChangesFlagsCodingRoleRegistry(t *testing.T) {
	t.Parallel()

	base := mustNormalize(t)

	withCustom := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "issue"
`))
	if got := RestartRequiredChanges(base, withCustom); !hasIssuePath(got, "roles.coding.auditor") {
		t.Errorf("RestartRequiredChanges = %v, want roles.coding.auditor", got)
	}

	withPriority := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.worker]
priority = 5
`))
	if got := RestartRequiredChanges(base, withPriority); !hasIssuePath(got, "roles.coding.worker") {
		t.Errorf("RestartRequiredChanges = %v, want roles.coding.worker", got)
	}

	// An identical reload reports nothing.
	if got := RestartRequiredChanges(withCustom, withCustom); len(got) != 0 {
		t.Errorf("RestartRequiredChanges = %v, want no changes", got)
	}
}
