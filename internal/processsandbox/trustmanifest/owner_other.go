//go:build !darwin && !linux

package trustmanifest

import (
	"fmt"
	"os"
)

func openSealedFile(path string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func fileOwnerUID(os.FileInfo) (uint32, error) {
	return 0, fmt.Errorf("sealed trust manifests are unsupported on this platform")
}
