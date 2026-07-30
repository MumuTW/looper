package reviewer

import (
	"os"
	"testing"

	"github.com/MumuTW/looper/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunTestMain(m))
}
