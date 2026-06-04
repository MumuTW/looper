package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/spf13/cobra"
)

func lookPathFor(installed ...string) config.LookPathFunc {
	set := map[string]struct{}{}
	for _, name := range installed {
		set[name] = struct{}{}
	}
	return func(name string) (string, error) {
		if _, ok := set[name]; ok {
			return "/usr/local/bin/" + name, nil
		}
		return "", fmt.Errorf("%s not found", name)
	}
}

func TestDetectInstalledVendors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		installed []string
		want      []config.AgentVendor
	}{
		{name: "none", installed: nil, want: []config.AgentVendor{}},
		{name: "claude only", installed: []string{"claude"}, want: []config.AgentVendor{config.AgentVendorClaudeCode}},
		{name: "codex and opencode", installed: []string{"codex", "opencode"}, want: []config.AgentVendor{config.AgentVendorCodex, config.AgentVendorOpenCode}},
		{name: "cursor agent binary not auto-detected", installed: []string{"agent"}, want: []config.AgentVendor{}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := detectInstalledVendors(lookPathFor(tt.installed...))
			if len(got) != len(tt.want) {
				t.Fatalf("detectInstalledVendors() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("detectInstalledVendors()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTakeoverScopingRoles(t *testing.T) {
	t.Parallel()

	withoutMerge := takeoverScopingRoles(false)
	for name, got := range map[string]*bool{
		"planner":  withoutMerge.Planner.AutoDiscovery,
		"worker":   withoutMerge.Worker.AutoDiscovery,
		"fixer":    withoutMerge.Fixer.AutoDiscovery,
		"reviewer": withoutMerge.Reviewer.Discovery.AutoDiscovery,
	} {
		if got == nil || *got {
			t.Fatalf("%s autoDiscovery = %v, want false", name, got)
		}
	}
	if withoutMerge.Reviewer.AutoMerge != nil {
		t.Fatalf("auto-merge should be unset when merge=false")
	}

	withMerge := takeoverScopingRoles(true)
	if withMerge.Reviewer.AutoMerge == nil || withMerge.Reviewer.AutoMerge.Enabled == nil || !*withMerge.Reviewer.AutoMerge.Enabled {
		t.Fatalf("auto-merge should be enabled when merge=true, got %+v", withMerge.Reviewer.AutoMerge)
	}
}

func newTakeoverRuntime(t *testing.T, configPath string, deps Deps) *commandRuntime {
	t.Helper()
	if deps.Stdout == nil {
		deps.Stdout = &bytes.Buffer{}
	}
	if deps.Stderr == nil {
		deps.Stderr = &bytes.Buffer{}
	}
	return newCommandRuntime(New(deps), []string{"--config", configPath})
}

func newTakeoverCmd(t *testing.T, vendor string, yes bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().String("agent-vendor", "", "")
	cmd.Flags().Bool("merge", false, "")
	cmd.Flags().Bool("no-fix", false, "")
	cmd.Flags().Bool("yes", false, "")
	cmd.Flags().Bool("json", false, "")
	if vendor != "" {
		if err := cmd.Flags().Set("agent-vendor", vendor); err != nil {
			t.Fatalf("set agent-vendor: %v", err)
		}
	}
	if yes {
		if err := cmd.Flags().Set("yes", "true"); err != nil {
			t.Fatalf("set yes: %v", err)
		}
	}
	return cmd
}

func TestResolveTakeoverVendor(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Parallel()
		r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor()})
		cmd := newTakeoverCmd(t, "codex", false)
		vendor, _, err := r.resolveTakeoverVendor(cmd, takeoverOptions{AgentVendor: "codex"})
		if err != nil {
			t.Fatalf("resolveTakeoverVendor() error = %v", err)
		}
		if vendor != config.AgentVendorCodex {
			t.Fatalf("vendor = %q, want codex", vendor)
		}
	})

	t.Run("invalid flag", func(t *testing.T) {
		t.Parallel()
		r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor()})
		cmd := newTakeoverCmd(t, "bogus", false)
		if _, _, err := r.resolveTakeoverVendor(cmd, takeoverOptions{AgentVendor: "bogus"}); err == nil {
			t.Fatal("resolveTakeoverVendor() error = nil, want unsupported vendor error")
		}
	})

	t.Run("single detected", func(t *testing.T) {
		t.Parallel()
		r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor("claude")})
		cmd := newTakeoverCmd(t, "", false)
		vendor, note, err := r.resolveTakeoverVendor(cmd, takeoverOptions{})
		if err != nil {
			t.Fatalf("resolveTakeoverVendor() error = %v", err)
		}
		if vendor != config.AgentVendorClaudeCode {
			t.Fatalf("vendor = %q, want claude-code", vendor)
		}
		if note == "" {
			t.Fatal("expected a detection note")
		}
	})

	t.Run("yes with none detected fails", func(t *testing.T) {
		t.Parallel()
		r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor()})
		cmd := newTakeoverCmd(t, "", true)
		if _, _, err := r.resolveTakeoverVendor(cmd, takeoverOptions{Yes: true}); err == nil {
			t.Fatal("resolveTakeoverVendor() error = nil, want no-agent error")
		}
	})

	t.Run("yes with multiple detected fails", func(t *testing.T) {
		t.Parallel()
		r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor("claude", "codex")})
		cmd := newTakeoverCmd(t, "", true)
		if _, _, err := r.resolveTakeoverVendor(cmd, takeoverOptions{Yes: true}); err == nil {
			t.Fatal("resolveTakeoverVendor() error = nil, want ambiguous-agent error")
		}
	})
}

