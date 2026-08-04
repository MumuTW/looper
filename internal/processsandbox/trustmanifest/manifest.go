// Package trustmanifest seals and verifies the executable closure used by
// Looper's process sandbox runtime.
package trustmanifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	manifestVersion     = 2
	ManifestFileName    = ".looper-srt-trust.json"
	closureResolverRoot = "closure-resolver"
	lddTimeout          = 10 * time.Second
)

type EntryKind string

const (
	EntryTreeFile          EntryKind = "tree-file"
	EntryTreeSymlink       EntryKind = "tree-symlink"
	EntryRuntimeScript     EntryKind = "runtime-script"
	EntryELFBinary         EntryKind = "elf-binary"
	EntryMachOBinary       EntryKind = "mach-o-binary"
	EntrySharedLibrary     EntryKind = "shared-library"
	EntryELFInterpreter    EntryKind = "elf-interpreter"
	EntryScriptInterpreter EntryKind = "script-interpreter"
)

// Root binds a logical runtime component to the exact resolved executable
// path that was sealed.
type Root struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Entry records the digest of one executable-closure object. Symlink entries
// hash the link text; every other entry hashes regular-file contents.
type Entry struct {
	Path   string    `json:"path"`
	Kind   EntryKind `json:"kind"`
	SHA256 string    `json:"sha256"`
	Mode   uint32    `json:"mode"`
	Size   int64     `json:"size"`
}

// Manifest is the canonical, content-addressed trust authority installed for
// one SRT package and its support tools.
type Manifest struct {
	Version     int     `json:"version"`
	PackageRoot string  `json:"packageRoot"`
	Roots       []Root  `json:"roots"`
	Entries     []Entry `json:"entries"`
}

// Input names the complete runtime node_modules tree and every executable root
// the sandbox spawn path can use. Roots must include srt and all
// platform-required support tools.
type Input struct {
	PackageRoot string
	Roots       map[string]string
	// LaunchPath is the exact PATH suffix used after the sealed support-tool
	// directories. A nil value uses the current process PATH for library users
	// that do not have a separately constructed launch environment.
	LaunchPath []string
}

// ManifestPath returns the fixed manifest location for a node_modules root.
// The manifest sits at the install prefix rather than inside the sealed tree,
// avoiding a circular digest over the manifest itself.
func ManifestPath(moduleRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(moduleRoot)), ManifestFileName)
}

// Build resolves and hashes the complete executable closure. Seal and Verify
// deliberately share this resolver so their definitions of completeness
// cannot drift.
func Build(input Input) (Manifest, error) {
	normalized, err := normalizeInput(input)
	if err != nil {
		return Manifest{}, err
	}
	collector := closureCollector{
		entries:             make(map[string]Entry),
		visitedELF:          make(map[string]struct{}),
		visitedInterpreters: make(map[string]struct{}),
		interpreterPaths:    executableSearchPaths(normalized.Roots, normalized.LaunchPath),
		elfContextPath:      normalized.Roots["node"],
		visitedMachO:        make(map[string]struct{}),
	}
	if err := collector.addPackageTree(normalized.PackageRoot, normalized.Roots["srt"]); err != nil {
		return Manifest{}, err
	}
	rootNames := make([]string, 0, len(normalized.Roots))
	for name := range normalized.Roots {
		rootNames = append(rootNames, name)
	}
	sort.Strings(rootNames)
	for _, name := range rootNames {
		if err := collector.addExecutable(normalized.Roots[name], name == "srt"); err != nil {
			return Manifest{}, fmt.Errorf("resolve %s closure: %w", name, err)
		}
	}
	manifest := Manifest{Version: manifestVersion, PackageRoot: normalized.PackageRoot}
	for _, name := range rootNames {
		manifest.Roots = append(manifest.Roots, Root{Name: name, Path: normalized.Roots[name]})
	}
	for _, entry := range collector.entries {
		manifest.Entries = append(manifest.Entries, entry)
	}
	sort.Slice(manifest.Entries, func(i, j int) bool {
		if manifest.Entries[i].Path == manifest.Entries[j].Path {
			return manifest.Entries[i].Kind < manifest.Entries[j].Kind
		}
		return manifest.Entries[i].Path < manifest.Entries[j].Path
	})
	return manifest, nil
}

