// Package upgraderelease stages matching CLI and daemon binaries as immutable
// release directories and switches a single current pointer between them.
package upgraderelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/version"
)

const ManifestVersion = 1

var releaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var activateSyncDirectory = syncDirectory

type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

type Manifest struct {
	Version   int             `json:"version"`
	CreatedAt string          `json:"createdAt"`
	Build     version.Info    `json:"build"`
	Files     map[string]File `json:"files"`
}

type StageInput struct {
	RootDir          string
	ReleaseID        string
	CLIBinaryPath    string
	DaemonBinaryPath string
	Build            version.Info
	Now              func() time.Time
}

type StageResult struct {
	RootDir   string   `json:"rootDir"`
	ReleaseID string   `json:"releaseId"`
	Directory string   `json:"directory"`
	Manifest  Manifest `json:"manifest"`
}

type ActivationResult struct {
	RootDir           string `json:"rootDir"`
	PreviousReleaseID string `json:"previousReleaseId,omitempty"`
	CurrentReleaseID  string `json:"currentReleaseId"`
	// ServiceExecutable is the path a supervised unit must launch after
	// activation (root/current/looperd). Install via looperd service install
	// rewrites release-tree binaries onto this pointer automatically.
	ServiceExecutable string `json:"serviceExecutable"`
}

// Stage copies both executable inputs into a new immutable release directory.
// The final directory appears only after its manifest is written, and is never
// modified afterward. Callers must execute the staged binaries themselves to
// prove their embedded identities match Manifest.Build before activation.
func Stage(input StageInput) (StageResult, error) {
	root, err := normalizedRoot(input.RootDir)
	if err != nil {
		return StageResult{}, err
	}
	if err := validateReleaseID(input.ReleaseID); err != nil {
		return StageResult{}, err
	}
	if err := validateBuild(input.Build); err != nil {
		return StageResult{}, err
	}
	if strings.TrimSpace(input.CLIBinaryPath) == "" || strings.TrimSpace(input.DaemonBinaryPath) == "" {
		return StageResult{}, fmt.Errorf("CLI and daemon binary paths are required")
	}
	now := time.Now
	if input.Now != nil {
		now = input.Now
	}
	releasesDir := filepath.Join(root, "releases")
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		return StageResult{}, fmt.Errorf("create releases directory: %w", err)
	}
	destination := filepath.Join(releasesDir, input.ReleaseID)
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return StageResult{}, fmt.Errorf("release destination %s must be a real directory, not a symlink or other file", destination)
		}
		// Idempotent restage: later cutovers re-stage the live pair that is
		// already this release id from a prior candidate stage.
		return reuseExistingRelease(root, input)
	} else if !os.IsNotExist(err) {
		return StageResult{}, fmt.Errorf("inspect release destination: %w", err)
	}
	temporary, err := os.MkdirTemp(releasesDir, ".stage-")
	if err != nil {
		return StageResult{}, fmt.Errorf("create release staging directory: %w", err)
	}
	fail := func(err error) (StageResult, error) {
		_ = os.RemoveAll(temporary)
		return StageResult{}, err
	}
	files := map[string]File{}
	for _, item := range []struct {
		source string
		name   string
	}{{input.CLIBinaryPath, "looper"}, {input.DaemonBinaryPath, "looperd"}} {
		file, err := copyExecutable(item.source, filepath.Join(temporary, item.name), item.name)
		if err != nil {
			return fail(err)
		}
		files[item.name] = file
	}
	manifest := Manifest{Version: ManifestVersion, CreatedAt: now().UTC().Format(time.RFC3339Nano), Build: input.Build, Files: files}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("encode release manifest: %w", err))
	}
	if err := writeFile(filepath.Join(temporary, "manifest.json"), append(encoded, '\n'), 0o600); err != nil {
		return fail(fmt.Errorf("write release manifest: %w", err))
	}
	if err := syncDirectory(temporary); err != nil {
		return fail(err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fail(fmt.Errorf("publish release directory: %w", err))
	}
	if err := syncDirectory(releasesDir); err != nil {
		return StageResult{}, err
	}
	return StageResult{RootDir: root, ReleaseID: input.ReleaseID, Directory: destination, Manifest: manifest}, nil
}

