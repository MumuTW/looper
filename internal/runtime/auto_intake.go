package runtime

import (
	"context"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/planedoc"
)

// Plane auto-intake labels. A colleague applies ONLY looper:auto; this reconciler
// classifies the work item and routes it into the EXISTING planner (looper:plan) /
// worker (looper:worker-ready) pipeline. It is the TOP of the looper:auto flowchart
// (classify → product-spec gate → route) — the segment the GitHub coordinator
// provides for GitHub issues but which the Plane pipeline lacked, so a single
// looper:auto label never has to be maintained by hand.
const (
	autoLabel        = "looper:auto"
	planTriggerLabel = "looper:plan"
	// workerReadyLabel ("looper:worker-ready") is defined in spec_approval.go (same package).

	dispatchImplementLabel = "dispatch/implement"
	kindFeatureLabel       = "kind/feature"

	// Intake hold/terminal markers, so classification runs at most once per item.
	intakeAwaitingProductLabel = "looper:awaiting-product-spec"
	intakeOutOfScopeLabel      = "looper:out-of-scope"
	intakeNeedsHumanLabel      = "looper:needs-human"

	autoIntakeEnvVar = "LOOPER_PLANE_AUTO_INTAKE"
)

// intakeRoute is the next action for a looper:auto item after classification.
type intakeRoute int

const (
	intakeSkip intakeRoute = iota
	intakeRouteToPlan
	intakeRouteToImplement
	intakeHoldForProductSpec
	intakeHoldUnclear
	intakeMarkOutOfScope
)

// decideAutoIntakeRoute maps a triage decision + product-spec presence to the next
// intake action (flowchart nodes B–F). Pure — unit-tested without any I/O.
//
//	out-of-scope                        → mark, stop
//	unclear                             → hold, @human       (拿不准 → HITL 问人)
//	valid + feature + no product spec   → hold, @product     (node E: 补 product spec)
//	valid + dispatch/implement          → worker directly    (简单 bug: 直接修)
//	valid + dispatch/plan (or default)  → planner writes spec (需求 / 复杂 bug)
func decideAutoIntakeRoute(decision triage.Decision, hasProductSpec bool) intakeRoute {
	if decision.NoOp {
		return intakeSkip
	}
	switch decision.Disposition {
	case triage.DispositionOutOfScope:
		return intakeMarkOutOfScope
	case triage.DispositionUnclear:
		return intakeHoldUnclear
	}
	if labelsContainFold(decision.ApplyLabels, kindFeatureLabel) && !hasProductSpec {
		return intakeHoldForProductSpec
	}
	if labelsContainFold(decision.ApplyLabels, dispatchImplementLabel) {
		return intakeRouteToImplement
	}
	// dispatch/plan, or any other valid disposition → spec-first (safe default).
	return intakeRouteToPlan
}

func autoIntakeEnabled() bool {
	return strings.TrimSpace(os.Getenv(autoIntakeEnvVar)) == "1"
}

func labelsContainFold(labels []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

func labelNames(labels []forge.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if n := strings.TrimSpace(l.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// reconcileAutoIntake drives the top of the looper:auto flowchart for Plane
// projects. Env-gated (LOOPER_PLANE_AUTO_INTAKE=1) so it is zero-risk to existing
// deployments until enabled. Each looper:auto work item is classified once via the
// shared triage.Decide; the resulting dispatch decision stamps looper:plan or
// looper:worker-ready and the existing planner/worker discovery takes it from there.
func (r *Runtime) reconcileAutoIntake(ctx context.Context) {
	if !autoIntakeEnabled() {
		return
	}
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil || cfg.Agent.Vendor == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               cfg.Agent.Model,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
		},
		Repos:  repositories,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
	})
	llm := coordinatorrole.NewAgentLLM(executor, now,
		time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
		time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
	)
	for _, project := range cfg.Projects {
		if config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindPlane {
			continue
		}
		r.reconcileAutoIntakeProject(ctx, &cfg, project, llm, logger, now)
	}
}

func (r *Runtime) reconcileAutoIntakeProject(ctx context.Context, cfg *config.Config, project config.ProjectRefConfig, llm triage.LLM, logger bootstrap.Logger, now func() time.Time) {
	gateway, planeProjectID, ok := planeDocForProject(cfg, project.ID)
	if !ok || gateway == nil {
		return
	}
	provider, found := forgejoProviderByID(*cfg, project.Provider)
	if !found {
		return
	}
	client, err := forge.NewPlaneClientFromConfig(provider, project.Repo)
	if err != nil {
		if logger != nil {
			logger.Warn("auto-intake: plane client build failed", map[string]any{"projectId": project.ID, "error": err.Error()})
		}
		return
	}
	items, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: []string{autoLabel}})
	if err != nil {
		if logger != nil {
			logger.Warn("auto-intake: list looper:auto items failed", map[string]any{"projectId": project.ID, "error": err.Error()})
		}
		return
	}
	for _, item := range items {
		r.reconcileAutoIntakeItem(ctx, gateway, client, planeProjectID, project, item, llm, logger, now)
	}
}