// Write builds and atomically writes a canonical manifest. The caller owns
// installation policy; the production sealer requires euid 0 before calling
// this function.
func Write(path string, input Input) error {
	manifest, err := Build(input)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trust manifest: %w", err)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".looper-srt-trust-*")
	if err != nil {
		return fmt.Errorf("create temporary trust manifest: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return fmt.Errorf("set trust manifest mode: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		return fmt.Errorf("write trust manifest: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync trust manifest: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close trust manifest: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install trust manifest: %w", err)
	}
	removeTemp = false
	return nil
}

// VerifyRootOwnership checks the final manifest through a no-follow descriptor
// so installers fail immediately on root-squashed or otherwise anonymous
// filesystems instead of producing an unusable manifest.
func VerifyRootOwnership(path string) error {
	file, info, err := openSealedFile(path)
	if err != nil {
		return fmt.Errorf("read trust manifest metadata: %w", err)
	}
	defer file.Close()
	if !info.Mode().IsRegular() {
		return fmt.Errorf("trust manifest %s is not a regular file", path)
	}
	owner, err := fileOwnerUID(info)
	if err != nil {
		return err
	}
	if owner != 0 {
		return fmt.Errorf("trust manifest %s is not owned by root", path)
	}
	return nil
}

// Verify loads a root-sealed manifest, verifies every recorded object before
// invoking the closure resolver, then rebuilds the closure and requires exact
// equality. The ordering matters: ldd only observes binaries whose recorded
// bytes have already matched the authority.
//
// Authority: the root-owned manifest and its SHA-256 entries, not ancestor
// writability and not daemon output. This detects substitution of launchers,
// ELF interpreters, and shared libraries. It deliberately does not close the
// verify-to-exec TOCTOU window. On hosts where the daemon user has passwordless
// sudo, root ownership is also not a hard boundary; that user is root-equivalent.
func Verify(path string, input Input) error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("sandboxed execution is not supported as root")
	}
	manifest, err := loadRootSealed(path)
	if err != nil {
		return err
	}
	if err := verifyRecordedContent(manifest); err != nil {
		return err
	}
	if err := verifyInputRoots(manifest, input); err != nil {
		return err
	}
	current, err := Build(input)
	if err != nil {
		return fmt.Errorf("rebuild executable closure: %w", err)
	}
	if err := compareManifests(manifest, current); err != nil {
		return err
	}
	return nil
}

func verifyInputRoots(manifest Manifest, input Input) error {
	normalized, err := normalizeInput(input)
	if err != nil {
		return err
	}
	if manifest.PackageRoot != normalized.PackageRoot {
		return fmt.Errorf("trust manifest package root drift: sealed %s, current %s", manifest.PackageRoot, normalized.PackageRoot)
	}
	names := make([]string, 0, len(normalized.Roots))
	for name := range normalized.Roots {
		names = append(names, name)
	}
	sort.Strings(names)
	current := make([]Root, 0, len(names))
	for _, name := range names {
		current = append(current, Root{Name: name, Path: normalized.Roots[name]})
	}
	if !equalRoots(manifest.Roots, current) {
		return fmt.Errorf("trust manifest executable roots changed")
	}
	return nil
}

