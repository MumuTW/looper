package api

import (
	"encoding/json"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestConfigResponseDoesNotAdvertiseUnsupportedAutoUpgrade(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	response := NewHandler(Context{Config: cfg}).buildConfigResponse()
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Package map[string]any `json:"package"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, found := decoded.Package["autoUpgradeEnabled"]; found {
		t.Fatalf("package response advertises autoUpgradeEnabled: %s", encoded)
	}
}
