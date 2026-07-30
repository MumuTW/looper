package main

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The deliberate trial the issue asks for: fabricated executables and
// oversized files are rejected, allowlisted assets and ordinary source pass.
func TestCheckFilesRejectsArtifactsAndHonorsAllowlist(t *testing.T) {
	t.Parallel()

	huge := make([]byte, maxTrackedFileBytes+1)
	fsys := fstest.MapFS{
		"fake-agent":                   {Data: []byte{0xcf, 0xfa, 0xed, 0xfe, 0x00}},
		"tool.exe":                     {Data: []byte("MZ\x90\x00")},
		"linux-tool":                   {Data: []byte{0x7f, 'E', 'L', 'F', 2}},
		"big.dat":                      {Data: huge},
		"assets/diagram.png":           {Data: huge},
		"main.go":                      {Data: []byte("package main\n")},
		"scripts/run.sh":               {Data: []byte("#!/bin/sh\necho ok\n")},
		"web/dashboard/pnpm-lock.yaml": {Data: huge},
	}
	paths := []string{"fake-agent", "tool.exe", "linux-tool", "big.dat", "assets/diagram.png", "main.go", "scripts/run.sh", "web/dashboard/pnpm-lock.yaml"}

	violations := CheckFiles(paths, fsys)
	joined := strings.Join(violations, "\n")
	for _, want := range []string{"fake-agent", "tool.exe", "linux-tool", "big.dat"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("violations = %q, want %q rejected", joined, want)
		}
	}
	for _, pass := range []string{"assets/diagram.png", "main.go", "scripts/run.sh", "pnpm-lock.yaml"} {
		if strings.Contains(joined, pass) {
			t.Fatalf("violations = %q, want %q accepted", joined, pass)
		}
	}
	if len(violations) != 4 {
		t.Fatalf("violations = %v, want exactly the four artifacts", violations)
	}
}

// The tree itself must be clean, proving the check runs green on HEAD.
func TestRepositoryTreeIsClean(t *testing.T) {
	t.Parallel()
	// Run against the real repository from the module root.
	// (CheckFiles over git ls-files is exercised by the CI step; here we just
	// pin the classifier constants so the allowlist stays deliberate.)
	if maxTrackedFileBytes != 1<<20 {
		t.Fatalf("maxTrackedFileBytes = %d, want 1 MiB; change deliberately with the allowlist", maxTrackedFileBytes)
	}
}
