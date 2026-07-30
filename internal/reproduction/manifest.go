// Package reproduction owns the Reproduction Record: the durable, repository-
// carried statement of how a bug is observed to fail.
//
// The record exists in two bound forms. The authority is the
// `reproduction.recorded` event written by Reproducer before Planner is
// reached; the manifest committed at .looper/reproduction.json is that same
// record travelling with the branch, so any later Role can read it from its own
// worktree without a database lookup, an issue-number join, or a diff heuristic.
// The event carries the commit SHA the manifest was committed in, which is what
// binds the two.
//
// The manifest is deliberately generic. Looper runs against arbitrary
// repositories, so the reproduction is a *command* the repository supplies, not
// a Go test name.
package reproduction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestRelPath is where the Reproduction Record lives inside a worktree. It
// is committed, not a scratch sentinel: a reviewer reading the pull request can
// see exactly which command and which files carry the claim.
const ManifestRelPath = ".looper/reproduction.json"

// ManifestVersion is the current manifest schema version. An unknown version is
// rejected rather than best-effort decoded, because a gate that silently
// misreads its own authority is worse than one that refuses to run.
const ManifestVersion = 1

// FileHash pins one reproduction test file to the content it had at commit
// time. This is what makes tamper detection mechanical instead of a prompt
// instruction: Worker and Fixer cannot weaken the test and still pass the gate.
type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is the Reproduction Record as committed to the branch.
type Manifest struct {
	Version int    `json:"version"`
	Repo    string `json:"repo,omitempty"`
	// IssueNumber ties the record to the bug it reproduces.
	IssueNumber int64 `json:"issueNumber,omitempty"`
	// Command must fail on BaseSHA and pass once the bug is fixed. It runs
	// through the same sandbox as repository validation commands.
	Command string `json:"command"`
	// Files are the reproduction test files, hashed at commit time.
	Files []FileHash `json:"files"`
	// ObservedFailure is the failure mode the Reproducer actually saw, quoted
	// from the command output, so a human can check it against the Issue report.
	ObservedFailure string `json:"observedFailure,omitempty"`
	// BaseSHA is the commit the command was proven to fail on.
	BaseSHA string `json:"baseSha,omitempty"`
	// IdempotencyKey identifies the reproduction attempt that produced this
	// manifest. A resumed run reads it back off the branch to recognise its own
	// already-committed work instead of authoring a second reproduction commit.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// CommitSHA is filled in the event record, not here: the manifest is part of
	// the commit and cannot contain its own hash.
	RecordedAt string `json:"recordedAt,omitempty"`
}

// Draft is the subset of a Manifest the Reproducer agent authors. Hashes, base
// SHA and timestamp are filled by the daemon: an agent-supplied hash would
// authenticate nothing, since the same agent could supply a hash of whatever it
// wanted the file to look like.
type Draft struct {
	Version         int      `json:"version"`
	Command         string   `json:"command"`
	Files           []string `json:"files"`
	ObservedFailure string   `json:"observedFailure,omitempty"`
}

// ManifestPath returns the absolute manifest path inside a worktree.
func ManifestPath(worktreePath string) string {
	return filepath.Join(worktreePath, filepath.FromSlash(ManifestRelPath))
}

// ReadManifest loads the committed Reproduction Record from a worktree. The
// second return is false when the branch carries no reproduction at all, which
// is the normal case for every non-bug Issue and every project with the
// Reproducer disabled.
func ReadManifest(worktreePath string) (Manifest, bool, error) {
	raw, err := os.ReadFile(ManifestPath(worktreePath))
	if os.IsNotExist(err) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, fmt.Errorf("read reproduction manifest: %w", err)
	}
	manifest, err := DecodeManifest(raw)
	if err != nil {
		return Manifest{}, true, err
	}
	return manifest, true, nil
}

