package reproducer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
)

// runAgent starts the reproduction agent session in the prepared worktree.
func (r *Runner) runAgent(ctx context.Context, project storage.ProjectRecord, repo string, issue githubinfra.IssueDetail, worktree worktreeContext) (planner.AgentResult, error) {
	executionID := eventlog.NewEventID("agent")
	execution, err := r.agentExecutor.Start(ctx, planner.AgentRunInput{
		ExecutionID: executionID, ProjectID: project.ID,
		Prompt: buildReproducerPrompt(repo, issue), WorkingDirectory: worktree.Path,
		Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout,
		Metadata: map[string]any{
			"loopType": "reproducer", "repo": repo, "issueNumber": issue.Number,
			"branch": worktree.Branch,
		},
		IdempotencyKey: fmt.Sprintf("reproducer:%s:%s#%d", project.ID, strings.ToLower(repo), issue.Number),
	})
	if err != nil {
		return planner.AgentResult{}, err
	}
	return execution.Wait(ctx)
}

// adoptCommittedReproduction recognises a reproduction this attempt already
// committed but never acknowledged.
//
// The manifest carries the attempt's idempotency key, so "did I already do
// this?" is answered from the branch itself rather than from a lock or a
// second durable state machine. A dirty worktree is not adopted: only work
// that reached a commit counts.
func (r *Runner) adoptCommittedReproduction(ctx context.Context, project storage.ProjectRecord, repo string, issue githubinfra.IssueDetail, worktree worktreeContext, target candidate) (reproduction.Record, bool, error) {
	manifest, present, err := reproduction.ReadManifest(worktree.Path)
	if err != nil || !present {
		// A malformed manifest is not adoptable; the attempt re-authors.
		return reproduction.Record{}, false, nil
	}
	if manifest.IdempotencyKey != target.IdempotencyKey {
		return reproduction.Record{}, false, nil
	}
	head, err := r.git.InspectHead(ctx, planner.InspectHeadInput{
		RepoPath: project.RepoPath, WorktreeRoot: worktree.Root, WorktreePath: worktree.Path,
	})
	if err != nil {
		return reproduction.Record{}, false, err
	}
	if head.HasUncommittedChanges {
		return reproduction.Record{}, false, nil
	}
	return reproduction.Record{
		ProjectID: project.ID, Repo: repo, IssueNumber: issue.Number, Branch: worktree.Branch,
		Command: manifest.Command, Files: manifest.Files, CommitSHA: head.HeadSHA,
		BaseSHA: manifest.BaseSHA, ObservedFailure: manifest.ObservedFailure,
		IdempotencyKey: target.IdempotencyKey, RecordedAt: r.nowISO(),
	}, true, nil
}

// recordUnreproducible persists the structured cannot-reproduce record and
// parks the Issue for a human.
//
// It is deliberately not a failure: no attempt counter moves, no retry is
// scheduled, and nothing reads as a crash. The Issue stops before Planner is
// reached, which is the whole reason the Role runs first.
func (r *Runner) recordUnreproducible(ctx context.Context, project storage.ProjectRecord, repo string, issue githubinfra.IssueDetail, target candidate, record reproduction.Unreproducible, result *DiscoveryResult) error {
	loop, err := r.planner.ParkIssueForHuman(ctx, planner.ParkIssueInput{
		Project: project, Repo: repo, Authority: target.IdempotencyKey,
		Issue: planner.IssueSummary{
			Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL,
			Assignees: append([]string(nil), issue.Assignees...), Labels: append([]string(nil), issue.Labels...),
		},
		Ask: loops.HITLAsk{
			Question:          buildCannotReproduceQuestion(repo, issue, record),
			Options:           []string{AnswerProceed, AnswerReject},
			RecommendedOption: AnswerReject,
			Recommendation:    "Reproducer could not make this bug fail on the current base, so planning would be against a description rather than a demonstrated failure.",
			Consequences: map[string]string{
				AnswerProceed: "Planner proceeds with no reproduction; completion falls back to the repository suite plus review judgement.",
				AnswerReject:  "The Issue stays stopped until someone supplies the missing information.",
			},
			Confidence: "medium",
		},
	})
	if err != nil {
		return err
	}
	record.IdempotencyKey = target.IdempotencyKey
	record.ProjectID = project.ID
	record.Repo = repo
	record.IssueNumber = issue.Number
	record.LoopID = loop.ID
	record.RecordedAt = r.nowISO()
	if err := reproduction.AppendUnreproducible(ctx, r.repos, record); err != nil {
		return err
	}
	result.Unreproducible++
	return nil
}

func buildCannotReproduceQuestion(repo string, issue githubinfra.IssueDetail, record reproduction.Unreproducible) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Cannot reproduce %s#%d (%s).\n\n", repo, issue.Number, strings.TrimSpace(issue.Title))
	if summary := strings.TrimSpace(record.Summary); summary != "" {
		builder.WriteString(summary + "\n\n")
	}
	if len(record.Attempted) > 0 {
		builder.WriteString("Attempted:\n")
		for _, attempt := range record.Attempted {
			builder.WriteString("- " + attempt + "\n")
		}
		builder.WriteString("\n")
	}
	if observed := strings.TrimSpace(record.ObservedInstead); observed != "" {
		builder.WriteString("Observed instead:\n" + observed + "\n\n")
	}
	if len(record.MissingInformation) > 0 {
		builder.WriteString("Missing information:\n")
		for _, missing := range record.MissingInformation {
			builder.WriteString("- " + missing + "\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

func unreproducibleFrom(declined CannotReproduce) reproduction.Unreproducible {
	return reproduction.Unreproducible{
		Attempted:          declined.Attempted,
		ObservedInstead:    strings.TrimSpace(declined.ObservedInstead),
		MissingInformation: declined.MissingInformation,
		Summary:            strings.TrimSpace(declined.Summary),
	}
}

func readDraft(worktreePath string) (reproduction.Draft, bool, error) {
	raw, err := os.ReadFile(reproduction.ManifestPath(worktreePath))
	if os.IsNotExist(err) {
		return reproduction.Draft{}, false, nil
	}
	if err != nil {
		return reproduction.Draft{}, false, err
	}
	draft, err := reproduction.DecodeDraft(raw)
	if err != nil {
		// A manifest the daemon cannot decode is not a reproduction. Treat it as
		// absent so the caller records a cannot-reproduce rather than trusting a
		// half-parsed claim.
		return reproduction.Draft{}, false, nil
	}
	return draft, true, nil
}

func readCannotReproduce(worktreePath string) (CannotReproduce, bool, error) {
	path := filepath.Join(worktreePath, filepath.FromSlash(CannotReproduceRelPath))
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return CannotReproduce{}, false, nil
	}
	if err != nil {
		return CannotReproduce{}, false, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var declined CannotReproduce
	if err := decoder.Decode(&declined); err != nil {
		return CannotReproduce{
			Summary:         "The reproduction agent wrote an undecodable cannot-reproduce record.",
			ObservedInstead: strings.TrimSpace(string(raw)),
		}, true, nil
	}
	return declined, true, nil
}

func parseJSONObject(value *string) map[string]any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return map[string]any{}
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(*value), &parsed); err != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}
