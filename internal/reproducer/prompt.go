package reproducer

import (
	"fmt"
	"strings"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reproduction"
)

// buildReproducerPrompt asks for a failing reproduction and nothing else.
//
// The prompt is generic on purpose: Looper runs against arbitrary
// repositories, so the durable artifact is a *command* the repository supplies,
// not a Go test name. The prompt does not ask the agent to assert that the
// reproduction fails — the daemon runs the command and observes that itself.
func buildReproducerPrompt(repo string, issue githubinfra.IssueDetail) string {
	return fmt.Sprintf(`You are Looper's Reproducer. Your only job is to turn this bug report into an executable failure. Do not fix the bug. Do not refactor. Do not commit.

Repository: %s
Issue: #%d %s

%s

Work in this worktree. Read enough of the repository to find the project's own test conventions and use them.

If you can make the reported bug fail:
1. Add or extend a test that fails *because of the reported bug*, using the repository's existing test layout and style.
2. Write %s with exactly this JSON and no other fields:

{"version":%d,"command":"<the command that runs the reproduction>","files":["<path>","<path>"],"observedFailure":"<the failure you observed, quoted from the command output>"}

   - "command" must run from the repository root, must exercise the new test, and must exit non-zero *today* and exit zero once the bug is fixed. Keep it as narrow as the repository's tooling allows.
   - "files" must list every file that carries the reproduction, worktree-relative.
   - Prefer a reproduction that does not turn the repository's default validation suite red, using whatever quarantine, tag, or opt-in mechanism the repository already has. If the repository has none, that is acceptable — say so in "observedFailure".

If you cannot make the reported bug fail, that is a legitimate and useful outcome. Do not guess and do not invent a test that passes. Write %s with exactly this JSON and no other fields:

{"version":%d,"attempted":["<what you tried>"],"observedInstead":"<what actually happened>","missingInformation":["<what you would need>"],"summary":"<one sentence>"}

Write exactly one of those two files, never both. The command you record will be executed by the daemon and must be observed failing before the reproduction is accepted; a command that passes immediately is rejected.`,
		repo, issue.Number, strings.TrimSpace(issue.Title), strings.TrimSpace(issue.Body),
		reproduction.ManifestRelPath, reproduction.ManifestVersion,
		CannotReproduceRelPath, reproduction.ManifestVersion)
}