// DecodeManifest strictly decodes a manifest. Unknown fields are rejected so a
// manifest written by a newer Looper cannot be partially honoured by an older
// one.
func DecodeManifest(raw []byte) (Manifest, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode reproduction manifest: %w", err)
	}
	if manifest.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("decode reproduction manifest: unsupported version %d", manifest.Version)
	}
	if strings.TrimSpace(manifest.Command) == "" {
		return Manifest{}, fmt.Errorf("decode reproduction manifest: command is required")
	}
	if len(manifest.Files) == 0 {
		return Manifest{}, fmt.Errorf("decode reproduction manifest: at least one reproduction file is required")
	}
	return manifest, nil
}

// DecodeDraft strictly decodes the agent-authored draft manifest.
func DecodeDraft(raw []byte) (Draft, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var draft Draft
	if err := decoder.Decode(&draft); err != nil {
		return Draft{}, fmt.Errorf("decode reproduction draft: %w", err)
	}
	if draft.Version != ManifestVersion {
		return Draft{}, fmt.Errorf("decode reproduction draft: unsupported version %d", draft.Version)
	}
	if strings.TrimSpace(draft.Command) == "" {
		return Draft{}, fmt.Errorf("decode reproduction draft: command is required")
	}
	draft.Files = normalizePaths(draft.Files)
	if len(draft.Files) == 0 {
		return Draft{}, fmt.Errorf("decode reproduction draft: at least one reproduction file is required")
	}
	return draft, nil
}

// WriteManifest writes the manifest into the worktree, creating .looper/.
func WriteManifest(worktreePath string, manifest Manifest) error {
	path := ManifestPath(worktreePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create reproduction manifest directory: %w", err)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode reproduction manifest: %w", err)
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// HashFiles hashes each reproduction file relative to the worktree. A path that
// escapes the worktree, or does not exist, is an error: the record must name
// real, in-tree files or it cannot be re-checked later.
func HashFiles(worktreePath string, paths []string) ([]FileHash, error) {
	hashes := make([]FileHash, 0, len(paths))
	for _, rel := range normalizePaths(paths) {
		absolute, err := resolveInsideWorktree(worktreePath, rel)
		if err != nil {
			return nil, err
		}
		sum, err := hashFile(absolute)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, FileHash{Path: rel, SHA256: sum})
	}
	return hashes, nil
}

func hashFile(absolute string) (string, error) {
	contents, err := os.ReadFile(absolute)
	if err != nil {
		return "", fmt.Errorf("hash reproduction file: %w", err)
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

// resolveInsideWorktree rejects absolute paths and traversal. The manifest is
// agent-authored, so a path it names is untrusted input.
func resolveInsideWorktree(worktreePath, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("reproduction file path must be worktree-relative: %s", rel)
	}
	root, err := filepath.Abs(worktreePath)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(rel))
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("reproduction file path escapes the worktree: %s", rel)
	}
	return joined, nil
}

func normalizePaths(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		value = filepath.ToSlash(filepath.Clean(value))
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// PromptBlock renders the committed Reproduction Record as explicit Role input.
//
// Planner, Worker and Fixer are all given the reproduction's identity directly
// rather than having to re-derive the failing test from the diff — which is the
// guesswork the record exists to remove. The "do not edit" instruction is
// included as a convenience, not as the enforcement: the enforcement is the
// hash re-check in the completion gate.
func PromptBlock(manifest Manifest) string {
	if strings.TrimSpace(manifest.Command) == "" {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\n\n## Reproduction (committed on this branch)\n\n")
	builder.WriteString("This work is governed by a committed reproduction of the reported bug.\n\n")
	fmt.Fprintf(&builder, "- Reproduction command: `%s`\n", manifest.Command)
	for _, file := range manifest.Files {
		fmt.Fprintf(&builder, "- Reproduction test file: %s\n", file.Path)
	}
	if observed := strings.TrimSpace(manifest.ObservedFailure); observed != "" {
		fmt.Fprintf(&builder, "\nObserved failure before the fix:\n\n```\n%s\n```\n", observed)
	}
	builder.WriteString("\nThe change is complete only when that command passes and the repository's own validation suite still passes.\n")
	builder.WriteString("Do not edit, weaken, skip, or delete the reproduction files or `" + ManifestRelPath +
		"`. Their content is hashed and re-checked mechanically, and a mismatch fails the run.\n")
	return builder.String()
}
