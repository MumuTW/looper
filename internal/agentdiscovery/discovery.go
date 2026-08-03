// Package agentdiscovery produces advisory executor suggestions. Discovery is
// never configuration authority: only the existing revision-bound config
// patch endpoint may apply a suggestion.
package agentdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/processsandbox"
)

const ProbeTimeout = 2 * time.Second

type Candidate struct {
	Vendor            config.AgentVendor `json:"vendor"`
	Executable        string             `json:"executable,omitempty"`
	Status            string             `json:"status"`
	Version           string             `json:"version,omitempty"`
	Diagnostic        string             `json:"diagnostic,omitempty"`
	CredentialEnvKeys []string           `json:"credentialEnvKeys,omitempty"`
}

type Suggestion struct {
	Set map[string]json.RawMessage `json:"set"`
}

type Report struct {
	ConfigRevision string      `json:"configRevision,omitempty"`
	Candidates     []Candidate `json:"candidates"`
	Suggestion     *Suggestion `json:"suggestion,omitempty"`
}

type Probe func(context.Context, config.AgentProviderDescriptor, string) (string, error)

type Dependencies struct {
	LookPath     func(string) (string, error)
	EvalSymlinks func(string) (string, error)
	Probe        Probe
}

func Discover(ctx context.Context, cfg config.Config, dependencies Dependencies) Report {
	if dependencies.LookPath == nil {
		dependencies.LookPath = exec.LookPath
	}
	if dependencies.EvalSymlinks == nil {
		dependencies.EvalSymlinks = filepath.EvalSymlinks
	}
	if dependencies.Probe == nil {
		dependencies.Probe = RestrictedVersionProbe
	}

	report := Report{Candidates: make([]Candidate, 0, len(config.AgentProviderDescriptors()))}
	hermesReady := false
	for _, descriptor := range config.AgentProviderDescriptors() {
		candidate := Candidate{
			Vendor:            descriptor.Vendor,
			Status:            "missing",
			CredentialEnvKeys: append([]string(nil), descriptor.CredentialEnvKeys...),
		}
		path, err := dependencies.LookPath(descriptor.Executable)
		if err != nil {
			candidate.Diagnostic = "executable not found on the daemon PATH"
			report.Candidates = append(report.Candidates, candidate)
			continue
		}
		if resolved, resolveErr := dependencies.EvalSymlinks(path); resolveErr == nil {
			path = resolved
		}
		if absolute, absoluteErr := filepath.Abs(path); absoluteErr == nil {
			path = absolute
		}
		candidate.Executable = filepath.Clean(path)
		if ctx.Err() != nil {
			candidate.Status = "cancelled"
			candidate.Diagnostic = "discovery cancelled before probe"
			report.Candidates = append(report.Candidates, candidate)
			continue
		}
		version, probeErr := dependencies.Probe(ctx, descriptor, candidate.Executable)
		if probeErr != nil {
			candidate.Status, candidate.Diagnostic = probeFailure(probeErr)
			report.Candidates = append(report.Candidates, candidate)
			continue
		}
		candidate.Status = "ready"
		candidate.Version = version
		report.Candidates = append(report.Candidates, candidate)
		if descriptor.Vendor == config.AgentVendorHermes {
			hermesReady = true
		}
	}

	if hermesReady && hermesSuggestionAllowed(cfg) {
		report.Suggestion = &Suggestion{Set: map[string]json.RawMessage{
			"agent.vendor": json.RawMessage(`"hermes"`),
		}}
	}
	return report
}

func hermesSuggestionAllowed(cfg config.Config) bool {
	if cfg.Agent.Vendor != nil || cfg.Agent.Model != nil || cfg.Agent.ReasoningEffort != nil || len(cfg.Agent.Profiles) > 0 || len(cfg.Agent.Params) > 0 || len(cfg.Agent.Env) > 0 {
		return false
	}
	for _, binding := range []*config.RoleAgentConfig{
		cfg.Roles.Planner.Agent,
		cfg.Roles.Worker.Agent,
		cfg.Roles.Reviewer.Agent,
		cfg.Roles.Fixer.Agent,
	} {
		if binding != nil {
			return false
		}
	}
	return true
}

var versionPattern = regexp.MustCompile(`\b\d+\.\d+(?:\.\d+)?\b`)

// RestrictedVersionProbe executes a fixed descriptor probe behind the same
// credential-free, no-network, read-only process boundary used for untrusted
// repository assessment. The binary may write only to the sandbox temp root;
// its real HOME, provider stores, plugins, and daemon credentials are unreadable.
func RestrictedVersionProbe(ctx context.Context, descriptor config.AgentProviderDescriptor, executable string) (string, error) {
	info, err := os.Stat(executable)
	if err != nil {
		return "", fmt.Errorf("inspect executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("candidate is not an executable regular file")
	}
	workingDirectory, err := os.MkdirTemp("", "looper-provider-probe-")
	if err != nil {
		return "", fmt.Errorf("create probe workspace: %w", err)
	}
	defer os.RemoveAll(workingDirectory)

	// Most Python/uv installs place the console script under <venv>/bin and
	// import packages from the same venv. Grant that reviewed candidate tree,
	// never HOME or Hermes auth/config roots as a whole.
	readRoot := filepath.Dir(filepath.Dir(executable))
	result, err := processsandbox.Run(ctx, processsandbox.Options{
		CWD:         workingDirectory,
		Command:     executable,
		Args:        append([]string(nil), descriptor.VersionArgs...),
		Timeout:     ProbeTimeout,
		Profile:     processsandbox.ReadOnlyProfile([]string{readRoot}, nil),
		Environment: processsandbox.ToolEnvironment{},
	})
	if err != nil {
		return "", err
	}
	return parseVersionOutput(descriptor, result)
}

func parseVersionOutput(descriptor config.AgentProviderDescriptor, result shell.Result) (string, error) {
	if result.StdoutTruncated || result.StderrTruncated {
		return "", fmt.Errorf("version output exceeded the bounded capture")
	}
	version := strings.Join(strings.Fields(strings.TrimSpace(result.Stdout)), " ")
	if version == "" {
		version = strings.Join(strings.Fields(strings.TrimSpace(result.Stderr)), " ")
	}
	if len(version) > 200 {
		return "", fmt.Errorf("version output exceeded 200 characters")
	}
	if version == "" || !versionPattern.MatchString(version) {
		return "", fmt.Errorf("version output did not identify a version")
	}
	if descriptor.Vendor == config.AgentVendorHermes && !strings.Contains(strings.ToLower(version), "hermes") {
		return "", fmt.Errorf("version output did not identify Hermes")
	}
	return version, nil
}

func probeFailure(err error) (status, diagnostic string) {
	var commandErr *shell.CommandExecutionError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", "probe cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out", "probe exceeded its bounded deadline"
	case errors.As(err, &commandErr) && commandErr.Category == shell.FailureSupervisorTimeout:
		return "timed_out", "probe exceeded its bounded deadline"
	case strings.Contains(err.Error(), "required srt runtime"), strings.Contains(err.Error(), "process sandbox"):
		return "unavailable", "restricted probe boundary is unavailable"
	default:
		return "failed", "restricted version probe failed"
	}
}
