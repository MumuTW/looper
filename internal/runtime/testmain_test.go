package runtime

import (
	"os"
	"testing"

	"github.com/nexu-io/looper/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunTestMain(m))
}
