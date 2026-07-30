package config

import "testing"

func TestAgentProviderDescriptorsCoverEveryConfigurableVendor(t *testing.T) {
	t.Parallel()
	for _, vendor := range ConfigurableAgentVendors() {
		descriptor, ok := AgentProviderDescriptorFor(vendor)
		if !ok {
			t.Errorf("AgentProviderDescriptorFor(%q) = missing", vendor)
			continue
		}
		if len(descriptor.ExecutableCandidates) == 0 || len(descriptor.VersionArgs) == 0 {
			t.Errorf("descriptor for %q lacks executable candidates or version probe: %#v", vendor, descriptor)
		}
	}
}

func TestAgentProviderDescriptorsReturnDeepCopies(t *testing.T) {
	t.Parallel()
	first := AgentProviderDescriptors()
	first[0].ExecutableCandidates[0] = "mutated"
	first[0].CredentialEnvKeys[0] = "MUTATED_SECRET"
	second := AgentProviderDescriptors()
	if second[0].ExecutableCandidates[0] == "mutated" || second[0].CredentialEnvKeys[0] == "MUTATED_SECRET" {
		t.Fatalf("registry was mutated through returned descriptors: %#v", second[0])
	}
}
