package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

var requiredRuntimeCapabilities = []RuntimeCapability{
	CapabilityExecutableDiscovery,
	CapabilityVersionProbe,
	CapabilityAuthenticationProbe,
	CapabilityNonInteractivePrompt,
	CapabilityModelSelection,
	CapabilityStructuredTerminal,
	CapabilityStructuredLiveEvents,
	CapabilitySessionIdentity,
	CapabilityHeadlessResume,
	CapabilityInteractiveTakeover,
	CapabilityProcessContainment,
	CapabilityCancellation,
	CapabilityBoundedOutput,
	CapabilityToolNetworkRestriction,
}

func TestRuntimeContractsCoverEveryConfiguredVendor(t *testing.T) {
	contracts, err := RuntimeContracts()
	if err != nil {
		t.Fatal(err)
	}
	vendors := config.ConfigurableAgentVendors()
	if len(contracts) != len(vendors) {
		t.Fatalf("contracts = %d, configured vendors = %d", len(contracts), len(vendors))
	}
	for index, contract := range contracts {
		if contract.Version != 1 || contract.Vendor != vendors[index] || strings.TrimSpace(contract.DefaultCommand) == "" {
			t.Fatalf("contract[%d] = %#v", index, contract)
		}
		for _, capability := range requiredRuntimeCapabilities {
			evidence, ok := contract.Capabilities[capability]
			if !ok {
				t.Errorf("%s missing %s", contract.Vendor, capability)
				continue
			}
			if !validRuntimeSupport(evidence.Support) || strings.TrimSpace(evidence.Version) == "" || strings.TrimSpace(evidence.Source) == "" {
				t.Errorf("%s capability %s has incomplete evidence: %#v", contract.Vendor, capability, evidence)
			}
		}
	}
}

func TestRuntimeContractMatchesBehaviorBearingAdapter(t *testing.T) {
	for vendor, adapter := range runtimeAdapters {
		contract, ok := RuntimeContractFor(vendor)
		if !ok {
			t.Fatalf("RuntimeContractFor(%q) missing", vendor)
		}
		if contract.Vendor != vendor || contract.DefaultCommand == "" {
			t.Errorf("%s identity mismatch: contract=%#v", vendor, contract)
		}
		if got, want := contract.Supports(CapabilityHeadlessResume), adapter.resolveNativeResumeArgs != nil; got != want {
			t.Errorf("%s headless resume declaration=%v implementation=%v", vendor, got, want)
		}
		if got, want := contract.Supports(CapabilityInteractiveTakeover), adapter.resolveInteractiveResume != nil; got != want {
			t.Errorf("%s takeover declaration=%v implementation=%v", vendor, got, want)
		}

		_, args := ResolveSpawn(ExecutorConfig{Vendor: vendor, LiveToolEvents: true}, "/tmp/worktree", "prompt")
		hasJSON := slices.Contains(args, "--json")
		if got := contract.Supports(CapabilityStructuredLiveEvents); got != hasJSON {
			t.Errorf("%s structured events declaration=%v argv --json=%v (%v)", vendor, got, hasJSON, args)
		}
	}
}

func TestRuntimeCapabilitySecurityAndContinuationMatrix(t *testing.T) {
	want := map[config.AgentVendor]struct {
		headless, takeover, liveEvents, restrictedNetwork bool
	}{
		config.AgentVendorClaudeCode:        {headless: true, takeover: true},
		config.AgentVendorCodex:             {headless: true, takeover: true, liveEvents: true, restrictedNetwork: true},
		config.AgentVendorOpenCode:          {headless: true},
		config.AgentVendorCursorCLI:         {headless: true},
		config.AgentVendorGrokBuild:         {},
		config.AgentVendorDevinExperimental: {},
	}
	for vendor, expected := range want {
		contract, ok := RuntimeContractFor(vendor)
		if !ok {
			t.Fatalf("missing %s contract", vendor)
		}
		got := struct {
			headless, takeover, liveEvents, restrictedNetwork bool
		}{
			contract.Supports(CapabilityHeadlessResume),
			contract.Supports(CapabilityInteractiveTakeover),
			contract.Supports(CapabilityStructuredLiveEvents),
			contract.Supports(CapabilityToolNetworkRestriction),
		}
		if got != expected {
			t.Errorf("%s capabilities = %#v, want %#v", vendor, got, expected)
		}
	}
}

func TestRuntimeContractForReturnsDefensiveCopy(t *testing.T) {
	first, ok := RuntimeContractFor(config.AgentVendorCodex)
	if !ok {
		t.Fatal("codex contract missing")
	}
	first.Capabilities[CapabilityHeadlessResume] = unsupportedEvidence("mutated-test-copy")
	second, _ := RuntimeContractFor(config.AgentVendorCodex)
	if !second.Supports(CapabilityHeadlessResume) {
		t.Fatal("caller mutation changed adapter authority")
	}
}

func validRuntimeSupport(support RuntimeSupport) bool {
	switch support {
	case RuntimeSupportNative, RuntimeSupportLooperTranslated, RuntimeSupportLooperEnforced, RuntimeSupportExperimental, RuntimeSupportUnsupported:
		return true
	default:
		return false
	}
}
