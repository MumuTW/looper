package e2e

import (
	"os"
	"testing"

	"github.com/MumuTW/looper/internal/e2e/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.RunTestMain(m))
}