func loadRootSealed(path string) (Manifest, error) {
	file, info, err := openSealedFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read sealed trust manifest metadata: %w", err)
	}
	defer file.Close()
	if !info.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("sealed trust manifest %s is not a regular file", path)
	}
	owner, err := fileOwnerUID(info)
	if err != nil {
		return Manifest{}, err
	}
	if owner != 0 {
		return Manifest{}, fmt.Errorf("sealed trust manifest %s is not owned by root", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return Manifest{}, fmt.Errorf("sealed trust manifest %s is group- or world-writable", path)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return Manifest{}, fmt.Errorf("read sealed trust manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode sealed trust manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, fmt.Errorf("decode sealed trust manifest: trailing JSON value")
		}
		return Manifest{}, fmt.Errorf("decode sealed trust manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func verifyRecordedContent(manifest Manifest) error {
	for _, entry := range manifest.Entries {
		digest, mode, size, err := digestEntryMetadata(entry.Path, entry.Kind, entry.Size)
		if err != nil {
			return fmt.Errorf("verify %s: %w", entry.Path, err)
		}
		if mode != entry.Mode || size != entry.Size {
			return fmt.Errorf("executable closure digest mismatch for %s (metadata changed)", entry.Path)
		}
		if digest != entry.SHA256 {
			return fmt.Errorf("executable closure digest mismatch for %s", entry.Path)
		}
	}
	return nil
}

func compareManifests(sealed, current Manifest) error {
	if sealed.Version != current.Version {
		return fmt.Errorf("trust manifest version drift: sealed %d, current %d", sealed.Version, current.Version)
	}
	if sealed.PackageRoot != current.PackageRoot {
		return fmt.Errorf("trust manifest package root drift: sealed %s, current %s", sealed.PackageRoot, current.PackageRoot)
	}
	if !equalRoots(sealed.Roots, current.Roots) {
		return fmt.Errorf("trust manifest executable roots changed")
	}
	if len(sealed.Entries) != len(current.Entries) {
		return fmt.Errorf("trust manifest executable closure changed: sealed %d entries, current %d", len(sealed.Entries), len(current.Entries))
	}
	for i := range sealed.Entries {
		if sealed.Entries[i] != current.Entries[i] {
			return fmt.Errorf("trust manifest executable closure changed at %s", firstNonEmpty(current.Entries[i].Path, sealed.Entries[i].Path))
		}
	}
	return nil
}

func equalRoots(left, right []Root) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validateManifest(manifest Manifest) error {
	if manifest.Version != manifestVersion {
		return fmt.Errorf("unsupported trust manifest version %d", manifest.Version)
	}
	if !filepath.IsAbs(manifest.PackageRoot) {
		return fmt.Errorf("trust manifest package root must be absolute")
	}
	if len(manifest.Roots) == 0 || len(manifest.Entries) == 0 {
		return fmt.Errorf("trust manifest has an empty executable closure")
	}
	seenRoots := make(map[string]struct{}, len(manifest.Roots))
	for _, root := range manifest.Roots {
		if strings.TrimSpace(root.Name) == "" || !filepath.IsAbs(root.Path) {
			return fmt.Errorf("trust manifest has an invalid executable root")
		}
		if _, ok := seenRoots[root.Name]; ok {
			return fmt.Errorf("trust manifest repeats executable root %s", root.Name)
		}
		seenRoots[root.Name] = struct{}{}
	}
	seenEntries := make(map[string]struct{}, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if !filepath.IsAbs(entry.Path) || !validEntryKind(entry.Kind) || len(entry.SHA256) != sha256.Size*2 || entry.Size < 0 || entry.Mode > 0o777 {
			return fmt.Errorf("trust manifest has an invalid closure entry for %s", entry.Path)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("trust manifest has an invalid digest for %s", entry.Path)
		}
		if _, ok := seenEntries[entry.Path]; ok {
			return fmt.Errorf("trust manifest repeats closure path %s", entry.Path)
		}
		seenEntries[entry.Path] = struct{}{}
	}
	return nil
}

func validEntryKind(kind EntryKind) bool {
	switch kind {
	case EntryTreeFile, EntryTreeSymlink, EntryRuntimeScript, EntryELFBinary, EntryMachOBinary, EntrySharedLibrary, EntryELFInterpreter, EntryScriptInterpreter:
		return true
	default:
		return false
	}
}

type normalizedInput struct {
	PackageRoot string
	Roots       map[string]string
	LaunchPath  []string
}

func normalizeInput(input Input) (normalizedInput, error) {
	packageRoot, err := resolveExistingPath(input.PackageRoot)
	if err != nil {
		return normalizedInput{}, fmt.Errorf("resolve package root: %w", err)
	}
	info, err := os.Stat(packageRoot)
	if err != nil || !info.IsDir() {
		return normalizedInput{}, fmt.Errorf("package root %s is not a directory", packageRoot)
	}
	if len(input.Roots) == 0 {
		return normalizedInput{}, fmt.Errorf("at least one executable root is required")
	}
	roots := make(map[string]string, len(input.Roots))
	for name, path := range input.Roots {
		name = strings.TrimSpace(name)
		if name == "" {
			return normalizedInput{}, fmt.Errorf("executable root name is required")
		}
		if name == closureResolverRoot {
			return normalizedInput{}, fmt.Errorf("executable root name %s is reserved", closureResolverRoot)
		}
		path = strings.TrimSpace(path)
		if path == "" {
			return normalizedInput{}, fmt.Errorf("executable root %s path is required", name)
		}
		if !filepath.IsAbs(path) {
			return normalizedInput{}, fmt.Errorf("executable root %s path must be absolute", name)
		}
		resolved, err := resolveExistingPath(path)
		if err != nil {
			return normalizedInput{}, fmt.Errorf("resolve executable root %s: %w", name, err)
		}
		if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
			return normalizedInput{}, fmt.Errorf("executable root %s is not a regular file", name)
		} else if info.Mode().Perm()&0o111 == 0 {
			return normalizedInput{}, fmt.Errorf("executable root %s is not executable", name)
		}
		roots[name] = resolved
	}
	launchPath := input.LaunchPath
	if launchPath == nil {
		launchPath = filepath.SplitList(os.Getenv("PATH"))
	}
	normalizedLaunchPath := make([]string, 0, len(launchPath))
	for _, dir := range launchPath {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			return normalizedInput{}, fmt.Errorf("launch PATH entry %q must be absolute", dir)
		}
		normalizedLaunchPath = append(normalizedLaunchPath, dir)
	}
	if _, ok := roots["srt"]; !ok {
		return normalizedInput{}, fmt.Errorf("srt executable root is required")
	}
	return normalizedInput{PackageRoot: packageRoot, Roots: roots, LaunchPath: normalizedLaunchPath}, nil
}

func resolveExistingPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// executableSearchPaths mirrors the isolated runtime PATH precedence: the
// sealed Node directory wins for env-style shebangs, followed by the other
// sealed support-tool directories and the fixed system fallbacks.
func executableSearchPaths(roots map[string]string, launchPath []string) []string {
	orderedNames := []string{"node", "srt", "rg", "bwrap", "socat"}
	paths := make([]string, 0, len(orderedNames)+len(launchPath)+3)
	seen := make(map[string]struct{})
	for _, name := range orderedNames {
		path := strings.TrimSpace(roots[name])
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		paths = append(paths, dir)
	}
	for _, dir := range launchPath {
		if dir = strings.TrimSpace(dir); dir == "" {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		paths = append(paths, dir)
	}
	for _, dir := range []string{"/usr/bin", "/bin"} {
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		paths = append(paths, dir)
	}
	return paths
}

type closureCollector struct {
	entries             map[string]Entry
	visitedELF          map[string]struct{}
	visitedInterpreters map[string]struct{}
	interpreterPaths    []string
	// elfContextPath is the executable whose loader resolves ELF shared
	// objects discovered in the package tree. Native Node addons are loaded
	// with Node's loader context, but unlike an executable they do not carry a
	// PT_INTERP segment of their own.
	elfContextPath string
	visitedMachO   map[string]struct{}
}

func (c *closureCollector) addPackageTree(root, runtimePath string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			if !pathContains(root, resolved) {
				return fmt.Errorf("package symlink %s resolves outside %s", path, root)
			}
			return c.add(path, EntryTreeSymlink)
		case info.Mode().IsRegular():
			kind := EntryTreeFile
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr == nil && resolved == runtimePath {
				kind = EntryRuntimeScript
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := c.addContent(path, kind, raw); err != nil {
				return err
			}
			if isELFBytes(raw) {
				if err := c.addContent(path, EntryELFBinary, raw); err != nil {
					return err
				}
				return c.addELFClosure(path)
			}
			if isMachOBytes(raw) {
				if err := c.addContent(path, EntryMachOBinary, raw); err != nil {
					return err
				}
				return c.addMachOClosure(path)
			}
			if info.Mode().Perm()&0o111 == 0 {
				return nil
			}
			interpreter, envInterpreter, ok, err := resolveScriptInterpreter(path, raw, c.interpreterPaths)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if envInterpreter != "" {
				if err := c.addInterpreterClosure(envInterpreter); err != nil {
					return err
				}
			}
			return c.addInterpreterClosure(interpreter)
		default:
			return nil
		}
	})
}

func (c *closureCollector) addExecutable(path string, runtimeScript bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if isELFBytes(raw) {
		if err := c.addContent(path, EntryELFBinary, raw); err != nil {
			return err
		}
		return c.addELFClosure(path)
	}
	if isMachOBytes(raw) {
		if err := c.addContent(path, EntryMachOBinary, raw); err != nil {
			return err
		}
		return c.addMachOClosure(path)
	}
	interpreter, envInterpreter, ok, err := resolveScriptInterpreter(path, raw, c.interpreterPaths)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is neither ELF nor a script with an absolute interpreter", path)
	}
	kind := EntryTreeFile
	if runtimeScript {
		kind = EntryRuntimeScript
	}
	if err := c.addContent(path, kind, raw); err != nil {
		return err
	}
	if envInterpreter != "" {
		if err := c.addInterpreterClosure(envInterpreter); err != nil {
			return err
		}
	}
	return c.addInterpreterClosure(interpreter)
}

func (c *closureCollector) addELFClosure(path string) error {
	executablePath := c.elfContextPath
	if executablePath == "" || executablePath == path {
		executablePath = path
	}
	return c.addELFClosureFrom(path, executablePath)
}

func (c *closureCollector) addELFClosureFrom(path, executablePath string) error {
	visitKey := path + "\x00" + executablePath
	if _, ok := c.visitedELF[visitKey]; ok {
		return nil
	}
	c.visitedELF[visitKey] = struct{}{}
	interpreter, ok, err := elfLoaderFor(path, executablePath)
	if err != nil {
		return err
	}
	if ok {
		if err := c.add(interpreter, EntryELFInterpreter); err != nil {
			return err
		}
	}
	dependencies, err := lddDependencies(path, executablePath)
	if err != nil {
		return err
	}
	for _, dependency := range dependencies {
		if err := c.add(dependency, EntrySharedLibrary); err != nil {
			return err
		}
	}
	return nil
}

