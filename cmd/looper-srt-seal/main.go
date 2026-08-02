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
	canonicalPackageRoot, err := filepath.EvalSymlinks(filepath.Clean(*packageRoot))
	if err != nil {
		return fmt.Errorf("resolve package-root: %w", err)
	}
	expectedManifest := trustmanifest.ManifestPath(canonicalPackageRoot)
	providedManifest := filepath.Clean(*manifestPath)
	providedParent, err := filepath.EvalSymlinks(filepath.Dir(providedManifest))
	if err != nil {
		return fmt.Errorf("resolve manifest parent: %w", err)
	}
	if filepath.Base(providedManifest) != filepath.Base(expectedManifest) || providedParent != filepath.Dir(expectedManifest) {
		return fmt.Errorf("manifest path must resolve to %s", expectedManifest)
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
	if err := trustmanifest.Write(expectedManifest, trustmanifest.Input{
		PackageRoot: canonicalPackageRoot,
		Roots:       roots,
		LaunchPath:  filepath.SplitList(os.Getenv("PATH")),
	}); err != nil {
		return err
	}
	if err := trustmanifest.VerifyRootOwnership(expectedManifest); err != nil {
		return err
	}
	info, err := os.Stat(expectedManifest)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o644 {
		return fmt.Errorf("manifest mode is %o, want 644", info.Mode().Perm())
	}
	return nil
}
