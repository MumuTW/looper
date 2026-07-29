//go:build !darwin && !linux

package worker

import (
	"fmt"
	"os"
)

func openSpecFileBeneath(_, _ string) (*os.File, error) {
	return nil, fmt.Errorf("secure spec file reads are unsupported on this platform")
}
