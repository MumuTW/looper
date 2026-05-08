package workflowpolicy

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/powerformer/looper/internal/config"
)

//go:embed builtin/*.json
var builtinPacks embed.FS

type Pack struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Version int                   `json:"version"`
	Roles   map[string]RolePolicy `json:"roles"`
}

type RolePolicy struct {
	Instructions string `json:"instructions"`
}

type Block struct {
	Enabled      bool   `json:"enabled"`
	Role         string `json:"role"`
	ProjectID    string `json:"projectId,omitempty"`
	PackID       string `json:"packId,omitempty"`
	PackName     string `json:"packName,omitempty"`
	Instructions string `json:"-"`
}

func ResolveBlock(cfg config.Config, projectID, role string) (Block, error) {
	block := Block{Enabled: cfg.WorkflowPolicyPacks.Enabled, Role: role, ProjectID: projectID}
	if !cfg.WorkflowPolicyPacks.Enabled {
		return block, nil
	}
	packID := strings.TrimSpace(rolePolicyPack(config.ProjectRoleConfigs(cfg, projectID), role))
	if packID == "" {
		return block, nil
	}
	pack, err := LoadPack(cfg, packID)
	if err != nil {
		return block, err
	}
	policy, ok := pack.Roles[role]
	if !ok || strings.TrimSpace(policy.Instructions) == "" {
		return block, nil
	}
	block.PackID = pack.ID
	block.PackName = pack.Name
	block.Instructions = fmt.Sprintf("Workflow policy pack: %s (%s)\n%s", pack.ID, pack.Name, strings.TrimSpace(policy.Instructions))
	return block, nil
}

func LoadPack(cfg config.Config, id string) (Pack, error) {
	var ref *config.WorkflowPolicyPackRef
	for index := range cfg.WorkflowPolicyPacks.Packs {
		if cfg.WorkflowPolicyPacks.Packs[index].ID == id {
			ref = &cfg.WorkflowPolicyPacks.Packs[index]
			break
		}
	}
	if ref == nil {
		return Pack{}, fmt.Errorf("workflow policy pack %q is not configured", id)
	}
	pack, err := loadPackRef(*ref)
	if err != nil {
		return Pack{}, err
	}
	if err := ValidatePack(pack, cfg.Instructions.MaxBytes); err != nil {
		return Pack{}, err
	}
	if pack.ID != ref.ID {
		return Pack{}, fmt.Errorf("workflow policy pack %q loaded pack id %q", ref.ID, pack.ID)
	}
	if strings.TrimSpace(ref.Name) != "" && pack.Name != ref.Name {
		pack.Name = ref.Name
	}
	return pack, nil
}

func loadPackRef(ref config.WorkflowPolicyPackRef) (Pack, error) {
	var raw []byte
	var err error
	switch ref.Source {
	case config.WorkflowPolicyPackSourceBuiltin:
		raw, err = builtinPacks.ReadFile(path.Join("builtin", ref.ID+".json"))
		if err != nil {
			return Pack{}, fmt.Errorf("load builtin workflow policy pack %q: %w", ref.ID, err)
		}
	case config.WorkflowPolicyPackSourceFile:
		raw, err = os.ReadFile(ref.Path)
		if err != nil {
			return Pack{}, fmt.Errorf("load workflow policy pack file %q: %w", ref.Path, err)
		}
	default:
		return Pack{}, fmt.Errorf("unsupported workflow policy pack source %q", ref.Source)
	}
	var pack Pack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return Pack{}, fmt.Errorf("decode workflow policy pack %q: %w", ref.ID, err)
	}
	return pack, nil
}

func ValidatePack(pack Pack, maxBytes int) error {
	var issues []string
	if strings.TrimSpace(pack.ID) == "" {
		issues = append(issues, "id is required")
	}
	if strings.TrimSpace(pack.Name) == "" {
		issues = append(issues, "name is required")
	}
	if pack.Version < 1 {
		issues = append(issues, "version must be a positive integer")
	}
	if len(pack.Roles) == 0 {
		issues = append(issues, "roles must contain at least one role")
	}
	for role, policy := range pack.Roles {
		if !isRole(role) {
			issues = append(issues, fmt.Sprintf("roles.%s is not a supported role", role))
			continue
		}
		text := strings.TrimSpace(policy.Instructions)
		if text == "" {
			issues = append(issues, fmt.Sprintf("roles.%s.instructions is required", role))
			continue
		}
		if maxBytes > 0 && len([]byte(text)) > maxBytes {
			issues = append(issues, fmt.Sprintf("roles.%s.instructions must be at most %d bytes", role, maxBytes))
		}
		if protected := protectedPhrase(text); protected != "" {
			issues = append(issues, fmt.Sprintf("roles.%s.instructions must not override protected Looper contract %q", role, protected))
		}
	}
	if len(issues) > 0 {
		return fmt.Errorf("workflow policy pack %q validation failed: %s", pack.ID, strings.Join(issues, "; "))
	}
	return nil
}

func rolePolicyPack(roles config.RoleConfigs, role string) string {
	switch role {
	case "planner":
		return roles.Planner.PolicyPack
	case "worker":
		return roles.Worker.PolicyPack
	case "reviewer":
		return roles.Reviewer.PolicyPack
	case "fixer":
		return roles.Fixer.PolicyPack
	default:
		return ""
	}
}

func isRole(role string) bool {
	switch role {
	case "planner", "worker", "reviewer", "fixer":
		return true
	default:
		return false
	}
}

func protectedPhrase(text string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
	protected := []string{"systemprompt", "system prompt", "__looper_result__", "completion marker", "git_pr_lifecycle", "summary field", "commits field", "result json", "allowautopush", "allowautoapprove", "allow auto push", "allow auto approve", "auto approve", "auto push", "pr creation policy", "review submission policy", "looper review submit", "review submit wrapper", "gh pr review", "disclosure stamping", "auth requirement", "permission boundary", "state transition", "state machine", "ignore lifecycle", "override lifecycle", "custom completion"}
	for _, phrase := range protected {
		if strings.Contains(normalized, phrase) {
			return phrase
		}
	}
	return ""
}
