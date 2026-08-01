package config

// AgentProviderDescriptor is reviewed, non-secret metadata for one supported
// executor. It is discovery input, never execution or configuration authority.
// Candidate commands are fixed: user plugins and custom provider definitions
// cannot extend this registry.
type AgentProviderDescriptor struct {
	Vendor            AgentVendor
	Executable        string
	VersionArgs       []string
	ModelFlag         string
	CredentialEnvKeys []string
}

var agentProviderDescriptors = []AgentProviderDescriptor{
	{Vendor: AgentVendorClaudeCode, Executable: "claude", VersionArgs: []string{"--version"}, ModelFlag: "--model", CredentialEnvKeys: []string{"ANTHROPIC_API_KEY"}},
	{Vendor: AgentVendorCodex, Executable: "codex", VersionArgs: []string{"--version"}, ModelFlag: "--model", CredentialEnvKeys: []string{"OPENAI_API_KEY"}},
	{Vendor: AgentVendorOpenCode, Executable: "opencode", VersionArgs: []string{"--version"}, ModelFlag: "--model"},
	{Vendor: AgentVendorCursorCLI, Executable: "agent", VersionArgs: []string{"--version"}, ModelFlag: "--model"},
	{Vendor: AgentVendorGrokBuild, Executable: "grok", VersionArgs: []string{"--version"}, ModelFlag: "--model", CredentialEnvKeys: []string{"XAI_API_KEY"}},
	{Vendor: AgentVendorDevinExperimental, Executable: "devin", VersionArgs: []string{"--version"}, ModelFlag: "--model", CredentialEnvKeys: []string{"DEVIN_API_KEY"}},
	// Hermes intentionally declares no general credential key. Its official
	// CLI supports many provider-specific API keys plus OAuth-backed stores;
	// one environment name would be an inaccurate readiness claim.
	{Vendor: AgentVendorHermes, Executable: "hermes", VersionArgs: []string{"--version"}, ModelFlag: "--model"},
}

// AgentProviderDescriptors returns a detached copy in deterministic order.
func AgentProviderDescriptors() []AgentProviderDescriptor {
	result := make([]AgentProviderDescriptor, len(agentProviderDescriptors))
	for index, descriptor := range agentProviderDescriptors {
		result[index] = descriptor
		result[index].VersionArgs = append([]string(nil), descriptor.VersionArgs...)
		result[index].CredentialEnvKeys = append([]string(nil), descriptor.CredentialEnvKeys...)
	}
	return result
}

func AgentProviderDescriptorFor(vendor AgentVendor) (AgentProviderDescriptor, bool) {
	for _, descriptor := range agentProviderDescriptors {
		if descriptor.Vendor == vendor {
			copy := descriptor
			copy.VersionArgs = append([]string(nil), descriptor.VersionArgs...)
			copy.CredentialEnvKeys = append([]string(nil), descriptor.CredentialEnvKeys...)
			return copy, true
		}
	}
	return AgentProviderDescriptor{}, false
}