func (c *closureCollector) addInterpreterClosure(path string) error {
	if _, ok := c.visitedInterpreters[path]; ok {
		return nil
	}
	c.visitedInterpreters[path] = struct{}{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := c.addContent(path, EntryScriptInterpreter, raw); err != nil {
		return err
	}
	if isELFBytes(raw) {
		return c.addELFClosure(path)
	}
	if isMachOBytes(raw) {
		return c.addMachOClosure(path)
	}
	next, envInterpreter, ok, err := resolveScriptInterpreter(path, raw, c.interpreterPaths)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if envInterpreter != "" {
		if err := c.addInterpreterClosure(envInterpreter); err != nil {
			return err
		}
	}
	return c.addInterpreterClosure(next)
}

func (c *closureCollector) add(path string, kind EntryKind) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if existing, ok := c.entries[absolute]; ok {
		if entryKindPriority(kind) > entryKindPriority(existing.Kind) {
			existing.Kind = kind
			c.entries[absolute] = existing
		}
		return nil
	}
	digest, mode, size, err := digestEntryMetadata(absolute, kind, -1)
	if err != nil {
		return err
	}
	c.entries[absolute] = Entry{Path: absolute, Kind: kind, SHA256: digest, Mode: mode, Size: size}
	return nil
}

func (c *closureCollector) addContent(path string, kind EntryKind, raw []byte) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if existing, ok := c.entries[absolute]; ok {
		if entryKindPriority(kind) > entryKindPriority(existing.Kind) {
			existing.Kind = kind
			c.entries[absolute] = existing
		}
		return nil
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("closure entry %s is not a regular file", absolute)
	}
	if info.Size() != int64(len(raw)) {
		return fmt.Errorf("closure entry %s changed while sealing", absolute)
	}
	digest := sha256.Sum256(raw)
	c.entries[absolute] = Entry{Path: absolute, Kind: kind, SHA256: hex.EncodeToString(digest[:]), Mode: sealedMode(info.Mode()), Size: info.Size()}
	return nil
}

func entryKindPriority(kind EntryKind) int {
	switch kind {
	case EntryRuntimeScript:
		return 7
	case EntryELFInterpreter:
		return 6
	case EntryScriptInterpreter:
		return 5
	case EntryELFBinary:
		return 4
	case EntryMachOBinary:
		return 4
	case EntrySharedLibrary:
		return 3
	case EntryTreeSymlink:
		return 2
	default:
		return 1
	}
}

// digestEntryMetadata hashes one recorded object while keeping the file
// descriptor and metadata from the same no-follow open. expectedSize is -1
// while sealing; verification supplies the sealed size to bound the read.
func digestEntryMetadata(path string, kind EntryKind, expectedSize int64) (string, uint32, int64, error) {
	hash := sha256.New()
	if kind == EntryTreeSymlink {
		info, err := os.Lstat(path)
		if err != nil {
			return "", 0, 0, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return "", 0, 0, fmt.Errorf("expected symlink")
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", 0, 0, err
		}
		size := int64(len(target))
		if expectedSize >= 0 && size != expectedSize {
			return "", 0, 0, fmt.Errorf("digest mismatch: symlink size changed")
		}
		_, err = io.WriteString(hash, target)
		if err != nil {
			return "", 0, 0, err
		}
		return hex.EncodeToString(hash.Sum(nil)), 0, size, nil
	} else {
		file, info, err := openRegularFile(path)
		if err != nil {
			return "", 0, 0, err
		}
		defer file.Close()
		if expectedSize >= 0 && info.Size() != expectedSize {
			return "", 0, 0, fmt.Errorf("digest mismatch: file size changed")
		}
		limit := info.Size()
		if expectedSize >= 0 {
			limit = expectedSize
		}
		if _, err := io.CopyN(hash, file, limit); err != nil {
			_ = file.Close()
			return "", 0, 0, err
		}
		after, err := file.Stat()
		if err != nil {
			return "", 0, 0, err
		}
		if info.Size() != limit || after.Size() != info.Size() || sealedMode(after.Mode()) != sealedMode(info.Mode()) {
			return "", 0, 0, fmt.Errorf("digest mismatch: file size changed while hashing")
		}
		return hex.EncodeToString(hash.Sum(nil)), sealedMode(info.Mode()), info.Size(), nil
	}
}

func sealedMode(mode os.FileMode) uint32 {
	return uint32(mode.Perm())
}

func isELFBytes(raw []byte) bool {
	return len(raw) >= 4 && bytes.Equal(raw[:4], []byte{0x7f, 'E', 'L', 'F'})
}

func isMachOBytes(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	be := binary.BigEndian.Uint32(raw[:4])
	le := binary.LittleEndian.Uint32(raw[:4])
	return be == macho.MagicFat || be == macho.Magic32 || be == macho.Magic64 || le == macho.Magic32 || le == macho.Magic64
}

