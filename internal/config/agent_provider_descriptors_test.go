package config

import (
	"reflect"
	"testing"
)

func TestAgentProviderDescriptorsCoverConfigurableVendors(t *testing.T) {
	t.Parallel()
	descriptors := AgentProviderDescriptors()
	got := make([]AgentVendor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		got = append(got, descriptor.Vendor)
		if descriptor.Executable == "" || len(descriptor.VersionArgs) == 0 {
			t.Fatalf("descriptor = %#v, want fixed executable and version probe", descriptor)
		}
	}
	if want := ConfigurableAgentVendors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor vendors = %#v, want configurable roster %#v", got, want)
	}
}

func TestHermesDescriptorDoesNotInventCredentialReadiness(t *testing.T) {
	t.Parallel()
	descriptor, ok := AgentProviderDescriptorFor(AgentVendorHermes)
	if !ok {
		t.Fatal("Hermes descriptor missing")
	}
	if descriptor.Executable != "hermes" || !reflect.DeepEqual(descriptor.VersionArgs, []string{"--version"}) {
		t.Fatalf("Hermes descriptor = %#v", descriptor)
	}
	if len(descriptor.CredentialEnvKeys) != 0 {
		t.Fatalf("Hermes credential keys = %#v, want none: Hermes has provider-specific and OAuth stores", descriptor.CredentialEnvKeys)
	}
}
