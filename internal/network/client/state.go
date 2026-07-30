package client

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/network/protocol"
)

type LocalState struct {
	URL       string                  `json:"url"`
	NetworkID string                  `json:"networkId"`
	NodeID    string                  `json:"nodeId"`
	NodeName  string                  `json:"nodeName"`
	NodeToken string                  `json:"nodeToken"`
	GitHub    protocol.GitHubIdentity `json:"github"`
}

// ValidateState verifies the durable enrollment authority that a Routed node
// needs before it can participate in loopernet. Config can describe project
// policy, but it cannot substitute for the node id/token issued by Join.
func ValidateState(state LocalState) error {
	parsed, err := url.Parse(strings.TrimSpace(state.URL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("url must be an absolute http(s) URL")
	}
	if strings.TrimSpace(state.NetworkID) == "" || strings.TrimSpace(state.NodeID) == "" || strings.TrimSpace(state.NodeToken) == "" {
		return fmt.Errorf("network id, node id, and node token are required")
	}
	if err := protocol.ValidateNodeName(state.NodeName); err != nil {
		return fmt.Errorf("node name: %w", err)
	}
	if strings.TrimSpace(state.GitHub.Login) == "" || state.GitHub.NumericID <= 0 {
		return fmt.Errorf("GitHub login and positive numeric id are required")
	}
	return nil
}

func DefaultStatePath(homeDir string) string {
	return filepath.Join(homeDir, ".looper", "network.json")
}

func LoadState(path string) (LocalState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return LocalState{}, err
	}
	var state LocalState
	if err := json.Unmarshal(raw, &state); err != nil {
		return LocalState{}, fmt.Errorf("decode network state: %w", err)
	}
	return state, nil
}

func SaveState(path string, state LocalState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create network state directory: %w", err)
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode network state: %w", err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func RemoveState(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