func (r *Runtime) reconcileAutoIntakeItem(ctx context.Context, gateway *planedoc.Gateway, client *forge.PlaneClient, planeProjectID string, project config.ProjectRefConfig, item forge.Issue, llm triage.LLM, logger bootstrap.Logger, now func() time.Time) {
	names := labelNames(item.Labels)
	// Already routed into the pipeline — nothing to do.
	if labelsContainFold(names, planTriggerLabel) || labelsContainFold(names, workerReadyLabel) {
		return
	}
	// Terminal intake states that need a human, not another classification.
	if labelsContainFold(names, intakeOutOfScopeLabel) || labelsContainFold(names, intakeNeedsHumanLabel) {
		return
	}
	workItemID := planedoc.WorkItemIDFromURL(item.HTMLURL)
	if workItemID == "" {
		return
	}

	// Held awaiting a product spec: re-check the gate WITHOUT re-classifying (we
	// already know it is a feature). When the spec appears, drop the hold and route
	// to the planner.
	if labelsContainFold(names, intakeAwaitingProductLabel) {
		present, _, err := gateway.HasProductSpec(ctx, planeProjectID, workItemID)
		if err != nil || !present {
			return
		}
		_ = gateway.RemoveWorkItemLabel(ctx, planeProjectID, workItemID, intakeAwaitingProductLabel)
		r.routeIntake(ctx, gateway, planeProjectID, workItemID, planTriggerLabel, "<p>✅ product spec 已补齐,进入技术方案(planner)。</p>", logger, project.ID, item.Number)
		return
	}

	// Fresh looper:auto item — classify once (flowchart node A/B).
	comments := intakeComments(ctx, client, item.Number)
	decision := triage.Decide(ctx, llm, triage.Input{
		Issue: triage.Issue{
			Number:   item.Number,
			Title:    item.Title,
			Body:     item.Body,
			URL:      item.HTMLURL,
			Labels:   names,
			Comments: comments,
		},
		RepoContext: triage.RepoContext{Repo: project.Repo, WorkingDirectory: project.RepoPath},
		Config:      triage.Config{OutOfScopeLabel: intakeOutOfScopeLabel, UnclearLabel: intakeNeedsHumanLabel},
		Now:         now().UTC(),
	})

	present, _, _ := gateway.HasProductSpec(ctx, planeProjectID, workItemID)
	route := decideAutoIntakeRoute(decision, present)

	// Stamp the LLM's audit labels (kind/*, complexity/*, dispatch/*) so the
	// classification is visible on the item, mirroring the GitHub coordinator.
	if len(decision.ApplyLabels) > 0 {
		if _, err := client.AddIssueLabels(ctx, item.Number, decision.ApplyLabels); err != nil && logger != nil {
			logger.Warn("auto-intake: stamp audit labels failed", map[string]any{"projectId": project.ID, "item": item.Number, "error": err.Error()})
		}
	}

	switch route {
	case intakeRouteToPlan:
		r.routeIntake(ctx, gateway, planeProjectID, workItemID, planTriggerLabel, intakeComment(decision, "技术方案(planner)"), logger, project.ID, item.Number)
	case intakeRouteToImplement:
		r.routeIntake(ctx, gateway, planeProjectID, workItemID, workerReadyLabel, intakeComment(decision, "直接实现(worker)"), logger, project.ID, item.Number)
	case intakeHoldForProductSpec:
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeAwaitingProductLabel)
		// Plane-side node E ask. The Feishu @product card is a separate surface driven
		// off the same hold label once the item is picked up downstream.
		if err := gateway.RequestProductSpec(ctx, planeProjectID, workItemID, "产品负责人", item.Title); err != nil && logger != nil {
			logger.Warn("auto-intake: request product spec failed", map[string]any{"projectId": project.ID, "item": item.Number, "error": err.Error()})
		}
		r.logIntake(logger, project.ID, item.Number, "hold: awaiting product spec")
	case intakeHoldUnclear:
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeNeedsHumanLabel)
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, intakeComment(decision, "拿不准,等人确认(HITL)"))
		r.logIntake(logger, project.ID, item.Number, "hold: unclear → needs-human")
	case intakeMarkOutOfScope:
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeOutOfScopeLabel)
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, intakeComment(decision, "超出范围,已标记"))
		r.logIntake(logger, project.ID, item.Number, "out-of-scope")
	default:
		// NoOp / classification unavailable — leave the item untouched for a retry.
	}
}

// routeIntake stamps the pipeline trigger label (looper:plan or looper:worker-ready)
// and drops an audit comment. The existing planner/worker discovery picks it up.
func (r *Runtime) routeIntake(ctx context.Context, gateway *planedoc.Gateway, planeProjectID, workItemID, triggerLabel, commentHTML string, logger bootstrap.Logger, projectID string, number int64) {
	if err := gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, triggerLabel); err != nil {
		if logger != nil {
			logger.Warn("auto-intake: stamp trigger label failed", map[string]any{"projectId": projectID, "item": number, "label": triggerLabel, "error": err.Error()})
		}
		return
	}
	if strings.TrimSpace(commentHTML) != "" {
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, commentHTML)
	}
	r.logIntake(logger, projectID, number, "routed → "+triggerLabel)
}

func (r *Runtime) logIntake(logger bootstrap.Logger, projectID string, number int64, outcome string) {
	if logger != nil {
		logger.Info("auto-intake", map[string]any{"projectId": projectID, "item": number, "outcome": outcome})
	}
}

func intakeComments(ctx context.Context, client *forge.PlaneClient, number int64) []triage.Comment {
	raw, err := client.ListIssueComments(ctx, number)
	if err != nil {
		return nil
	}
	out := make([]triage.Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, triage.Comment{ID: c.ID, Author: c.User.Login, Body: c.Body, CreatedAt: c.UpdatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out
}

func intakeComment(decision triage.Decision, routeText string) string {
	summary := strings.TrimSpace(decision.CommentBody)
	if summary == "" {
		summary = "已分类。"
	}
	return fmt.Sprintf("<p>🧭 looper 分类:%s → %s</p>", html.EscapeString(summary), html.EscapeString(routeText))
}
