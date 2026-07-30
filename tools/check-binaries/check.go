package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// maxTrackedFileBytes is the size threshold for tracked files that are not
// allowlisted assets. The largest legitimate source file is well under this.
const maxTrackedFileBytes = 1 << 20

// allowedPrefixes are intentional large/binary assets. Everything else that
// looks like an executable or exceeds the threshold is a committed artifact.
var allowedPrefixes = []string{
	"assets/",
	".codex/reports/",
	"web/dashboard/pnpm-lock.yaml",
}

// binaryMagics are the file-signature prefixes of Mach-O (thin, fat, both
// endiannesses), ELF, and PE executables.
var binaryMagics = [][]byte{
	{0xcf, 0xfa, 0xed, 0xfe},
	{0xce, 0xfa, 0xed, 0xfe},
	{0xfe, 0xed, 0xfa, 0xce},
	{0xfe, 0xed, 0xfa, 0xcf},
	{0xca, 0xfe, 0xba, 0xbe},
	{0x7f, 'E', 'L', 'F'},
	{'M', 'Z'},
}

func allowed(path string) bool {
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func isExecutableBinary(header []byte) bool {
	for _, magic := range binaryMagics {
		if bytes.HasPrefix(header, magic) {
			return true
		}
	}
	return false
}

// CheckFiles returns one violation string per offending tracked file.
func CheckFiles(paths []string, fsys fs.FS) []string {
	violations := make([]string, 0)
	for _, path := range paths {
		if strings.TrimSpace(path) == "" || allowed(path) {
			continue
		}
		file, err := fsys.Open(path)
		if err != nil {
			continue
		}
		header := make([]byte, 4)
		n, _ := io.ReadFull(file, header)
		info, statErr := fs.Stat(fsys, path)
		_ = file.Close()
		if isExecutableBinary(header[:n]) {
			violations = append(violations, fmt.Sprintf("%s is a committed executable binary", path))
			continue
		}
		if statErr == nil && info.Size() > maxTrackedFileBytes {
			violations = append(violations, fmt.Sprintf("%s is %d bytes (limit %d)", path, info.Size(), maxTrackedFileBytes))
		}
	}
	return violations
}
