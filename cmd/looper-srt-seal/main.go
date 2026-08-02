// looper-srt-seal is the root-only install helper for the process-sandbox
// executable-closure manifest.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/MumuTW/looper/internal/processsandbox/trustmanifest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "looper-srt-seal: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("looper-srt-seal", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	manifestPath := flags.String("manifest", "", "absolute output manifest path")
	packageRoot := flags.String("package-root", "", "absolute complete runtime node_modules root")
	srt := flags.String("srt", "", "srt launcher path")
	node := flags.String("node", "", "node executable path")
	rg := flags.String("rg", "", "ripgrep executable path")
	bwrap := flags.String("bwrap", "", "bubblewrap executable path")
	socat := flags.String("socat", "", "socat executable path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root so the manifest is root-owned")
	}
	if !filepath.IsAbs(*manifestPath) || !filepath.IsAbs(*packageRoot) {
		return fmt.Errorf("manifest and package-root must be absolute")
	}
	if filepath.Clean(*manifestPath) != filepath.Clean(trustmanifest.ManifestPath(*packageRoot)) {
		return fmt.Errorf("manifest path must be %s", trustmanifest.ManifestPath(*packageRoot))
	}
	roots := map[string]string{"srt": *srt, "node": *node, "rg": *rg}
	if runtime.GOOS == "linux" {
		roots["bwrap"] = *bwrap
		roots["socat"] = *socat
	}
	for name, path := range roots {
		path = strings.TrimSpace(path)
		if path == "" {
			return fmt.Errorf("%s path is required", name)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s path must be absolute", name)
		}
	}
	if err := trustmanifest.Write(*manifestPath, trustmanifest.Input{PackageRoot: *packageRoot, Roots: roots}); err != nil {
		return err
	}
	info, err := os.Stat(*manifestPath)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o644 {
		return fmt.Errorf("manifest mode is %o, want 644", info.Mode().Perm())
	}
	return nil
}
