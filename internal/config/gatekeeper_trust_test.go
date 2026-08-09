package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func gatekeeperIssues(cfg Config) []ValidationIssue {
	var issues []ValidationIssue
	validateGatekeeperRoleConfig(cfg.Roles.Gatekeeper, "roles.gatekeeper", &issues)
	return issues
}

func TestGatekeeperDefaultsOwnMergeStrategy(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Gatekeeper.Strategy != MergeStrategySquash {
		t.Fatalf("strategy = %q, want squash", cfg.Roles.Gatekeeper.Strategy)
	}
	if issues := gatekeeperIssues(cfg); len(issues) != 0 {
		t.Fatalf("default config produced issues: %+v", issues)
	}
}

func TestEmptyGatekeeperStrategyNormalizesToSquash(t *testing.T) {
	t.Parallel()
	empty := MergeStrategy(" \t")
	cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Strategy: &empty}}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Gatekeeper.Strategy != MergeStrategySquash {
		t.Fatalf("strategy = %q, want squash", cfg.Roles.Gatekeeper.Strategy)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("normalized empty strategy rejected: %v", err)
	}
}

func TestGatekeeperTrustAndStrategyValidation(t *testing.T) {
	t.Parallel()
	for _, trust := range []GatekeeperTrustLevel{GatekeeperTrustObserve, GatekeeperTrustAdvise, GatekeeperTrustAuto} {
		for _, strategy := range []MergeStrategy{MergeStrategySquash, MergeStrategyMerge, MergeStrategyRebase} {
			cfg := Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: trust, Strategy: strategy}}}
			if issues := gatekeeperIssues(cfg); len(issues) != 0 {
				t.Fatalf("trust=%q strategy=%q issues=%+v", trust, strategy, issues)
			}
		}
	}
	for _, test := range []struct {
		role GatekeeperRoleConfig
		path string
	}{
		{role: GatekeeperRoleConfig{Trust: "merge-everything", Strategy: MergeStrategySquash}, path: "roles.gatekeeper.trust"},
		{role: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto, Strategy: "octopus"}, path: "roles.gatekeeper.strategy"},
	} {
		issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: test.role}})
		if len(issues) != 1 || issues[0].Path != test.path {
			t.Fatalf("role=%+v issues=%+v, want %s", test.role, issues, test.path)
		}
	}
}

func TestReviewerAutoMergeEnabledProducesMigrationError(t *testing.T) {
	t.Parallel()
	enabled := true
	for _, partial := range []PartialConfig{
		{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &enabled}}}},
		{Projects: &[]PartialProjectRefConfig{{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &enabled}}}}}},
	} {
		cfg, err := Normalize(t.TempDir(), partial)
		if err != nil {
			t.Fatal(err)
		}
		err = Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "roles.gatekeeper.trust") {
			t.Fatalf("Validate() error = %v, want explicit Gatekeeper migration", err)
		}
	}
}

func TestGatekeeperReviewThresholdAcceptsDefaultAndRejectsNegative(t *testing.T) {
	t.Parallel()
	if issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto}}}); len(issues) != 0 {
		t.Fatalf("default threshold issues = %+v", issues)
	}
	issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{RequiredReviewChangedLines: -1}}})
	if len(issues) != 1 || issues[0].Path != "roles.gatekeeper.requiredReviewChangedLines" {
		t.Fatalf("issues = %+v, want negative threshold rejected", issues)
	}
}

func TestGatekeeperReviewThresholdDefaultAndExplicitZeroScopes(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if got := cfg.Roles.Gatekeeper.RequiredReviewChangedLines; got != 200 {
		t.Fatalf("global threshold = %d, want normalized default 200", got)
	}
	zero := 0
	mergeConfig(&cfg, PartialConfig{Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{RequiredReviewChangedLines: &zero}}})
	if got := cfg.Roles.Gatekeeper.RequiredReviewChangedLines; got != 0 {
		t.Fatalf("explicit global zero = %d, want threshold disabled", got)
	}
	cfg.Roles.Gatekeeper.RequiredReviewChangedLines = 200
	cfg.Projects = []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{RequiredReviewChangedLines: &zero}}}}
	if got := ProjectRoleConfigs(cfg, "demo").Gatekeeper.RequiredReviewChangedLines; got != 0 {
		t.Fatalf("explicit project zero = %d, want project threshold disabled", got)
	}
}

func TestGatekeeperAutoRejectsMarkerlessCommentCleanPolicy(t *testing.T) {
	t.Parallel()
	clean := ReviewerReviewEventComment
	cfg := Config{Roles: RoleConfigs{
		Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto, RequiredReviewChangedLines: 200},
		Reviewer:   ReviewerRoleConfig{Behavior: ReviewerConfig{ReviewEvents: ReviewerReviewEventsConfig{Clean: clean}}},
	}}
	var issues []ValidationIssue
	validateGatekeeperReviewEventCompatibility(cfg, &issues)
	if len(issues) != 1 || issues[0].Path != "roles.reviewer.behavior.reviewEvents.clean" {
		t.Fatalf("issues = %+v, want one global clean-event conflict", issues)
	}
}

