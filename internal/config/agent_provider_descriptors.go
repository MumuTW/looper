package config

// AgentProviderDescriptor is compiled, reviewed metadata for one supported
// executor. It is deliberately limited to non-secret discovery information:
// callers can find and probe a binary or report credential-key presence, but
// cannot import a provider's configuration, endpoints, plugins, or secrets.
type AgentProviderDescriptor struct {
	Vendor               AgentVendor
	ExecutableCandidates []string
	VersionArgs          []string
	ModelFlags           []string
	CredentialEnvKeys    []string
}

var agentProviderDescriptors = []AgentProviderDescriptor{
	{Vendor: AgentVendorClaudeCode, ExecutableCandidates: []string{"claude"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model"}, CredentialEnvKeys: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}},
	{Vendor: AgentVendorCodex, ExecutableCandidates: []string{"codex"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model", "-m"}, CredentialEnvKeys: []string{"OPENAI_API_KEY"}},
	{Vendor: AgentVendorOpenCode, ExecutableCandidates: []string{"opencode"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model", "-m"}, CredentialEnvKeys: []string{"OPENCODE_API_KEY"}},
	{Vendor: AgentVendorCursorCLI, ExecutableCandidates: []string{"agent"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model"}, CredentialEnvKeys: []string{"CURSOR_API_KEY"}},
	{Vendor: AgentVendorGrokBuild, ExecutableCandidates: []string{"grok"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model", "-m"}, CredentialEnvKeys: []string{"XAI_API_KEY"}},
	{Vendor: AgentVendorDevinExperimental, ExecutableCandidates: []string{"devin"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model"}, CredentialEnvKeys: []string{"DEVIN_API_KEY"}},
	{Vendor: AgentVendorHermes, ExecutableCandidates: []string{"hermes"}, VersionArgs: []string{"--version"}, ModelFlags: []string{"--model", "-m"}, CredentialEnvKeys: []string{"HERMES_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"}},
}

// AgentProviderDescriptors returns a deep copy so consumers cannot mutate the
// compiled registry and silently change what discovery claims is supported.
func AgentProviderDescriptors() []AgentProviderDescriptor {
	out := make([]AgentProviderDescriptor, len(agentProviderDescriptors))
	for i, descriptor := range agentProviderDescriptors {
		out[i] = descriptor
		out[i].ExecutableCandidates = append([]string(nil), descriptor.ExecutableCandidates...)
		out[i].VersionArgs = append([]string(nil), descriptor.VersionArgs...)
		out[i].ModelFlags = append([]string(nil), descriptor.ModelFlags...)
		out[i].CredentialEnvKeys = append([]string(nil), descriptor.CredentialEnvKeys...)
	}
	return out
}

// AgentProviderDescriptorFor finds compiled discovery metadata for a supported
// executor identity.
func AgentProviderDescriptorFor(vendor AgentVendor) (AgentProviderDescriptor, bool) {
	for _, descriptor := range agentProviderDescriptors {
		if descriptor.Vendor == vendor {
			descriptor.ExecutableCandidates = append([]string(nil), descriptor.ExecutableCandidates...)
			descriptor.VersionArgs = append([]string(nil), descriptor.VersionArgs...)
			descriptor.ModelFlags = append([]string(nil), descriptor.ModelFlags...)
			descriptor.CredentialEnvKeys = append([]string(nil), descriptor.CredentialEnvKeys...)
			return descriptor, true
		}
	}
	return AgentProviderDescriptor{}, false
}