// reuseExistingRelease returns a verified existing release when the operator
// re-stages the same build (normal on the second+ cutover). Rejects a
// same-id directory whose build or binary bytes differ.
func reuseExistingRelease(root string, input StageInput) (StageResult, error) {
	existing, err := Verify(root, input.ReleaseID)
	if err != nil {
		return StageResult{}, fmt.Errorf("release %s already exists but is not a valid immutable stage: %w", input.ReleaseID, err)
	}
	if !releaseBuildMatches(existing.Manifest.Build, input.Build) {
		return StageResult{}, fmt.Errorf("release %s already exists with a different build identity", input.ReleaseID)
	}
	for _, item := range []struct {
		source string
		name   string
	}{{input.CLIBinaryPath, "looper"}, {input.DaemonBinaryPath, "looperd"}} {
		want, ok := existing.Manifest.Files[item.name]
		if !ok {
			return StageResult{}, fmt.Errorf("release %s is missing staged %s", input.ReleaseID, item.name)
		}
		got, err := describeFile(item.source, item.name)
		if err != nil {
			return StageResult{}, fmt.Errorf("hash input %s for existing release %s: %w", item.name, input.ReleaseID, err)
		}
		if got.SHA256 != want.SHA256 || got.Size != want.Size {
			return StageResult{}, fmt.Errorf("release %s already exists but %s bytes differ from the staged copy", input.ReleaseID, item.name)
		}
	}
	return existing, nil
}

func releaseBuildMatches(existing, input version.Info) bool {
	if existing.Complete() && input.Complete() {
		return existing.SameBuild(input)
	}
	// Incomplete identities (tests, partial metadata) still require the fields
	// Stage already validated plus matching optional commit when both present.
	if existing.Version != input.Version ||
		existing.Metadata.VersionSource != input.Metadata.VersionSource ||
		existing.Metadata.Channel != input.Metadata.Channel ||
		existing.Metadata.APIVersion != input.Metadata.APIVersion {
		return false
	}
	if existing.Metadata.GitCommitSHA != nil && input.Metadata.GitCommitSHA != nil &&
		*existing.Metadata.GitCommitSHA != *input.Metadata.GitCommitSHA {
		return false
	}
	return true
}

// Verify confirms a staged release has the exact immutable layout Stage
// creates. It validates file integrity, not source provenance; callers that
// need identity proof must execute both staged binaries before activation.
func Verify(rootDir, releaseID string) (StageResult, error) {
	root, err := normalizedRoot(rootDir)
	if err != nil {
		return StageResult{}, err
	}
	if err := validateReleaseID(releaseID); err != nil {
		return StageResult{}, err
	}
	directory := filepath.Join(root, "releases", releaseID)
	manifestPath := filepath.Join(directory, "manifest.json")
	if err := requireRegularFile(manifestPath, "release manifest"); err != nil {
		return StageResult{}, err
	}
	encoded, err := os.ReadFile(manifestPath)
	if err != nil {
		return StageResult{}, fmt.Errorf("read release manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return StageResult{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return StageResult{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return StageResult{}, fmt.Errorf("read release directory: %w", err)
	}
	expected := map[string]bool{"manifest.json": true}
	for name, want := range manifest.Files {
		expected[name] = true
		path := filepath.Join(directory, name)
		if err := requireRegularFile(path, "release file "+name); err != nil {
			return StageResult{}, err
		}
		got, err := describeFile(path, name)
		if err != nil {
			return StageResult{}, err
		}
		if got != want {
			return StageResult{}, fmt.Errorf("release file %s does not match manifest", name)
		}
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return StageResult{}, fmt.Errorf("unexpected release entry %s", entry.Name())
		}
	}
	return StageResult{RootDir: root, ReleaseID: releaseID, Directory: directory, Manifest: manifest}, nil
}