func elfInterpreter(path string) (string, bool, error) {
	file, err := elf.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		// A kernel interpreter path is bounded by PATH_MAX; do not let a
		// package-controlled ELF header drive an unbounded allocation here.
		const maxInterpreterSize = 4096
		if program.Filesz > maxInterpreterSize {
			return "", false, fmt.Errorf("ELF interpreter segment is too large")
		}
		raw, err := io.ReadAll(io.LimitReader(program.Open(), int64(program.Filesz)))
		if err != nil {
			return "", false, fmt.Errorf("read ELF interpreter: %w", err)
		}
		name := strings.TrimRight(string(raw), "\x00\n")
		if !filepath.IsAbs(name) {
			return "", false, fmt.Errorf("ELF interpreter %q is not absolute", name)
		}
		resolved, err := resolveExecutablePath(name, nil)
		if err != nil {
			return "", false, fmt.Errorf("resolve ELF interpreter: %w", err)
		}
		return resolved, true, nil
	}
	return "", false, nil
}

func (c *closureCollector) addMachOClosure(path string) error {
	return c.addMachOClosureFrom(path, path)
}

func (c *closureCollector) addMachOClosureFrom(path, executablePath string) error {
	if _, ok := c.visitedMachO[path]; ok {
		return nil
	}
	c.visitedMachO[path] = struct{}{}
	dependencies, err := machoDependenciesForExecutable(path, executablePath)
	if err != nil {
		return err
	}
	for _, dependency := range dependencies {
		if err := c.add(dependency, EntrySharedLibrary); err != nil {
			return err
		}
		raw, err := os.ReadFile(dependency)
		if err != nil {
			return err
		}
		if isMachOBytes(raw) {
			if err := c.addMachOClosureFrom(dependency, executablePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func machoDependenciesForExecutable(path, executablePath string) ([]string, error) {
	file, err := macho.Open(path)
	if err == nil {
		defer file.Close()
		return machoFileDependencies(file, path, executablePath)
	}
	fat, fatErr := macho.OpenFat(path)
	if fatErr != nil {
		return nil, err
	}
	defer fat.Close()
	paths := make(map[string]struct{})
	for _, arch := range fat.Arches {
		dependencies, archErr := machoFileDependencies(arch.File, path, executablePath)
		if archErr != nil {
			return nil, archErr
		}
		for _, dependency := range dependencies {
			paths[dependency] = struct{}{}
		}
	}
	result := make([]string, 0, len(paths))
	for dependency := range paths {
		result = append(result, dependency)
	}
	sort.Strings(result)
	return result, nil
}

func machoFileDependencies(file *macho.File, path string, executablePaths ...string) ([]string, error) {
	executablePath := path
	if len(executablePaths) > 0 && strings.TrimSpace(executablePaths[0]) != "" {
		executablePath = executablePaths[0]
	}
	libraries, err := file.ImportedLibraries()
	if err != nil {
		return nil, err
	}
	libraries = append(libraries, rawMachOLibraries(file)...)
	rpaths := []string{}
	for _, load := range file.Loads {
		if rpath, ok := load.(*macho.Rpath); ok {
			if err := validateMachORpath(rpath.Path); err != nil {
				return nil, fmt.Errorf("Mach-O %s: %w", path, err)
			}
			rpaths = append(rpaths, rpath.Path)
		}
	}
	resolved := make(map[string]struct{}, len(libraries))
	for _, library := range libraries {
		candidate, err := resolveMachOLibraryFrom(path, executablePath, library, rpaths)
		if err != nil {
			if isMachOSystemLibrary(library) {
				// Modern macOS stores these immutable libraries in the dyld
				// shared cache instead of exposing regular files at their load
				// paths. The platform owns that cache; package-local dylibs
				// still fail closed above.
				continue
			}
			return nil, err
		}
		resolved[candidate] = struct{}{}
	}
	result := make([]string, 0, len(resolved))
	for dependency := range resolved {
		result = append(result, dependency)
	}
	sort.Strings(result)
	return result, nil
}

const (
	machoLoadWeakDylib macho.LoadCmd = 0x80000018
	machoLazyLoadDylib macho.LoadCmd = 0x00000020
	machoReexportDylib macho.LoadCmd = 0x8000001f
	machoUpwardDylib   macho.LoadCmd = 0x80000023
)

func rawMachOLibraries(file *macho.File) []string {
	known := map[macho.LoadCmd]struct{}{
		machoLoadWeakDylib: {},
		machoLazyLoadDylib: {},
		machoReexportDylib: {},
		machoUpwardDylib:   {},
	}
	var libraries []string
	for _, load := range file.Loads {
		raw := load.Raw()
		if len(raw) < 12 {
			continue
		}
		cmd := file.ByteOrder.Uint32(raw[:4])
		if _, ok := known[macho.LoadCmd(cmd)]; !ok {
			continue
		}
		nameOffset := file.ByteOrder.Uint32(raw[8:12])
		if nameOffset >= uint32(len(raw)) {
			continue
		}
		name := raw[nameOffset:]
		if end := bytes.IndexByte(name, 0); end >= 0 {
			name = name[:end]
		}
		if len(name) > 0 {
			libraries = append(libraries, string(name))
		}
	}
	return libraries
}

func validateMachORpath(rpath string) error {
	rpath = strings.TrimSpace(rpath)
	if rpath == "" {
		return fmt.Errorf("empty LC_RPATH")
	}
	if filepath.IsAbs(rpath) || strings.HasPrefix(rpath, "@loader_path/") || strings.HasPrefix(rpath, "@executable_path/") {
		return nil
	}
	return fmt.Errorf("relative LC_RPATH %q is not allowed", rpath)
}

func isMachOSystemLibrary(path string) bool {
	return strings.HasPrefix(path, "/usr/lib/") || strings.HasPrefix(path, "/System/Library/")
}

func resolveMachOLibraryFrom(binaryPath, executablePath, library string, rpaths []string) (string, error) {
	library = strings.TrimSpace(library)
	if library == "" {
		return "", fmt.Errorf("Mach-O %s has an empty library dependency", binaryPath)
	}
	candidates := []string{}
	loaderDir := filepath.Dir(binaryPath)
	executableDir := filepath.Dir(executablePath)
	switch {
	case filepath.IsAbs(library):
		candidates = append(candidates, library)
	case strings.HasPrefix(library, "@loader_path/"):
		candidates = append(candidates, filepath.Join(loaderDir, strings.SplitN(library, "/", 2)[1]))
	case strings.HasPrefix(library, "@executable_path/"):
		candidates = append(candidates, filepath.Join(executableDir, strings.SplitN(library, "/", 2)[1]))
	case strings.HasPrefix(library, "@rpath/"):
		relative := strings.SplitN(library, "/", 2)[1]
		for _, rpath := range rpaths {
			rpath = strings.ReplaceAll(rpath, "@loader_path", loaderDir)
			rpath = strings.ReplaceAll(rpath, "@executable_path", executableDir)
			candidates = append(candidates, filepath.Join(rpath, relative))
		}
		candidates = append(candidates, filepath.Join("/usr/lib", relative), filepath.Join("/System/Library", relative))
	default:
		candidates = append(candidates, filepath.Join(loaderDir, library), filepath.Join("/usr/lib", library), filepath.Join("/System/Library/Frameworks", library))
	}
	for _, candidate := range candidates {
		resolved, err := resolveExistingPath(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("resolve Mach-O dependency %s for %s: file not found", library, binaryPath)
}

func resolveScriptInterpreter(path string, raw []byte, searchPaths []string) (string, string, bool, error) {
	line, _, _ := bytes.Cut(raw, []byte{'\n'})
	if !bytes.HasPrefix(line, []byte("#!")) {
		return "", "", false, nil
	}
	fields := strings.Fields(strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("#!")))))
	if len(fields) == 0 {
		return "", "", false, fmt.Errorf("script %s has no absolute interpreter", path)
	}
	interpreter := fields[0]
	envInterpreter := ""
	if filepath.Base(interpreter) == "env" {
		if !canonicalEnvInterpreter(interpreter) {
			return "", "", false, fmt.Errorf("script %s uses unsupported env interpreter path %q", path, interpreter)
		}
		if len(fields) != 2 || strings.HasPrefix(fields[1], "-") {
			return "", "", false, fmt.Errorf("script %s uses unsupported env interpreter form", path)
		}
		resolvedEnv, err := resolveExecutablePath(interpreter, nil)
		if err != nil {
			return "", "", false, fmt.Errorf("resolve env interpreter: %w", err)
		}
		envInterpreter = resolvedEnv
		interpreter = fields[1]
	} else if !filepath.IsAbs(interpreter) {
		return "", "", false, fmt.Errorf("script %s has a non-absolute direct interpreter %q", path, interpreter)
	}
	resolved, err := resolveExecutablePath(interpreter, searchPaths)
	if err != nil {
		return "", "", false, fmt.Errorf("resolve script interpreter: %w", err)
	}
	return resolved, envInterpreter, true, nil
}

func canonicalEnvInterpreter(path string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	return path == "/usr/bin/env" || path == "/bin/env"
}

func resolveExecutablePath(path string, searchPaths []string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("interpreter path is required")
	}
	if filepath.IsAbs(path) {
		resolved, err := resolveExistingPath(path)
		if err != nil {
			return "", err
		}
		return validateExecutablePath(resolved)
	}
	for _, dir := range searchPaths {
		candidate := filepath.Join(dir, path)
		resolved, err := resolveExistingPath(candidate)
		if err != nil {
			continue
		}
		if validated, err := validateExecutablePath(resolved); err == nil {
			return validated, nil
		}
	}
	return "", fmt.Errorf("interpreter %q was not found in the sealed PATH", path)
}

func validateExecutablePath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable regular file", path)
	}
	return path, nil
}

