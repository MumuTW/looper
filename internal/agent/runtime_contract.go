package agent

import (
	"fmt"

	"github.com/MumuTW/looper/internal/config"
)

// RuntimeCapability names behavior core policy may require without branching
// on a CLI vendor. New capabilities extend this vocabulary; they do not add a
// field to every runner or executor input.
type RuntimeCapability string

const (
	CapabilityExecutableDiscovery    RuntimeCapability = "executable_discovery"
	CapabilityVersionProbe           RuntimeCapability = "version_probe"
	CapabilityAuthenticationProbe    RuntimeCapability = "authentication_preflight"
	CapabilityNonInteractivePrompt   RuntimeCapability = "non_interactive_prompt"
	CapabilityModelSelection         RuntimeCapability = "model_selection"
	CapabilityStructuredTerminal     RuntimeCapability = "structured_terminal_result"
	CapabilityStructuredLiveEvents   RuntimeCapability = "structured_live_events"
	CapabilitySessionIdentity        RuntimeCapability = "session_identity"
	CapabilityHeadlessResume         RuntimeCapability = "headless_resume"
	CapabilityInteractiveTakeover    RuntimeCapability = "interactive_takeover"
	CapabilityProcessContainment     RuntimeCapability = "process_containment"
	CapabilityCancellation           RuntimeCapability = "cancellation"
	CapabilityBoundedOutput          RuntimeCapability = "bounded_output"
	CapabilityToolNetworkRestriction RuntimeCapability = "tool_network_restriction"
)

// RuntimeSupport distinguishes CLI-native behavior from Looper-owned
// translation/enforcement. Unsupported is explicit: configuring a vendor does
// not imply it satisfies every lifecycle or security capability.
type RuntimeSupport string

const (
	RuntimeSupportNative           RuntimeSupport = "native"
	RuntimeSupportLooperTranslated RuntimeSupport = "looper_translated"
	RuntimeSupportLooperEnforced   RuntimeSupport = "looper_enforced"
	RuntimeSupportExperimental     RuntimeSupport = "experimental"
	RuntimeSupportUnsupported      RuntimeSupport = "unsupported"
)

type CapabilityEvidence struct {
	Support RuntimeSupport `json:"support"`
	Version string         `json:"version"`
	Source  string         `json:"source"`
}

// RuntimeContract is the behavior-oriented characterization owned by one
// adapter. The adapter declaration is the authority for capability policy;
// contract tests compare it to the adapter's actual functions and spawn shape.
type RuntimeContract struct {
	Version        int                                      `json:"version"`
	Vendor         config.AgentVendor                       `json:"vendor"`
	DefaultCommand string                                   `json:"defaultCommand"`
	Capabilities   map[RuntimeCapability]CapabilityEvidence `json:"capabilities"`
}

func (c RuntimeContract) Supports(capability RuntimeCapability) bool {
	evidence, ok := c.Capabilities[capability]
	return ok && evidence.Support != RuntimeSupportUnsupported
}

func characterizedContract(vendor config.AgentVendor, command string, overrides map[RuntimeCapability]CapabilityEvidence) RuntimeContract {
	common := map[RuntimeCapability]CapabilityEvidence{
		CapabilityExecutableDiscovery:    nativeEvidence("configured-executor/path-resolution"),
		CapabilityVersionProbe:           unsupportedEvidence("no-version-probe"),
		CapabilityAuthenticationProbe:    unsupportedEvidence("no-authentication-preflight"),
		CapabilityNonInteractivePrompt:   nativeEvidence("adapter/argv-prompt"),
		CapabilityModelSelection:         nativeEvidence("adapter/model-argument"),
		CapabilityStructuredTerminal:     translatedEvidence("looper-result-marker/v1"),
		CapabilityStructuredLiveEvents:   unsupportedEvidence("plain-output-only"),
		CapabilitySessionIdentity:        unsupportedEvidence("no-verified-session-capture"),
		CapabilityHeadlessResume:         unsupportedEvidence("checkpoint-restart-only"),
		CapabilityInteractiveTakeover:    unsupportedEvidence("no-verified-interactive-continuity"),
		CapabilityProcessContainment:     enforcedEvidence("configured-executor/process-group"),
		CapabilityCancellation:           enforcedEvidence("configured-executor/confirmed-death"),
		CapabilityBoundedOutput:          enforcedEvidence("configured-executor/bounded-persistence"),
		CapabilityToolNetworkRestriction: unsupportedEvidence("no-enforceable-tool-network-boundary"),
	}
	for capability, evidence := range overrides {
		common[capability] = evidence
	}
	return RuntimeContract{Version: 1, Vendor: vendor, DefaultCommand: command, Capabilities: common}
}

func nativeEvidence(source string) CapabilityEvidence {
	return CapabilityEvidence{Support: RuntimeSupportNative, Version: "characterization/v1", Source: source}
}

func translatedEvidence(source string) CapabilityEvidence {
	return CapabilityEvidence{Support: RuntimeSupportLooperTranslated, Version: "characterization/v1", Source: source}
}

func enforcedEvidence(source string) CapabilityEvidence {
	return CapabilityEvidence{Support: RuntimeSupportLooperEnforced, Version: "characterization/v1", Source: source}
}

func experimentalEvidence(version, source string) CapabilityEvidence {
	return CapabilityEvidence{Support: RuntimeSupportExperimental, Version: version, Source: source}
}

func unsupportedEvidence(source string) CapabilityEvidence {
	return CapabilityEvidence{Support: RuntimeSupportUnsupported, Version: "characterization/v1", Source: source}
}

// RuntimeContractFor returns a defensive copy suitable for status and policy
// consumers. Unknown vendors have no contract rather than an inferred one.
func RuntimeContractFor(vendor config.AgentVendor) (RuntimeContract, bool) {
	adapter, ok := runtimeAdapterFor(vendor)
	if !ok {
		return RuntimeContract{}, false
	}
	contract := adapter.contract
	contract.Capabilities = cloneCapabilityEvidence(contract.Capabilities)
	return contract, true
}

// RuntimeContracts returns configured vendors in config's stable order.
func RuntimeContracts() ([]RuntimeContract, error) {
	vendors := config.ConfigurableAgentVendors()
	contracts := make([]RuntimeContract, 0, len(vendors))
	for _, vendor := range vendors {
		contract, ok := RuntimeContractFor(vendor)
		if !ok {
			return nil, fmt.Errorf("configured agent vendor has no runtime adapter contract: %s", vendor)
		}
		contracts = append(contracts, contract)
	}
	return contracts, nil
}

func cloneCapabilityEvidence(source map[RuntimeCapability]CapabilityEvidence) map[RuntimeCapability]CapabilityEvidence {
	cloned := make(map[RuntimeCapability]CapabilityEvidence, len(source))
	for capability, evidence := range source {
		cloned[capability] = evidence
	}
	return cloned
}

func runtimeCapabilitySupported(vendor config.AgentVendor, capability RuntimeCapability) bool {
	adapter, ok := runtimeAdapterFor(vendor)
	return ok && adapter.contract.Supports(capability)
}