func TestEnsureTakeoverConfigCreatesScopedProject(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	repoRoot := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	r := newTakeoverRuntime(t, configPath, Deps{})

	changed, err := r.ensureTakeoverConfig(configPath, cwd, repoRoot, config.AgentVendorClaudeCode, true)
	if err != nil {
		t.Fatalf("ensureTakeoverConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for new config")
	}

	partial, present, err := config.ReadPartialConfigFile(configPath)
	if err != nil || !present {
		t.Fatalf("read config: present=%v err=%v", present, err)
	}
	if partial.Agent == nil || partial.Agent.Vendor == nil || *partial.Agent.Vendor != config.AgentVendorClaudeCode {
		t.Fatalf("agent vendor not set: %+v", partial.Agent)
	}
	if partial.Projects == nil || len(*partial.Projects) != 1 {
		t.Fatalf("want exactly one project, got %+v", partial.Projects)
	}
	project := (*partial.Projects)[0]
	if !samePath(project.RepoPath, repoRoot) {
		t.Fatalf("project repoPath = %q, want %q", project.RepoPath, repoRoot)
	}
	if project.Roles == nil || project.Roles.Reviewer == nil || project.Roles.Reviewer.Discovery == nil ||
		project.Roles.Reviewer.Discovery.AutoDiscovery == nil || *project.Roles.Reviewer.Discovery.AutoDiscovery {
		t.Fatalf("reviewer auto-discovery should be disabled: %+v", project.Roles)
	}
	if project.Roles.Reviewer.AutoMerge == nil || project.Roles.Reviewer.AutoMerge.Enabled == nil || !*project.Roles.Reviewer.AutoMerge.Enabled {
		t.Fatalf("auto-merge should be enabled with --merge: %+v", project.Roles.Reviewer.AutoMerge)
	}

	// Re-running with identical inputs must be a no-op (no spurious restart).
	changedAgain, err := r.ensureTakeoverConfig(configPath, cwd, repoRoot, config.AgentVendorClaudeCode, true)
	if err != nil {
		t.Fatalf("ensureTakeoverConfig() second call error = %v", err)
	}
	if changedAgain {
		t.Fatal("changed = true on identical re-run, want false")
	}
}

func TestResolveTakeoverTarget(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")

	t.Run("explicit fully qualified ref", func(t *testing.T) {
		t.Parallel()
		r := newTakeoverRuntime(t, configPath, Deps{})
		repo, pr, err := r.resolveTakeoverTarget(context.Background(), "acme/looper#42", t.TempDir())
		if err != nil {
			t.Fatalf("resolveTakeoverTarget() error = %v", err)
		}
		if repo != "acme/looper" || pr != 42 {
			t.Fatalf("got %s#%d, want acme/looper#42", repo, pr)
		}
	})

	t.Run("detect from current branch", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			LookPath: lookPathFor("gh", "git"),
			RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
				if len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
					return commandExecutionResult{Stdout: "acme/looper\n"}, nil
				}
				if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
					return commandExecutionResult{Stdout: "57\n"}, nil
				}
				return commandExecutionResult{ExitCode: 1, Stderr: "unexpected"}, nil
			},
		}
		r := newTakeoverRuntime(t, configPath, deps)
		repo, pr, err := r.resolveTakeoverTarget(context.Background(), "", t.TempDir())
		if err != nil {
			t.Fatalf("resolveTakeoverTarget() error = %v", err)
		}
		if repo != "acme/looper" || pr != 57 {
			t.Fatalf("got %s#%d, want acme/looper#57", repo, pr)
		}
	})

	t.Run("no PR for branch", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			LookPath: lookPathFor("gh", "git"),
			RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
				if len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
					return commandExecutionResult{Stdout: "acme/looper\n"}, nil
				}
				return commandExecutionResult{ExitCode: 1, Stderr: "no pull requests found"}, nil
			},
		}
		r := newTakeoverRuntime(t, configPath, deps)
		if _, _, err := r.resolveTakeoverTarget(context.Background(), "", t.TempDir()); err == nil {
			t.Fatal("resolveTakeoverTarget() error = nil, want missing-PR error")
		}
	})
}

func TestEnsureTakeoverConfigDoesNotClobberExistingProjects(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	repoRoot := t.TempDir()
	otherRepo := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.toml")

	existingVendor := config.AgentVendorCodex
	raw, err := config.MarshalConfigFile(configPath, config.PartialConfig{
		Agent:    &config.PartialAgentConfig{Vendor: &existingVendor},
		Projects: &[]config.PartialProjectRefConfig{{ID: "other", Name: "Other", RepoPath: otherRepo}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	r := newTakeoverRuntime(t, configPath, Deps{})
	if _, err := r.ensureTakeoverConfig(configPath, cwd, repoRoot, config.AgentVendorClaudeCode, false); err != nil {
		t.Fatalf("ensureTakeoverConfig() error = %v", err)
	}

	partial, _, err := config.ReadPartialConfigFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if partial.Agent == nil || partial.Agent.Vendor == nil || *partial.Agent.Vendor != config.AgentVendorCodex {
		t.Fatalf("existing agent vendor should be preserved, got %+v", partial.Agent)
	}
	if partial.Projects == nil || len(*partial.Projects) != 2 {
		t.Fatalf("want existing + new project, got %+v", partial.Projects)
	}
	foundOther, foundNew := false, false
	for _, p := range *partial.Projects {
		if samePath(p.RepoPath, otherRepo) {
			foundOther = true
			if p.Roles != nil {
				t.Fatalf("existing project should be untouched, got roles %+v", p.Roles)
			}
		}
		if samePath(p.RepoPath, repoRoot) {
			foundNew = true
		}
	}
	if !foundOther || !foundNew {
		t.Fatalf("foundOther=%v foundNew=%v", foundOther, foundNew)
	}
}