// elfLoaderFor returns the loader that would resolve path's ELF closure. A
// normal executable names its loader in PT_INTERP. Shared objects do not, so
// their DT_NEEDED entries must be resolved with the loader of the executable
// that will load them (Node for package-native addons).
func elfLoaderFor(path, executablePath string) (string, bool, error) {
	interpreter, ok, err := elfInterpreter(path)
	if err != nil {
		return "", false, err
	}
	if ok {
		return interpreter, true, nil
	}
	needed, err := elfNeededLibraries(path)
	if err != nil {
		return "", false, fmt.Errorf("read ELF dependencies for %s: %w", path, err)
	}
	if len(needed) == 0 {
		// An ELF without PT_INTERP and DT_NEEDED is genuinely static (or a
		// data-only ELF object), so there is no loader to execute or record.
		return "", false, nil
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" || executablePath == path {
		return "", false, fmt.Errorf("ELF shared object %s has DT_NEEDED dependencies but no loading executable context", path)
	}
	interpreter, ok, err = elfInterpreter(executablePath)
	if err != nil {
		return "", false, fmt.Errorf("resolve loading executable for %s: %w", path, err)
	}
	if !ok {
		return "", false, fmt.Errorf("loading executable %s has no PT_INTERP for ELF shared object %s", executablePath, path)
	}
	return interpreter, true, nil
}

