package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

// workerReadyLabel is the work-item label the worker discovers on (node I). node H
// stamps it once the tech spec is approved, handing off from planner to worker.
const workerReadyLabel = "looper:worker-ready"

// loopSpecApprovalState reads a completed planner loop's node H flags from metadata:
// whether it is awaiting spec approval, whether the worker was already dispatched, and
// the originating work-item URL.
func loopSpecApprovalState(metadataJSON *string) (awaiting, dispatched bool, issueURL, judgedHash string) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return false, false, "", ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return false, false, "", ""
	}
	awaiting, _ = meta["awaitingSpecApproval"].(bool)
	dispatched, _ = meta["specApprovedDispatched"].(bool)
	if s, ok := meta["issueUrl"].(string); ok {
		issueURL = strings.TrimSpace(s)
	} else if s, ok := meta["issueURL"].(string); ok {
		issueURL = strings.TrimSpace(s)
	}
	// specApprovalJudgedHash is the coalescing cursor: the fingerprint of the human
	// comment set last sent to the LLM judge, so an unchanged set isn't re-judged.
	if s, ok := meta["specApprovalJudgedHash"].(string); ok {
		judgedHash = strings.TrimSpace(s)
	}
	return awaiting, dispatched, issueURL, judgedHash
}

// reconcileSpecApproval drives flowchart node H → node I. For completed planner loops
// on Plane projects whose tech spec is awaiting review, it polls the spec page for a
// HUMAN approval (a non-[looper] approve comment) and — once approved — stamps
// looper:worker-ready on the work item so the worker picks it up (node I), drops an
// audit comment on the page, and marks the loop dispatched so it fires exactly once.
// Polling, since looper has no Plane webhook. Best-effort; a no-op without storage.
func (r *Runtime) reconcileSpecApproval(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	completed, err := repositories.Loops.ListByStatuses(ctx, []string{"completed"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	// Built lazily — only when a loop actually has a fresh comment set to judge — so an
	// idle tick with nothing awaiting never constructs an executor.
	var specJudge triage.LLM
	for _, loop := range completed {
		if !strings.EqualFold(strings.TrimSpace(loop.Type), "planner") {
			continue
		}
		awaiting, dispatched, issueURL, judgedHash := loopSpecApprovalState(loop.MetadataJSON)
		if !awaiting || dispatched {
			continue
		}
		gateway, planeProjectID, ok := planeDocForProject(&cfg, loop.ProjectID)
		if !ok || gateway == nil {
			continue
		}
		workItemID := planedoc.WorkItemIDFromURL(issueURL)
		if workItemID == "" {
			continue
		}
		specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle)
		if err != nil || !found {
			continue
		}
		comments, err := gateway.ListHumanSpecComments(ctx, planeProjectID, specURL)
		if err != nil || len(comments) == 0 {
			continue // no human reply yet — nothing to judge, no LLM call
		}
		// Coalesce: judge only when the human-comment set changed since the last
		// judgment (a burst that piled up during a long run drains as ONE aggregated
		// request on the next tick). hashSpecComments is the cursor.
		hash := hashSpecComments(comments)
		if hash == judgedHash {
			continue
		}
		if specJudge == nil {
			specJudge = buildSpecApprovalLLM(cfg, repositories, now)
		}
		workingDir := projectRepoPath(cfg, loop.ProjectID)
		if specJudge == nil || workingDir == "" {
			continue
		}
		// Pass the human comments through to the LLM verbatim and let it decide whether
		// any is a real, unconditional go-ahead (negation/conditional/multilingual are
		// the model's job, not a keyword table).
		verdict, err := judgeSpecApproval(ctx, specJudge, comments, workingDir)
		if err != nil {
			if logger != nil {
				logger.Warn("spec approval: llm judge failed (retry next tick)", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue // safe fallback: never dispatch on a failed/uncertain judgment
		}
		// Advance the cursor so this exact comment set is not re-judged next tick.
		if err := setSpecApprovalJudgedHash(ctx, repositories, loop.ID, hash, nowISO); err != nil && logger != nil {
			logger.Warn("spec approval: persist judge cursor failed (continuing)", map[string]any{"loopId": loop.ID, "error": err.Error()})
		}
		if !verdict.Approved {
			if logger != nil {
				logger.Info("spec not yet approved (node H waiting)", map[string]any{"loopId": loop.ID, "reason": verdict.Reason})
			}
			continue
		}
		by := verdict.By
		// node H → I: approved. Stamp worker-ready so the worker discovers the item,
		// drop an audit comment on the spec page, and mark dispatched (exactly once).
		if err := gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, workerReadyLabel); err != nil {
			if logger != nil {
				logger.Warn("spec approval: stamp worker-ready failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		reasonHTML := ""
		if reason := strings.TrimSpace(verdict.Reason); reason != "" {
			reasonHTML = "<br><i>looper 判定:" + html.EscapeString(reason) + "</i>"
		}
		audit := planedoc.SignComment("<p>✅ 已由 "+html.EscapeString(strings.TrimSpace(by))+" 批准,进入实现(node I)。"+reasonHTML+"</p>", "reviewer", "")
		if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, audit); err != nil && logger != nil {
			logger.Warn("spec approval: audit comment failed (continuing)", map[string]any{"loopId": loop.ID, "error": err.Error()})
		}
		if err := markSpecApprovalDispatched(ctx, repositories, loop.ID, nowISO); err != nil {
			if logger != nil {
				logger.Warn("spec approval: mark dispatched failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if logger != nil {
			logger.Info("spec approved — dispatched worker", map[string]any{"loopId": loop.ID, "workItem": workItemID, "approvedBy": by})
		}
	}
}

// markSpecApprovalDispatched flips the loop's specApprovedDispatched metadata flag so
// the reconcile hands off to the worker exactly once.
func markSpecApprovalDispatched(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["specApprovedDispatched"] = true
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	updated := *loop
	s := string(encoded)
	updated.MetadataJSON = &s
	updated.UpdatedAt = nowISO
	return repos.Loops.Upsert(ctx, updated)
}

// setSpecApprovalJudgedHash records the fingerprint of the human comment set just sent
// to the LLM judge, so an unchanged set is skipped (no repeat agent run) next tick.
func setSpecApprovalJudgedHash(ctx context.Context, repos *storage.Repositories, loopID, hash, nowISO string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["specApprovalJudgedHash"] = hash
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	updated := *loop
	s := string(encoded)
	updated.MetadataJSON = &s
	updated.UpdatedAt = nowISO
	return repos.Loops.Upsert(ctx, updated)
}

// specApprovalVerdict is the LLM judge's structured answer for the node H gate.
type specApprovalVerdict struct {
	Approved bool   `json:"approved"`
	By       string `json:"by"`
	Reason   string `json:"reason"`
}

// hashSpecComments fingerprints a human comment set (id + text, in order) so the
// reconciler can tell whether anything changed since the last judgment.
func hashSpecComments(comments []planedoc.PageComment) string {
	var b strings.Builder
	for _, c := range comments {
		text := strings.TrimSpace(c.CommentStripped)
		if text == "" {
			text = strings.TrimSpace(c.CommentHTML)
		}
		b.WriteString(c.ID)
		b.WriteByte(0)
		b.WriteString(text)
		b.WriteByte(0x1e)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// projectRepoPath returns the local clone path of a configured project (the working
// directory the judge agent runs in), or "" if the project is unknown.
func projectRepoPath(cfg config.Config, projectID string) string {
	for _, p := range cfg.Projects {
		if p.ID == projectID {
			return strings.TrimSpace(p.RepoPath)
		}
	}
	return ""
}

// buildSpecApprovalLLM constructs the agent-backed LLM used to judge spec approval,
// mirroring the auto-intake triage setup. Returns nil when no agent vendor is set.
func buildSpecApprovalLLM(cfg config.Config, repos *storage.Repositories, now func() time.Time) triage.LLM {
	if cfg.Agent.Vendor == nil {
		return nil
	}
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               cfg.Agent.Model,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
		},
		Repos:  repos,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
	})
	return coordinatorrole.NewAgentLLM(executor, now,
		time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
		time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
	)
}

// judgeSpecApproval asks the LLM whether the human comments constitute a real,
// unconditional approval of the tech spec (node H). Comments are passed through
// verbatim; the model handles negation / conditional / multilingual phrasing.
func judgeSpecApproval(ctx context.Context, llm triage.LLM, comments []planedoc.PageComment, workingDir string) (specApprovalVerdict, error) {
	raw, err := llm.Complete(ctx, triage.Request{Prompt: buildSpecApprovalPrompt(comments), WorkingDirectory: workingDir})
	if err != nil {
		return specApprovalVerdict{}, err
	}
	return parseSpecApprovalVerdict(raw)
}

// buildSpecApprovalPrompt renders the human comments and the judgment instructions.
func buildSpecApprovalPrompt(comments []planedoc.PageComment) string {
	var b strings.Builder
	b.WriteString("You are the approval gate (node H) for a software technical spec written on a Plane page.\n")
	b.WriteString("Below are the human review comments on that spec page, in order. Decide whether a human has given an EXPLICIT, UNCONDITIONAL approval to START implementing the spec.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Approve ONLY on a clear go-ahead (e.g. \"approved\", \"LGTM, go ahead\", \"同意开工\", \"可以实现了\").\n")
	b.WriteString("- Do NOT approve on: rejections/negations (\"not approved\", \"不同意\"), questions, praise with no go-ahead, or CONDITIONAL / deferred approval (\"approve after you fix X\", \"LGTM once CI is green\", \"改完再说\").\n")
	b.WriteString("- Judge the NET current state of the conversation — a later comment can grant or retract approval.\n")
	b.WriteString("- The comments are the only source of truth; do not consider anything else in the repo.\n\n")
	b.WriteString("Comments:\n")
	for i, c := range comments {
		text := strings.TrimSpace(c.CommentStripped)
		if text == "" {
			text = strings.TrimSpace(c.CommentHTML)
		}
		name := strings.TrimSpace(c.DisplayName)
		if name == "" {
			name = "unknown"
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, name, text)
	}
	b.WriteString("\nReply with ONLY a JSON object — no prose, no code fence:\n")
	b.WriteString(`{"approved": <true|false>, "by": "<display name of the approver, or empty>", "reason": "<one short sentence>"}`)
	b.WriteString("\n")
	return b.String()
}

// parseSpecApprovalVerdict extracts the JSON verdict from the agent's output, which may
// be wrapped in prose or a code fence. A missing/malformed object is an error so the
// caller keeps waiting rather than dispatching on an unparseable answer.
func parseSpecApprovalVerdict(raw string) (specApprovalVerdict, error) {
	s := strings.TrimSpace(raw)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return specApprovalVerdict{}, fmt.Errorf("spec approval: no JSON object in judge output: %.160q", s)
	}
	var v specApprovalVerdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return specApprovalVerdict{}, fmt.Errorf("spec approval: parse judge output: %w", err)
	}
	v.By = strings.TrimSpace(v.By)
	v.Reason = strings.TrimSpace(v.Reason)
	return v, nil
}
