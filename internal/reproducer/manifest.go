// Package reproducer defines the durable contract for a bug reproduction
// artifact.  The manifest is intentionally repository-local: a future
// Reproducer role can commit it next to the failing test, while Worker and
// Fixer can verify the same authority without inventing a second database
// record or trusting an agent's completion claim.
package reproducer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ManifestPath is the repository-relative location of a reproduction
	// manifest.  It is optional so repositories without a reproduction keep
	// today's Worker/Fixer behavior.
	ManifestPath = ".looper/reproducer.json"

	manifestVersion  = 1
	maxManifestBytes = 16 << 10
	maxCommandBytes  = 8 << 10
	maxIdentityBytes = 512
)

// Manifest is the durable reproduction authority. TestSHA256 binds the
// named test file to the reproduction commit; it is not an agent-produced
// assertion that the bug was fixed.
type Manifest struct {
	Version     int    `json:"version"`
	TestPath    string `json:"testPath"`
	TestName    string `json:"testName"`
	TestCommand string `json:"testCommand"`
	TestSHA256  string `json:"testSha256"`
}

// Parse decodes and validates a manifest without accepting unknown fields or
// trailing JSON. Strict decoding keeps the committed contract reviewable and
// prevents silently ignored fields from becoming an accidental authority.
func Parse(data []byte) (Manifest, error) {
	if len(data) == 0 {
		return Manifest{}, errors.New("reproduction manifest is empty")
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("reproduction manifest exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode reproduction manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("reproduction manifest contains trailing JSON")
		}
		return Manifest{}, fmt.Errorf("decode trailing reproduction manifest data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest.normalized(), nil
}

// Validate checks the manifest's shape. It does not inspect the repository;
// Verify performs the content-addressed check against a worktree.
func (m Manifest) Validate() error {
	if m.Version != manifestVersion {
		return fmt.Errorf("unsupported reproduction manifest version %d", m.Version)
	}
	path := strings.TrimSpace(m.TestPath)
	if path == "" {
		return errors.New("reproduction manifest testPath is required")
	}
	if filepath.IsAbs(path) {
		return errors.New("reproduction manifest testPath must be repository-relative")
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("reproduction manifest testPath escapes repository root")
	}
	if filepath.ToSlash(clean) != path {
		return fmt.Errorf("reproduction manifest testPath is not normalized: %s", m.TestPath)
	}
	if strings.TrimSpace(m.TestName) == "" {
		return errors.New("reproduction manifest testName is required")
	}
	if len(m.TestName) > maxIdentityBytes {
		return fmt.Errorf("reproduction manifest testName exceeds %d bytes", maxIdentityBytes)
	}
	command := strings.TrimSpace(m.TestCommand)
	if command == "" {
		return errors.New("reproduction manifest testCommand is required")
	}
	if len(command) > maxCommandBytes {
		return fmt.Errorf("reproduction manifest testCommand exceeds %d bytes", maxCommandBytes)
	}
	hash := strings.TrimSpace(m.TestSHA256)
	if len(hash) != sha256.Size*2 {
		return errors.New("reproduction manifest testSha256 must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("reproduction manifest testSha256 is not valid hex: %w", err)
	}
	return nil
}

func (m Manifest) normalized() Manifest {
	m.TestPath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(m.TestPath))))
	m.TestName = strings.TrimSpace(m.TestName)
	m.TestCommand = strings.TrimSpace(m.TestCommand)
	m.TestSHA256 = strings.ToLower(strings.TrimSpace(m.TestSHA256))
	return m
}

// Equal compares normalized manifest values. It is used to detect an agent
// replacing the manifest with a different test or hash during a run.
func (m Manifest) Equal(other Manifest) bool {
	return m.normalized() == other.normalized()
}

// Load reads the optional manifest from a repository root. A missing manifest
// returns (nil, nil); malformed, oversized, symlinked, or non-regular files
// fail closed.
func Load(repositoryRoot string) (*Manifest, error) {
	if strings.TrimSpace(repositoryRoot) == "" {
		// Some checkpoint-only paths (for example a legacy test fixture before a
		// worktree is prepared) have no repository root yet. There cannot be a
		// discoverable manifest in that state, so preserve the optional contract.
		return nil, nil
	}
	path, err := safeRepositoryPath(repositoryRoot, ManifestPath)
	if err != nil {
		return nil, err
	}
	data, err := readRegularFile(path, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read reproduction manifest: %w", err)
	}
	manifest, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Verify confirms that the manifest itself and its named test file are
// unchanged in repositoryRoot. It is deliberately independent of validation
// commands: a test can be tampered with even when the full suite remains green.
func (m Manifest) Verify(repositoryRoot string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	current, err := Load(repositoryRoot)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("reproduction manifest is missing")
	}
	if !m.Equal(*current) {
		return errors.New("reproduction manifest changed during run")
	}
	testPath, err := safeRepositoryPath(repositoryRoot, m.TestPath)
	if err != nil {
		return err
	}
	data, err := readRegularFile(testPath, false)
	if err != nil {
		return fmt.Errorf("read reproduction test %s: %w", m.TestPath, err)
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), m.TestSHA256) {
		return fmt.Errorf("reproduction test %s was modified", m.TestPath)
	}
	return nil
}

// PromptInstruction renders the minimum explicit input that coding roles
// need. The role still treats Verify as the authority; this text is guidance,
// not a substitute for the mechanical gate.
func (m Manifest) PromptInstruction() string {
	return strings.Join([]string{
		"Reproduction contract (daemon-verified):",
		fmt.Sprintf("- Test identity: %s (%s)", m.TestName, m.TestPath),
		fmt.Sprintf("- Reproduction command: %s", m.TestCommand),
		"- Do not modify, delete, rename, or regenerate the reproduction test or manifest.",
		"- Run the named reproduction command as part of validation; Looper will verify the committed test hash mechanically.",
	}, "\n")
}

func safeRepositoryPath(repositoryRoot, relative string) (string, error) {
	if strings.TrimSpace(repositoryRoot) == "" {
		return "", errors.New("repository root is required")
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("reproduction path must be repository-relative: %s", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("reproduction path escapes repository root: %s", relative)
	}
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			// The caller can turn a missing final file into the optional
			// (nil, nil) result; do not require the manifest directory to exist.
			return filepath.Join(root, clean), nil
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path component is a symlink: %s", current)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("path component is not a directory: %s", current)
		}
	}
	return current, nil
}

func readRegularFile(path string, bounded bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("path is a symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file: %s", path)
	}
	if bounded && info.Size() > maxManifestBytes {
		return nil, fmt.Errorf("reproduction manifest exceeds %d bytes", maxManifestBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limit := int64(^uint(0) >> 1)
	if bounded {
		limit = maxManifestBytes + 1
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, err
	}
	if bounded && len(data) > maxManifestBytes {
		return nil, fmt.Errorf("reproduction manifest exceeds %d bytes", maxManifestBytes)
	}
	return data, nil
}