func elfNeededLibraries(path string) ([]string, error) {
	file, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.ImportedLibraries()
}

func lddDependencies(path string, loadingExecutable ...string) ([]string, error) {
	executablePath := ""
	if len(loadingExecutable) > 0 {
		executablePath = loadingExecutable[0]
	}
	interpreter, ok, err := elfLoaderFor(path, executablePath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lddTimeout)
	defer cancel()
	// Invoke the exact PT_INTERP loader with its non-executing --list mode. This
	// avoids /usr/bin/ldd's shell wrapper and its RTLDLIST probe loop, while
	// matching the loader the kernel will use for this binary.
	credential, credentialOK := resolverCredential()
	if err := requireUnprivilegedResolver(os.Geteuid(), credentialOK); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, interpreter, "--list", path)
	// Resolve the closure under the same no-loader-override assumption rather
	// than inheriting a daemon LD_LIBRARY_PATH or LD_PRELOAD into the authority
	// calculation.
	command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	if credentialOK {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		combined := strings.ToLower(strings.TrimSpace(string(output) + "\n" + stderr.String()))
		if strings.Contains(combined, "statically linked") {
			return []string{}, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ldd %s: %w", path, ctx.Err())
		}
		return nil, fmt.Errorf("resolve ELF dependencies for %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return parseLDDOutput(string(output))
}

func requireUnprivilegedResolver(euid int, credentialOK bool) error {
	if euid == 0 && !credentialOK {
		return fmt.Errorf("refusing to execute ELF interpreter as root: set SUDO_UID/SUDO_GID to the unprivileged daemon identity")
	}
	return nil
}

func resolverCredential() (*syscall.Credential, bool) {
	return parseResolverCredential(os.Geteuid(), os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"))
}

func parseResolverCredential(euid int, uidText, gidText string) (*syscall.Credential, bool) {
	if euid != 0 {
		return nil, false
	}
	uid, uidErr := strconv.ParseUint(strings.TrimSpace(uidText), 10, 32)
	gid, gidErr := strconv.ParseUint(strings.TrimSpace(gidText), 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		// A root-only installer may not have an intended daemon identity in its
		// environment. Do not guess an unrelated account; the direct loader
		// resolver only reads metadata and Verify still runs as the daemon user.
		return nil, false
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, true
}

func parseLDDOutput(output string) ([]string, error) {
	paths := make(map[string]struct{})
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "statically linked") || strings.HasPrefix(line, "linux-vdso") {
			continue
		}
		if strings.Contains(line, "=> not found") {
			return nil, fmt.Errorf("unresolved ELF dependency: %s", line)
		}
		candidate := ""
		if _, after, ok := strings.Cut(line, "=>"); ok {
			fields := strings.Fields(after)
			if len(fields) == 0 {
				return nil, fmt.Errorf("malformed ldd output: %s", line)
			}
			candidate = strings.TrimSpace(fields[0])
		} else {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			candidate = strings.TrimSpace(fields[0])
		}
		if !filepath.IsAbs(candidate) {
			return nil, fmt.Errorf("non-absolute ELF dependency: %s", line)
		}
		resolved, err := resolveExistingPath(candidate)
		if err != nil {
			return nil, err
		}
		paths[resolved] = struct{}{}
	}
	result := make([]string, 0, len(paths))
	for dependency := range paths {
		result = append(result, dependency)
	}
	sort.Strings(result)
	return result, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}

func pathContains(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
