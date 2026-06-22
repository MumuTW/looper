package forgejocontract

import "testing"

type forgejoMirrorIntent string

const (
	forgejoMirrorRun           forgejoMirrorIntent = "run"
	forgejoMirrorSkip          forgejoMirrorIntent = "skip"
	forgejoMirrorNoCounterpart forgejoMirrorIntent = "no-counterpart"
)

type forgejoMirrorCase struct {
	GitHubCase  string
	ForgejoCase string
	Intent      forgejoMirrorIntent
	Reason      string
}

var forgejoContractMirrorCases = []forgejoMirrorCase{
	{GitHubCase: "TestInvariantGatewayUsesSupportedGHJSONFields", ForgejoCase: "TestInvariantForgejoGatewayUsesSupportedRESTSurface", Intent: forgejoMirrorRun, Reason: "Forgejo REST maps issue/PR list/view and response normalization; thread resolution slice remains a skipped unsupported subcase"},
	{GitHubCase: "TestInvariantGatewayDependencyWrappersUseSupportedRoutes", ForgejoCase: "TestInvariantForgejoGatewayDependencyWrappersUseSupportedRoutes", Intent: forgejoMirrorSkip, Reason: "Forgejo MVP does not support Coordinator/dependency-gate REST behavior"},
	{GitHubCase: "TestInvariantGatewaySupportsRepoForms", ForgejoCase: "TestInvariantForgejoGatewaySupportsRepoForms", Intent: forgejoMirrorSkip, Reason: "Forgejo authority is explicit baseUrl plus owner/repo, not GitHub host-qualified repo forms"},
	{GitHubCase: "TestFakeGHFixtureRejectsUnsupportedJSONField", ForgejoCase: "none", Intent: forgejoMirrorNoCounterpart, Reason: "tests fake GitHub CLI fixture schema enforcement, not provider behavior"},
	{GitHubCase: "TestRealGHReadOnlySmoke", ForgejoCase: "TestForgejoReadOnlySmoke", Intent: forgejoMirrorRun, Reason: "Forgejo REST read-only live smoke belongs behind Forgejo live env"},
}

func TestForgejoContractMirrorInventory(t *testing.T) {
	for _, tc := range forgejoContractMirrorCases {
		if tc.GitHubCase == "" || tc.ForgejoCase == "" || tc.Reason == "" {
			t.Fatalf("incomplete mirror case: %#v", tc)
		}
		switch tc.Intent {
		case forgejoMirrorRun, forgejoMirrorSkip, forgejoMirrorNoCounterpart:
		default:
			t.Fatalf("invalid mirror intent for %s: %q", tc.GitHubCase, tc.Intent)
		}
	}
}

func TestInvariantForgejoGatewayDependencyWrappersUseSupportedRoutes(t *testing.T) {
	t.Skip("Forgejo Coordinator/dependency-gate behavior is unsupported by the current MVP capability set")
}

func TestInvariantForgejoGatewaySupportsRepoForms(t *testing.T) {
	t.Skip("Forgejo uses explicit baseUrl plus owner/repo configuration, not GitHub host-qualified repo forms")
}
