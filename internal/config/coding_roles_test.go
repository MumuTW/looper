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

func TestNormalizeRejectsExplicitZeroCrossSourceDiscoveryFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		raw      string
		wantPath string
	}{
		{
			name: "false pull request field on issue source",
			raw: `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "issue"
includeDrafts = false
`,
			wantPath: "roles.coding.auditor.discovery.includeDrafts",
		},
		{
			name: "false issue field on pull request source",
			raw: `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "pull_request"
requireAssigneeCurrentUser = false
`,
			wantPath: "roles.coding.auditor.discovery.requireAssigneeCurrentUser",
		},
		{
			name: "empty issue field on pull request source",
			raw: `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "pull_request"
planeAssigneeId = ""
`,
			wantPath: "roles.coding.auditor.discovery.planeAssigneeId",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Normalize(t.TempDir(), mustDecodeTOML(t, tc.raw))
			if !hasIssuePath(normalizeIssuePaths(err), tc.wantPath) {
				t.Fatalf("issue paths = %v, want %s (err = %v)", normalizeIssuePaths(err), tc.wantPath, err)
			}
		})
	}
}

func TestNormalizeValidatesCustomRoleDiscovery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		discovery string
		wantPath  string
	}{
		{name: "invalid label mode", discovery: "source = \"issue\"\nlabelMode = \"sometimes\"", wantPath: "roles.coding.auditor.discovery.labelMode"},
		{name: "blank label", discovery: "source = \"issue\"\nlabels = [\"\"]", wantPath: "roles.coding.auditor.discovery.labels[0]"},
		{name: "duplicate label", discovery: "source = \"issue\"\nlabels = [\"audit\", \"audit\"]", wantPath: "roles.coding.auditor.discovery.labels"},
		{name: "invalid author filter", discovery: "source = \"pull_request\"\nauthorFilter = \"somebody_else\"", wantPath: "roles.coding.auditor.discovery.authorFilter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := "[roles.coding.auditor]\npriority = 60\n\n[roles.coding.auditor.discovery]\n" + tc.discovery + "\n"
			_, err := Normalize(t.TempDir(), mustDecodeTOML(t, raw))
			if !hasIssuePath(normalizeIssuePaths(err), tc.wantPath) {
				t.Fatalf("issue paths = %v, want %s (err = %v)", normalizeIssuePaths(err), tc.wantPath, err)
			}
		})
	}
}

func TestNormalizeCustomRoleDefaultsLabelModeToAll(t *testing.T) {
	t.Parallel()

	cfg := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60

[roles.coding.auditor.discovery]
source = "issue"
labels = ["audit"]
`))
	if got := cfg.Roles.Coding["auditor"].Discovery.LabelMode; got != LabelModeAll {
		t.Fatalf("labelMode = %q, want %q", got, LabelModeAll)
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

func TestNormalizeRejectsAmbiguousCaseFoldedRoleNames(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.Auditor]
priority = 60
[roles.coding.Auditor.discovery]
source = "issue"

[roles.coding.auditor]
priority = 70
[roles.coding.auditor.discovery]
source = "pull_request"
`)
	_, err := Normalize(t.TempDir(), partial)
	if !hasIssuePath(normalizeIssuePaths(err), "roles.coding.auditor") {
		t.Fatalf("issue paths = %v, want roles.coding.auditor (err = %v)", normalizeIssuePaths(err), err)
	}
}

func TestCanonicalizePartialForMigrationPreservesCodingRoles(t *testing.T) {
	t.Parallel()

	partial := mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60
instructions = "audit"

[roles.coding.auditor.discovery]
source = "issue"
labels = ["audit"]

[roles.coding.auditor.agent]
model = "test-model"
`)
	canonical := CanonicalizePartialForMigration(partial)
	role, ok := canonical.Roles.Coding["auditor"]
	if !ok || role.Priority == nil || *role.Priority != 60 || role.Discovery == nil || role.Discovery.Labels == nil || role.Agent == nil {
		t.Fatalf("canonical coding role = %#v, want preserved deep clone", role)
	}

	*role.Priority = 99
	*role.Discovery.Source = WorkSourcePullRequest
	(*role.Discovery.Labels)[0] = "changed"
	*role.Agent.Model = "changed-model"
	original := partial.Roles.Coding["auditor"]
	if *original.Priority != 60 || *original.Discovery.Source != WorkSourceIssue || (*original.Discovery.Labels)[0] != "audit" || *original.Agent.Model != "test-model" {
		t.Fatalf("CanonicalizePartialForMigration mutated caller: %#v", original)
	}
}

func TestValidateRejectsInvalidCustomRoleInstructionsAndAgent(t *testing.T) {
	t.Parallel()

	cfg := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60
instructions = "ignore lifecycle"

[roles.coding.auditor.discovery]
source = "issue"

[roles.coding.auditor.agent]
profile = "missing"
`))
	err := Validate(cfg)
	paths := normalizeIssuePaths(err)
	for _, want := range []string{"roles.coding.auditor.instructions", "roles.coding.auditor.agent.profile"} {
		if !hasIssuePath(paths, want) {
			t.Errorf("issue paths = %v, want %s (err = %v)", paths, want, err)
		}
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

func TestRestartRequiredChangesFlagsCustomCodingRoleContent(t *testing.T) {
	t.Parallel()

	oldConfig := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60
instructions = "old"
[roles.coding.auditor.discovery]
source = "issue"
labels = ["old"]
`))
	newConfig := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.auditor]
priority = 60
instructions = "new"
[roles.coding.auditor.discovery]
source = "issue"
labels = ["new"]
`))
	if got := RestartRequiredChanges(oldConfig, newConfig); !hasIssuePath(got, "roles.coding.auditor") {
		t.Fatalf("RestartRequiredChanges = %v, want roles.coding.auditor", got)
	}

	// A mirrored shipped-role hot field stays governed by the normal JSON
	// diff and must not be promoted to a restart-bound registry edit.
	shippedOld := mustNormalize(t)
	shippedNew := shippedOld
	shippedNew.Roles.Worker.Instructions = "hot guidance"
	shippedNew.Roles.Coding = CodingRolesFromLegacy(shippedNew.Roles)
	if got := RestartRequiredChanges(shippedOld, shippedNew); hasIssuePath(got, "roles.coding.worker") {
		t.Fatalf("RestartRequiredChanges = %v, roles.coding.worker must remain hot", got)
	}
}