// Activate switches root/current to a verified release through one atomic
// rename of a relative symlink. It never starts, stops, or signals looperd.
// The returned ServiceExecutable is root/current/looperd — the path supervised
// installs must use so activation alone switches the next service start.
func Activate(rootDir, releaseID string) (ActivationResult, error) {
	staged, err := Verify(rootDir, releaseID)
	if err != nil {
		return ActivationResult{}, err
	}
	previous, err := currentReleaseID(staged.RootDir)
	if err != nil {
		return ActivationResult{}, err
	}
	serviceExecutable := CurrentDaemonExecutable(staged.RootDir)
	if previous == releaseID {
		// Idempotent activate: still ensure the durability barrier for current.
		if err := activateSyncDirectory(staged.RootDir); err != nil {
			return ActivationResult{}, fmt.Errorf("sync already-active release pointer: %w", err)
		}
		return ActivationResult{RootDir: staged.RootDir, PreviousReleaseID: previous, CurrentReleaseID: releaseID, ServiceExecutable: serviceExecutable}, nil
	}
	temporary, err := os.CreateTemp(staged.RootDir, ".current-")
	if err != nil {
		return ActivationResult{}, fmt.Errorf("reserve current pointer: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return ActivationResult{}, fmt.Errorf("close current pointer reservation: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ActivationResult{}, fmt.Errorf("prepare current pointer: %w", err)
	}
	if err := os.Symlink(filepath.Join("releases", releaseID), temporaryPath); err != nil {
		return ActivationResult{}, fmt.Errorf("create current pointer: %w", err)
	}
	currentPath := filepath.Join(staged.RootDir, "current")
	if err := os.Rename(temporaryPath, currentPath); err != nil {
		_ = os.Remove(temporaryPath)
		return ActivationResult{}, fmt.Errorf("atomically switch current release: %w", err)
	}
	if err := activateSyncDirectory(staged.RootDir); err != nil {
		// current already points at the candidate; restore previous through an
		// atomic temporary-symlink rename and surface every rollback failure.
		var restoreErr error
		selected, selectErr := currentReleaseID(staged.RootDir)
		switch {
		case selectErr != nil:
			restoreErr = fmt.Errorf("inspect current pointer before rollback: %w", selectErr)
		case selected != releaseID:
			restoreErr = fmt.Errorf("current pointer changed to %q before rollback", selected)
		default:
			restoreErr = restoreCurrentPointer(staged.RootDir, currentPath, previous)
		}
		if restoreErr != nil {
			return ActivationResult{}, fmt.Errorf("sync current release pointer after activate: %v; restore previous %q: %w", err, previous, restoreErr)
		}
		return ActivationResult{}, fmt.Errorf("sync current release pointer after activate (restored previous %q): %w", previous, err)
	}
	return ActivationResult{RootDir: staged.RootDir, PreviousReleaseID: previous, CurrentReleaseID: releaseID, ServiceExecutable: serviceExecutable}, nil
}

func restoreCurrentPointer(root, currentPath, previous string) error {
	if previous == "" {
		if err := os.Remove(currentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove candidate current pointer: %w", err)
		}
		return activateSyncDirectory(root)
	}
	temporary, err := os.CreateTemp(root, ".current-restore-")
	if err != nil {
		return fmt.Errorf("reserve previous current pointer: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close previous current pointer reservation: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("prepare previous current pointer: %w", err)
	}
	if err := os.Symlink(filepath.Join("releases", previous), temporaryPath); err != nil {
		return fmt.Errorf("create previous current pointer: %w", err)
	}
	if err := os.Rename(temporaryPath, currentPath); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("atomically restore previous current pointer: %w", err)
	}
	if err := activateSyncDirectory(root); err != nil {
		return fmt.Errorf("sync restored current pointer: %w", err)
	}
	return nil
}

// CurrentDaemonExecutable is the supervised-launch path for an activated
// release tree (root/current/looperd).
func CurrentDaemonExecutable(rootDir string) string {
	root := strings.TrimSpace(rootDir)
	if root == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(root), "current", "looperd")
}

