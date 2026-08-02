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
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	manifestVersion     = 1
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
		interpreterPaths:    executableSearchPaths(normalized.Roots),
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
		digest, err := digestEntry(entry.Path, entry.Kind)
		if err != nil {
			return fmt.Errorf("verify %s: %w", entry.Path, err)
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
		if !filepath.IsAbs(entry.Path) || !validEntryKind(entry.Kind) || len(entry.SHA256) != sha256.Size*2 {
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
	if runtime.GOOS == "linux" {
		resolver, err := lddExecutable()
		if err != nil {
			return normalizedInput{}, err
		}
		roots[closureResolverRoot] = resolver
	}
	if _, ok := roots["srt"]; !ok {
		return normalizedInput{}, fmt.Errorf("srt executable root is required")
	}
	return normalizedInput{PackageRoot: packageRoot, Roots: roots}, nil
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
func executableSearchPaths(roots map[string]string) []string {
	orderedNames := []string{"node", "srt", "rg", "bwrap", "socat", closureResolverRoot}
	paths := make([]string, 0, len(orderedNames)+3)
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
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
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
	visitedMachO        map[string]struct{}
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
			interpreter, ok, err := scriptInterpreterBytes(path, raw, c.interpreterPaths)
			if err != nil {
				return err
			}
			if !ok {
				return nil
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
	interpreter, ok, err := scriptInterpreterBytes(path, raw, c.interpreterPaths)
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
	return c.addInterpreterClosure(interpreter)
}

func (c *closureCollector) addELFClosure(path string) error {
	if _, ok := c.visitedELF[path]; ok {
		return nil
	}
	c.visitedELF[path] = struct{}{}
	interpreter, ok, err := elfInterpreter(path)
	if err != nil {
		return err
	}
	if ok {
		if err := c.add(interpreter, EntryELFInterpreter); err != nil {
			return err
		}
	}
	dependencies, err := lddDependencies(path)
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
	next, ok, err := scriptInterpreterBytes(path, raw, c.interpreterPaths)
	if err != nil {
		return err
	}
	if !ok {
		return nil
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
	digest, err := digestEntry(absolute, kind)
	if err != nil {
		return err
	}
	c.entries[absolute] = Entry{Path: absolute, Kind: kind, SHA256: digest}
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
	digest := sha256.Sum256(raw)
	c.entries[absolute] = Entry{Path: absolute, Kind: kind, SHA256: hex.EncodeToString(digest[:])}
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

func digestEntry(path string, kind EntryKind) (string, error) {
	hash := sha256.New()
	if kind == EntryTreeSymlink {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		_, err = io.WriteString(hash, target)
		if err != nil {
			return "", err
		}
	} else {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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
		if program.Filesz > uint64(^uint64(0)>>1) {
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
	if _, ok := c.visitedMachO[path]; ok {
		return nil
	}
	c.visitedMachO[path] = struct{}{}
	dependencies, err := machoDependencies(path)
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
			if err := c.addMachOClosure(dependency); err != nil {
				return err
			}
		}
	}
	return nil
}

func machoDependencies(path string) ([]string, error) {
	file, err := macho.Open(path)
	if err == nil {
		defer file.Close()
		return machoFileDependencies(file, path)
	}
	fat, fatErr := macho.OpenFat(path)
	if fatErr != nil {
		return nil, err
	}
	defer fat.Close()
	paths := make(map[string]struct{})
	for _, arch := range fat.Arches {
		dependencies, archErr := machoFileDependencies(arch.File, path)
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

func machoFileDependencies(file *macho.File, path string) ([]string, error) {
	libraries, err := file.ImportedLibraries()
	if err != nil {
		return nil, err
	}
	rpaths := []string{}
	for _, load := range file.Loads {
		if rpath, ok := load.(*macho.Rpath); ok {
			rpaths = append(rpaths, rpath.Path)
		}
	}
	resolved := make(map[string]struct{}, len(libraries))
	for _, library := range libraries {
		candidate, err := resolveMachOLibrary(path, library, rpaths)
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

func isMachOSystemLibrary(path string) bool {
	return strings.HasPrefix(path, "/usr/lib/") || strings.HasPrefix(path, "/System/Library/")
}

func resolveMachOLibrary(binaryPath, library string, rpaths []string) (string, error) {
	library = strings.TrimSpace(library)
	if library == "" {
		return "", fmt.Errorf("Mach-O %s has an empty library dependency", binaryPath)
	}
	candidates := []string{}
	baseDir := filepath.Dir(binaryPath)
	switch {
	case filepath.IsAbs(library):
		candidates = append(candidates, library)
	case strings.HasPrefix(library, "@loader_path/") || strings.HasPrefix(library, "@executable_path/"):
		candidates = append(candidates, filepath.Join(baseDir, strings.SplitN(library, "/", 2)[1]))
	case strings.HasPrefix(library, "@rpath/"):
		relative := strings.SplitN(library, "/", 2)[1]
		for _, rpath := range rpaths {
			rpath = strings.ReplaceAll(rpath, "@loader_path", baseDir)
			candidates = append(candidates, filepath.Join(rpath, relative))
		}
		candidates = append(candidates, filepath.Join("/usr/lib", relative), filepath.Join("/System/Library", relative))
	default:
		candidates = append(candidates, filepath.Join(baseDir, library), filepath.Join("/usr/lib", library), filepath.Join("/System/Library/Frameworks", library))
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

func scriptInterpreterBytes(path string, raw []byte, searchPaths []string) (string, bool, error) {
	line, _, _ := bytes.Cut(raw, []byte{'\n'})
	if !bytes.HasPrefix(line, []byte("#!")) {
		return "", false, nil
	}
	fields := strings.Fields(strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("#!")))))
	if len(fields) == 0 {
		return "", false, fmt.Errorf("script %s has no absolute interpreter", path)
	}
	interpreter := fields[0]
	if filepath.Base(interpreter) == "env" {
		if len(fields) != 2 || strings.HasPrefix(fields[1], "-") {
			return "", false, fmt.Errorf("script %s uses unsupported env interpreter form", path)
		}
		interpreter = fields[1]
	}
	resolved, err := resolveExecutablePath(interpreter, searchPaths)
	if err != nil {
		return "", false, fmt.Errorf("resolve script interpreter: %w", err)
	}
	return resolved, true, nil
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

func lddDependencies(path string) ([]string, error) {
	lddPath, err := lddExecutable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lddTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, lddPath, path)
	// The sandbox process receives a closed environment. Resolve the closure
	// under the same no-loader-override assumption rather than inheriting a
	// daemon LD_LIBRARY_PATH or LD_PRELOAD into the authority calculation.
	command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	if os.Geteuid() == 0 {
		nobody, err := user.Lookup("nobody")
		if err != nil {
			return nil, fmt.Errorf("resolve non-root ldd user: %w", err)
		}
		uid, err := strconv.ParseUint(nobody.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse non-root ldd uid: %w", err)
		}
		gid, err := strconv.ParseUint(nobody.Gid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse non-root ldd gid: %w", err)
		}
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
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
		return nil, fmt.Errorf("ldd %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return parseLDDOutput(string(output))
}

func lddExecutable() (string, error) {
	candidate, err := exec.LookPath("ldd")
	if err != nil {
		return "", fmt.Errorf("ldd is required to resolve ELF closure: %w", err)
	}
	if !filepath.IsAbs(candidate) {
		candidate, err = filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
	}
	return resolveExecutablePath(candidate, nil)
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