func TestGatekeeperProjectAutoRejectsInheritedMarkerlessCommentCleanPolicy(t *testing.T) {
	t.Parallel()
	clean := ReviewerReviewEventComment
	auto := GatekeeperTrustAuto
	cfg := Config{
		Roles:    RoleConfigs{Gatekeeper: GatekeeperRoleConfig{RequiredReviewChangedLines: 200}, Reviewer: ReviewerRoleConfig{Behavior: ReviewerConfig{ReviewEvents: ReviewerReviewEventsConfig{Clean: clean}}}},
		Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto}}}},
	}
	var issues []ValidationIssue
	validateGatekeeperReviewEventCompatibility(cfg, &issues)
	if len(issues) != 1 || issues[0].Path != "projects[0].roles.gatekeeper.trust" {
		t.Fatalf("issues = %+v, want one project trust conflict", issues)
	}
}

func TestGatekeeperAutoAllowsMarkerlessCommentWhenReviewThresholdDisabled(t *testing.T) {
	t.Parallel()
	clean := ReviewerReviewEventComment
	cfg := Config{Roles: RoleConfigs{
		Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto, RequiredReviewChangedLines: 0},
		Reviewer:   ReviewerRoleConfig{Behavior: ReviewerConfig{ReviewEvents: ReviewerReviewEventsConfig{Clean: clean}}},
	}}
	var issues []ValidationIssue
	validateGatekeeperReviewEventCompatibility(cfg, &issues)
	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want markerless COMMENT allowed when threshold is disabled", issues)
	}
}

func TestGatekeeperProjectThresholdOverrideControlsCommentCompatibility(t *testing.T) {
	t.Parallel()
	clean := ReviewerReviewEventComment
	auto := GatekeeperTrustAuto
	zero := 0
	positive := 200
	base := Config{
		Roles:    RoleConfigs{Gatekeeper: GatekeeperRoleConfig{RequiredReviewChangedLines: 0}, Reviewer: ReviewerRoleConfig{Behavior: ReviewerConfig{ReviewEvents: ReviewerReviewEventsConfig{Clean: clean}}}},
		Projects: []ProjectRefConfig{{ID: "disabled", Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto, RequiredReviewChangedLines: &zero}}}, {ID: "enabled", Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto, RequiredReviewChangedLines: &positive}}}},
	}
	var issues []ValidationIssue
	validateGatekeeperReviewEventCompatibility(base, &issues)
	if len(issues) != 1 || issues[0].Path != "projects[1].roles.gatekeeper.trust" {
		t.Fatalf("issues = %+v, want only positive project threshold conflict", issues)
	}
}

func TestGatekeeperProjectThresholdOnlyOverrideControlsCommentCompatibility(t *testing.T) {
	t.Parallel()
	clean := ReviewerReviewEventComment
	auto := GatekeeperTrustAuto
	positive := 200
	base := Config{
		Roles:    RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: auto, RequiredReviewChangedLines: 0}, Reviewer: ReviewerRoleConfig{Behavior: ReviewerConfig{ReviewEvents: ReviewerReviewEventsConfig{Clean: clean}}}},
		Projects: []ProjectRefConfig{{ID: "enabled", Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{RequiredReviewChangedLines: &positive}}}},
	}
	var issues []ValidationIssue
	validateGatekeeperReviewEventCompatibility(base, &issues)
	if len(issues) != 1 || issues[0].Path != "projects[0].roles.gatekeeper.requiredReviewChangedLines" {
		t.Fatalf("issues = %+v, want threshold-only project override conflict", issues)
	}
}

func TestGatekeeperRejectsNegativeProjectReviewThreshold(t *testing.T) {
	t.Parallel()
	negative := -1
	cfg := Config{Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{RequiredReviewChangedLines: &negative}}}}}
	var issues []ValidationIssue
	validateCoreConfig(cfg, &issues)
	for _, issue := range issues {
		if issue.Path == "projects[0].roles.gatekeeper.requiredReviewChangedLines" {
			return
		}
	}
	t.Fatalf("issues = %+v, want negative project threshold rejected", issues)
}

func TestDisabledReviewerAutoMergeDefaultStrategyIsAcceptedButNonDefaultFails(t *testing.T) {
	t.Parallel()
	disabled := false
	defaultStrategy := MergeStrategySquash
	cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &disabled, Strategy: &defaultStrategy}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("disabled compatibility input rejected: %v", err)
	}
	raw := string(mustJSON(t, cfg.Roles.Reviewer))
	if strings.Contains(raw, "autoMerge") {
		t.Fatalf("deprecated autoMerge was projected: %s", raw)
	}
	for _, strategy := range []MergeStrategy{MergeStrategyRebase, MergeStrategyMerge} {
		cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &disabled, Strategy: &strategy}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "roles.reviewer.autoMerge.strategy") {
			t.Fatalf("Validate(strategy=%q) error = %v, want explicit migration", strategy, err)
		}
	}
}

