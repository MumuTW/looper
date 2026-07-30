// Command check-binaries rejects committed build artifacts (issue #236): any
// tracked file bearing an executable-binary magic number, or any tracked file
// over the size threshold, fails the build unless its path is explicitly
// allowlisted as an intentional asset. Run from the repository root:
//
//	go run ./tools/check-binaries
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-binaries: git ls-files: %v\n", err)
		os.Exit(2)
	}
	violations := CheckFiles(strings.Split(strings.TrimSpace(string(out)), "\n"), os.DirFS("."))
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, "check-binaries: "+v)
	}
	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "check-binaries: build artifacts belong in dist/ (gitignored); intentional assets go on the allowlist in tools/check-binaries/main.go")
		os.Exit(1)
	}
}
