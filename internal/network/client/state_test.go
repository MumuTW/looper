package client

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestValidateStateAcceptsJoinAuthority(t *testing.T) {
	t.Parallel()
	state := LocalState{URL: "https://loopernet.example.test", NetworkID: "net_1", NodeID: "node_1", NodeName: "worker-1", NodeToken: "node_token", GitHub: protocol.GitHubIdentity{Login: "octocat", NumericID: 42}}
	if err := ValidateState(state); err != nil {
		t.Fatalf("ValidateState() error = %v", err)
	}
}

func TestValidateStateRejectsMissingNodeToken(t *testing.T) {
	t.Parallel()
	state := LocalState{URL: "https://loopernet.example.test", NetworkID: "net_1", NodeID: "node_1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{Login: "octocat", NumericID: 42}}
	if err := ValidateState(state); err == nil || !strings.Contains(err.Error(), "node token") {
		t.Fatalf("ValidateState() error = %v", err)
	}
}
