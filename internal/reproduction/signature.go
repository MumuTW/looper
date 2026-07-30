package reproduction

import (
	"fmt"
	"os"
	"strings"
	"unicode"
)

// maxSignatureFieldBytes bounds each half of the expected-failure signature.
//
// The signature is the only part of the command's output that is persisted into
// the committed manifest, so it is a narrow, validated field rather than a
// diagnostics dump. The sandbox is credential-free but has read access to the
// linked Git common directory, tool paths and module caches, so a wide
// verbatim-output field is an exfiltration channel through a repository the
// daemon then pushes.
const maxSignatureFieldBytes = 200

// ExpectedFailure is the structured, test-specific signature of the failure the
// reproduction claims.
//
// A bare non-zero exit is not proof of a reported bug: a syntax error, a failed
// setup step, or an unrelated already-failing test all exit non-zero, and a
// Worker that repairs any of those turns the command green without fixing
// anything. The signature narrows "the command failed" to "this specific test
// failed for this specific reason", and both halves are checked against
// artifacts the agent does not control at check time:
//
//   - Test must appear in the content of a declared reproduction file, so the
//     signature names something the reproduction actually defines rather than an
//     arbitrary string chosen to match whatever the command happened to print.
//   - Test and Message must both appear in the observed command output, so the
//     command demonstrably reached and failed that test.
//
// Neither check makes the agent's claim self-certifying: the daemon holds the
// file contents and the command output, and the agent authored the claim before
// either existed.
type ExpectedFailure struct {
	// Test identifies the test the reproduction adds — a test function name, a
	// spec name, a test id: whatever the repository's runner prints.
	Test string `json:"test"`
	// Message is a verbatim fragment of the failure output that identifies this
	// bug's failure, such as the failing assertion message or the panic line.
	Message string `json:"message"`
}

// IsZero reports whether no signature was recorded at all.
func (e ExpectedFailure) IsZero() bool {
	return strings.TrimSpace(e.Test) == "" && strings.TrimSpace(e.Message) == ""
}

// Normalize trims both halves.
func (e ExpectedFailure) Normalize() ExpectedFailure {
	return ExpectedFailure{Test: strings.TrimSpace(e.Test), Message: strings.TrimSpace(e.Message)}
}

// Validate enforces the shape of a signature that will be committed. Both
// halves are required, single-line, printable, and bounded: this is the
// "narrowly validated" part of keeping raw diagnostics out of the repository.
func (e ExpectedFailure) Validate() error {
	e = e.Normalize()
	if err := validateSignatureField("test", e.Test); err != nil {
		return err
	}
	return validateSignatureField("message", e.Message)
}

func validateSignatureField(name, value string) error {
	if value == "" {
		return fmt.Errorf("expected failure signature: %s is required", name)
	}
	if len(value) > maxSignatureFieldBytes {
		return fmt.Errorf("expected failure signature: %s exceeds %d bytes", name, maxSignatureFieldBytes)
	}
	for _, r := range value {
		// Rejecting control characters keeps the signature to one printable line:
		// it is committed to the repository and rendered into prompts and HITL
		// questions, and a multi-line field is how "a short quote" becomes "the
		// whole log" again.
		if r != '\t' && unicode.IsControl(r) {
			return fmt.Errorf("expected failure signature: %s must be a single printable line", name)
		}
	}
	return nil
}

// matchesOutput reports whether both halves of the signature appear in the
// observed command output. The comparison is whitespace-normalized and
// case-insensitive so a re-run differing only in indentation or line endings
// still confirms the same failure, while an unrelated failure does not.
func (e ExpectedFailure) matchesOutput(output string) bool {
	e = e.Normalize()
	if e.Test == "" || e.Message == "" {
		return false
	}
	normalized := normalizeForMatch(output)
	return strings.Contains(normalized, normalizeForMatch(e.Test)) &&
		strings.Contains(normalized, normalizeForMatch(e.Message))
}

// declaredByFiles reports whether the signature's test identifier appears in the
// content of at least one declared reproduction file.
//
// This is the check that keeps the signature from being a free assertion. The
// agent picks the string, but it must also be the string its own committed test
// file contains — and that file's content is hashed into the record, so it
// cannot be changed later without failing the integrity check.
func (e ExpectedFailure) declaredByFiles(worktreePath string, files []FileHash) (bool, error) {
	test := normalizeForMatch(e.Test)
	if test == "" {
		return false, nil
	}
	for _, file := range files {
		absolute, err := resolveInsideWorktree(worktreePath, file.Path)
		if err != nil {
			return false, err
		}
		contents, err := readBoundedFile(absolute)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		if strings.Contains(normalizeForMatch(contents), test) {
			return true, nil
		}
	}
	return false, nil
}

func normalizeForMatch(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