func ReleaseIDs(rootDir string) ([]string, error) {
	root, err := normalizedRoot(rootDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if err != nil {
		return nil, fmt.Errorf("read releases directory: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && releaseIDPattern.MatchString(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// CurrentReleaseID reports the release selected by root/current. An empty
// value means no release has been activated yet; a malformed pointer is an
// error rather than a guess about what a future daemon launch will select.
func CurrentReleaseID(rootDir string) (string, error) {
	root, err := normalizedRoot(rootDir)
	if err != nil {
		return "", err
	}
	return currentReleaseID(root)
}

func normalizedRoot(rootDir string) (string, error) {
	if strings.TrimSpace(rootDir) == "" {
		return "", fmt.Errorf("release root directory is required")
	}
	root, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("resolve release root: %w", err)
	}
	return filepath.Clean(root), nil
}

func validateReleaseID(releaseID string) error {
	if !releaseIDPattern.MatchString(releaseID) {
		return fmt.Errorf("invalid release ID %q", releaseID)
	}
	return nil
}

func validateBuild(build version.Info) error {
	if strings.TrimSpace(build.Version) == "" || strings.TrimSpace(build.Metadata.VersionSource) == "" || strings.TrimSpace(build.Metadata.Channel) == "" || strings.TrimSpace(build.Metadata.APIVersion) == "" {
		return fmt.Errorf("complete build identity is required")
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("unsupported release manifest version %d", manifest.Version)
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return fmt.Errorf("invalid release manifest createdAt: %w", err)
	}
	if err := validateBuild(manifest.Build); err != nil {
		return fmt.Errorf("invalid release manifest build: %w", err)
	}
	if len(manifest.Files) != 2 {
		return fmt.Errorf("release manifest must name exactly looper and looperd")
	}
	for _, name := range []string{"looper", "looperd"} {
		file, ok := manifest.Files[name]
		if !ok || !validFile(file) {
			return fmt.Errorf("invalid release manifest file %s", name)
		}
	}
	return nil
}

func currentReleaseID(root string) (string, error) {
	currentPath := filepath.Join(root, "current")
	info, err := os.Lstat(currentPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect current release: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("current release pointer must be a symlink")
	}
	target, err := os.Readlink(currentPath)
	if err != nil {
		return "", fmt.Errorf("read current release pointer: %w", err)
	}
	prefix := "releases" + string(filepath.Separator)
	if !strings.HasPrefix(target, prefix) {
		return "", fmt.Errorf("current release pointer escapes release directory")
	}
	id := strings.TrimPrefix(target, prefix)
	if filepath.Base(id) != id || validateReleaseID(id) != nil {
		return "", fmt.Errorf("current release pointer has invalid target")
	}
	return id, nil
}

func copyExecutable(source, destination, name string) (File, error) {
	info, err := os.Stat(source)
	if err != nil {
		return File{}, fmt.Errorf("stat %s source: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return File{}, fmt.Errorf("%s source must be an executable regular file", name)
	}
	in, err := os.Open(source)
	if err != nil {
		return File{}, fmt.Errorf("open %s source: %w", name, err)
	}
	defer in.Close()
	if err := writeFileFrom(destination, in, info.Mode().Perm()); err != nil {
		return File{}, fmt.Errorf("copy %s: %w", name, err)
	}
	return describeFile(destination, name)
}

func writeFile(path string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeFileFrom(path string, source io.Reader, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func describeFile(path, name string) (File, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read %s for checksum: %w", name, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return File{}, fmt.Errorf("stat %s: %w", name, err)
	}
	sum := sha256.Sum256(contents)
	return File{SHA256: hex.EncodeToString(sum[:]), Size: info.Size(), Mode: uint32(info.Mode().Perm())}, nil
}

func requireRegularFile(path, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", description, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", description)
	}
	return nil
}

func validFile(file File) bool {
	if file.Size < 0 || file.Mode&0o111 == 0 || len(file.SHA256) != sha256.Size*2 || strings.ToLower(file.SHA256) != file.SHA256 {
		return false
	}
	_, err := hex.DecodeString(file.SHA256)
	return err == nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