func TestRedactProjectSecretsOmitsReviewerCompatibilityBlock(t *testing.T) {
	t.Parallel()
	projects := []ProjectRefConfig{{ID: "project_1", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{}}}}}
	redacted := RedactProjectSecrets(projects)
	if redacted[0].Roles == projects[0].Roles {
		t.Fatal("redaction reused project roles pointer")
	}
	if redacted[0].Roles.Reviewer == nil || redacted[0].Roles.Reviewer.AutoMerge != nil {
		t.Fatalf("redacted reviewer role = %#v, want compatibility block omitted", redacted[0].Roles.Reviewer)
	}
	if projects[0].Roles.Reviewer.AutoMerge == nil {
		t.Fatal("redaction mutated source project")
	}
}

func TestGatekeeperProjectOverrideSurvivesNormalizationAndClone(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	cfg := Config{
		Roles: RoleConfigs{Reviewer: ReviewerRoleConfig{AutoMerge: ReviewerAutoMergeConfig{Enabled: true}}},
		Projects: []ProjectRefConfig{{
			ID:    "looper",
			Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto}},
		}},
	}

	var issues []ValidationIssue
	validateCoreConfig(cfg, &issues)

	for _, issue := range issues {
		if issue.Path == "projects[0].roles.gatekeeper.trust" {
			t.Fatalf("project auto override was rejected: %+v", issues)
		}
	}
}

func TestMergePartialGatekeeperTrust(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	advise := GatekeeperTrustAdvise
	threshold := 350
	mergeConfig(&cfg, PartialConfig{Roles: &PartialRoleConfigs{
		Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &advise, RequiredReviewChangedLines: &threshold},
	}})

	if cfg.Roles.Gatekeeper.Trust != GatekeeperTrustAdvise {
		t.Fatalf("merged trust = %q, want advise", cfg.Roles.Gatekeeper.Trust)
	}
	if cfg.Roles.Gatekeeper.RequiredReviewChangedLines != threshold {
		t.Fatalf("merged threshold = %d, want %d", cfg.Roles.Gatekeeper.RequiredReviewChangedLines, threshold)
	}
}

// clonePartialRoleConfigs is invoked on every configuration layer. A field it
// forgets is silently dropped, and nothing else in the pipeline notices: the
// value type-checks, merges, and validates, then vanishes.
func TestClonePartialRoleConfigsPreservesGatekeeperTrust(t *testing.T) {
	t.Parallel()
	advise := GatekeeperTrustAdvise
	threshold := 350
	original := &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &advise, RequiredReviewChangedLines: &threshold}}

	cloned := clonePartialRoleConfigs(original)

	if cloned == nil || cloned.Gatekeeper == nil || cloned.Gatekeeper.Trust == nil {
		t.Fatal("clone dropped roles.gatekeeper.trust")
	}
	if *cloned.Gatekeeper.Trust != GatekeeperTrustAdvise {
		t.Fatalf("cloned trust = %q, want advise", *cloned.Gatekeeper.Trust)
	}
	if cloned.Gatekeeper.RequiredReviewChangedLines == nil || *cloned.Gatekeeper.RequiredReviewChangedLines != threshold {
		t.Fatalf("cloned threshold = %#v, want %d", cloned.Gatekeeper.RequiredReviewChangedLines, threshold)
	}
	// A shallow copy of the pointer would let a later edit of one layer rewrite
	// another.
	observe := GatekeeperTrustObserve
	*original.Gatekeeper.Trust = observe
	if *cloned.Gatekeeper.Trust != GatekeeperTrustAdvise {
		t.Fatal("clone aliases the original trust pointer")
	}
	threshold = 500
	if *cloned.Gatekeeper.RequiredReviewChangedLines != 350 {
		t.Fatal("clone aliases the original threshold pointer")
	}
}

// The end-to-end property the clone bug broke: a project override configured in a
// file must survive every normalization layer and reach Config.
func TestProjectGatekeeperTrustSurvivesNormalization(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	rebase := MergeStrategyRebase
	partial := PartialConfig{Projects: &[]PartialProjectRefConfig{{Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto, Strategy: &rebase}}}}}
	cloned := clonePartialConfig(partial)
	role := (*cloned.Projects)[0].Roles.Gatekeeper
	if role == nil || role.Trust == nil || *role.Trust != auto || role.Strategy == nil || *role.Strategy != rebase {
		t.Fatalf("cloned role = %+v", role)
	}
	observe := GatekeeperTrustObserve
	(*partial.Projects)[0].Roles.Gatekeeper.Trust = &observe
	if *role.Trust != auto {
		t.Fatal("clone aliases original trust pointer")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
