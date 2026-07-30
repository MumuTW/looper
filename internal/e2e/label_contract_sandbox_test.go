package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// fake-gh models two outcomes of `gh label create` on a name that already
// exists: without --force it fails, and with --force it updates the label in
// place. Looper depends on the first — it stopped passing --force so that
// applying a label cannot rewrite wording a maintainer chose — and the harness
// is what proves the gateway behaves that way.
//
// Nothing proved the model itself still matched GitHub. If the CLI stopped
// rejecting duplicates, every contract test would keep passing while the
// gateway silently lost the property it was changed to gain. This is the
// external check: it exercises the same two outcomes against a real
// repository, so the model has an authority outside itself.
//
// It is a mutation against a live repository, which is the boundary AGENTS.md
// reserves sandbox E2E for.
func TestGitHubSandboxLabelCreateDuplicateContract(t *testing.T) {
	sb := requireSandboxConfig(t)

	name := "looper-e2e-label-" + sb.RunID
	const (
		originalColor       = "b60205"
		originalDescription = "original wording, must survive a duplicate create"
		forcedColor         = "0e8a16"
		forcedDescription   = "rewritten by --force"
	)

	runSandboxCommandMust(t, "", sb.CmdEnv, "gh", "label", "create", name,
		"--repo", sb.Repo, "--color", originalColor, "--description", originalDescription)
	t.Cleanup(func() {
		// Best effort: the sandbox cleanup workflow does not know about labels,
		// and a leaked one would collide with nothing but still accumulate.
		_, _ = runSandboxCommand("", sb.CmdEnv, "gh", "label", "delete", name, "--repo", sb.Repo, "--yes")
	})

	t.Run("duplicate without force is rejected and changes nothing", func(t *testing.T) {
		output, err := runSandboxCommand("", sb.CmdEnv, "gh", "label", "create", name,
			"--repo", sb.Repo, "--color", forcedColor, "--description", forcedDescription)
		if err == nil {
			t.Fatalf("gh label create on an existing name succeeded; fake-gh models it as a failure\noutput=%s", output)
		}
		if !strings.Contains(strings.ToLower(output), "already exists") && !strings.Contains(strings.ToLower(output), "already_exists") {
			t.Fatalf("duplicate create failed with an unrecognised message; isLabelAlreadyExistsError matches on it\noutput=%s", output)
		}

		label := readSandboxLabel(t, sb, name)
		if label.Color != originalColor || label.Description != originalDescription {
			t.Fatalf("a rejected duplicate still changed the label: color=%q description=%q, want %q / %q",
				label.Color, label.Description, originalColor, originalDescription)
		}
	})

	t.Run("force updates in place", func(t *testing.T) {
		runSandboxCommandMust(t, "", sb.CmdEnv, "gh", "label", "create", name,
			"--repo", sb.Repo, "--color", forcedColor, "--description", forcedDescription, "--force")

		label := readSandboxLabel(t, sb, name)
		if label.Color != forcedColor || label.Description != forcedDescription {
			t.Fatalf("--force did not update the label: color=%q description=%q, want %q / %q",
				label.Color, label.Description, forcedColor, forcedDescription)
		}
	})
}

type sandboxLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func readSandboxLabel(tb testing.TB, sb sandboxConfig, name string) sandboxLabel {
	tb.Helper()
	output := runSandboxCommandMust(tb, "", sb.CmdEnv, "gh", "label", "list",
		"--repo", sb.Repo, "--limit", "1000", "--json", "name,color,description")
	var all []sandboxLabel
	if err := json.Unmarshal([]byte(output), &all); err != nil {
		tb.Fatalf("decode gh label list: %v\noutput=%s", err, output)
	}
	for _, label := range all {
		if strings.EqualFold(label.Name, name) {
			return label
		}
	}
	tb.Fatalf("label %q not found in the sandbox repository", name)
	return sandboxLabel{}
}
