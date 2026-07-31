package dispatch

import (
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/coordinator/depgraph"
	"github.com/nexu-io/looper/internal/labels"
)

const (
	ModeHumanGated = "human-gated"
	ModeAutonomous = "autonomous"

	DispatchPlan      = labels.DispatchPlan
	DispatchImplement = labels.DispatchImplement

	ReactionSuccess = "+1"
	ReactionFailure = "confused"
)

type Comment struct {
	ID                int64
	Author            string
	AuthorAssociation string
	HasWriteAccess    bool
	Body              string
	CreatedAt         time.Time
}

type Issue struct {
	Number    int64
	Labels    []string
	Comments  []Comment
	TriagedAt time.Time
}

type Config struct {
	Mode                 string
	TriagedLabel         string
	HoldLabel            string
	AutonomousDelay      time.Duration
	AllowedUsers         []string
	SlashCommands        []string
	AssignTo             string
	PlannerTriggerLabels []string
	WorkerTriggerLabels  []string
	Namespace            labels.Namespace
}

type Action struct {
	NoOp               bool
	TriggerLabels      []string
	AssignTo           string
	ReactionCommentID  int64
	ReactionContent    string
	FailureCommentBody string
}

func Decide(issue Issue, cfg Config, now time.Time, graph *depgraph.DependencyGraph) Action {
	if cfg.Mode == ModeAutonomous {
		return decideAutonomous(issue, cfg, now, graph)
	}
	return decideHumanGated(issue, cfg, graph)
}

func NeedsDependencyGate(issue Issue, cfg Config, now time.Time) bool {
	if cfg.Mode == ModeAutonomous {
		return autonomousNeedsDependencyGate(issue, cfg, now)
	}
	return humanNeedsDependencyGate(issue, cfg)
}

func humanNeedsDependencyGate(issue Issue, cfg Config) bool {
	_, command, ok := latestCommandAttempt(issue.Comments, cfg.SlashCommands, cfg.AllowedUsers)
	if !ok || !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return false
	}
	dispatchLabel, ok := singleDispatchLabelForNamespace(issue.Labels, cfg.Namespace)
	if !ok || !labels.Has([]string{dispatchLabel}, commandDispatchLabelForNamespace(command, cfg.Namespace)) {
		return false
	}
	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 {
		return false
	}
	return len(missingLabels(issue.Labels, triggerLabels)) > 0
}

func autonomousNeedsDependencyGate(issue Issue, cfg Config, now time.Time) bool {
	if !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return false
	}
	dispatchLabel, ok := singleDispatchLabelForNamespace(issue.Labels, cfg.Namespace)
	if !ok {
		return false
	}
	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 || autonomousDispatchHeldForNamespace(issue.Labels, cfg.HoldLabel, cfg.Namespace) || len(missingLabels(issue.Labels, triggerLabels)) == 0 {
		return false
	}
	if issue.TriagedAt.IsZero() || now.UTC().Before(issue.TriagedAt.UTC().Add(cfg.AutonomousDelay)) {
		return false
	}
	return true
}

func decideHumanGated(issue Issue, cfg Config, graph *depgraph.DependencyGraph) Action {
	comment, command, ok := latestCommandAttempt(issue.Comments, cfg.SlashCommands, cfg.AllowedUsers)
	if !ok {
		return Action{NoOp: true}
	}

	action := Action{ReactionCommentID: comment.ID}
	if !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return fail(action, "Coordinator can't dispatch until triage finishes.")
	}

	dispatchLabel, ok := singleDispatchLabelForNamespace(issue.Labels, cfg.Namespace)
	if !ok {
		return fail(action, "Coordinator can't dispatch because triage did not set a dispatch label.")
	}
	if !labels.Has([]string{dispatchLabel}, commandDispatchLabelForNamespace(command, cfg.Namespace)) {
		return fail(action, "Coordinator can't dispatch because the slash command does not match triage.")
	}

	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 {
		return fail(action, "Coordinator can't dispatch because the trigger label is not configured.")
	}
	missingLabels := missingLabels(issue.Labels, triggerLabels)
	if len(missingLabels) == 0 {
		action.NoOp = true
		action.ReactionContent = ReactionSuccess
		return action
	}
	if blockers := graph.Unsatisfied(issue.Number); len(blockers) > 0 {
		return fail(action, dependencyFailureBody(blockers))
	}

	action.AssignTo = strings.TrimSpace(cfg.AssignTo)
	action.TriggerLabels = missingLabels
	action.ReactionContent = ReactionSuccess
	return action
}

func decideAutonomous(issue Issue, cfg Config, now time.Time, graph *depgraph.DependencyGraph) Action {
	if !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return Action{NoOp: true}
	}
	dispatchLabel, ok := singleDispatchLabelForNamespace(issue.Labels, cfg.Namespace)
	if !ok {
		return Action{NoOp: true}
	}
	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 {
		return Action{NoOp: true}
	}
	if autonomousDispatchHeldForNamespace(issue.Labels, cfg.HoldLabel, cfg.Namespace) || len(missingLabels(issue.Labels, triggerLabels)) == 0 {
		return Action{NoOp: true}
	}
	if issue.TriagedAt.IsZero() || now.UTC().Before(issue.TriagedAt.UTC().Add(cfg.AutonomousDelay)) {
		return Action{NoOp: true}
	}
	if len(graph.Unsatisfied(issue.Number)) > 0 {
		return Action{NoOp: true}
	}
	return Action{AssignTo: strings.TrimSpace(cfg.AssignTo), TriggerLabels: missingLabels(issue.Labels, triggerLabels)}
}

func fail(action Action, body string) Action {
	action.NoOp = true
	action.ReactionContent = ReactionFailure
	action.FailureCommentBody = strings.TrimSpace(body)
	return action
}

func latestCommandAttempt(comments []Comment, slashCommands []string, allowedUsers []string) (Comment, string, bool) {
	for index := len(comments) - 1; index >= 0; index-- {
		comment := comments[index]
		command, ok := ParseSlashCommand(comment.Body, slashCommands)
		if !ok {
			continue
		}
		if !isAllowedUser(comment, allowedUsers) {
			continue
		}
		return comment, command, true
	}
	return Comment{}, "", false
}
func ParseSlashCommand(body string, configured []string) (string, bool) {
	allowed := configuredCommands(configured)
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(trimmed, ">") {
			continue
		}
		switch {
		case allowed["/plan"] && strings.HasPrefix(trimmed, "/plan") && commandBoundary(trimmed, len("/plan")):
			return "/plan", true
		case allowed["/implement"] && strings.HasPrefix(trimmed, "/implement") && commandBoundary(trimmed, len("/implement")):
			return "/implement", true
		}
	}
	return "", false
}

func configuredCommands(configured []string) map[string]bool {
	allowed := map[string]bool{"/plan": false, "/implement": false}
	for _, command := range configured {
		command = strings.TrimSpace(command)
		if _, ok := allowed[command]; ok {
			allowed[command] = true
		}
	}
	return allowed
}

func commandBoundary(value string, index int) bool {
	if len(value) == index {
		return true
	}
	switch value[index] {
	case ' ', '\t', '\r':
		return true
	default:
		return false
	}
}

func isAllowedUser(comment Comment, allowedUsers []string) bool {
	for _, user := range allowedUsers {
		if strings.EqualFold(strings.TrimSpace(user), comment.Author) {
			return true
		}
	}
	return comment.HasWriteAccess
}

func singleDispatchLabelForNamespace(issueLabels []string, namespace labels.Namespace) (string, bool) {
	configured := ""
	legacy := ""
	for _, label := range issueLabels {
		if !namespace.IsDispatch(label) {
			continue
		}
		if namespace.IsConfiguredDispatch(label) {
			if configured != "" {
				return "", false
			}
			configured = label
			continue
		}
		if legacy != "" {
			return "", false
		}
		legacy = label
	}
	if configured != "" {
		return configured, true
	}
	return legacy, legacy != ""
}

func triggerLabelsForDispatch(dispatchLabel string, cfg Config) []string {
	namespace := cfg.Namespace
	if strings.TrimSpace(namespace.Prefix) == "" {
		namespace = labels.DefaultNamespace()
	}
	switch {
	case namespace.IsDispatchPlan(dispatchLabel):
		return compactLabels(cfg.PlannerTriggerLabels)
	case namespace.IsDispatchImplement(dispatchLabel):
		return compactLabels(cfg.WorkerTriggerLabels)
	default:
		return nil
	}
}

func compactLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			out = append(out, label)
		}
	}
	return out
}

func missingLabels(existing []string, want []string) []string {
	missing := make([]string, 0, len(want))
	for _, label := range want {
		if !hasLabel(existing, label) {
			missing = append(missing, label)
		}
	}
	return missing
}

func commandDispatchLabelForNamespace(command string, namespace labels.Namespace) string {
	switch command {
	case "/plan":
		return namespace.DispatchPlan()
	case "/implement":
		return namespace.DispatchImplement()
	default:
		return ""
	}
}

// hasLabel matches a forge label, keeping the empty-want guard that lets an
// unconfigured legacy hold label mean "no legacy hold" rather than "held by a
// blank label". Comparison is normalized: it already trimmed the wanted label,
// and folding case too closes the gap where a hold spelled "Looper:Hold" let
// autonomous dispatch proceed despite a human veto.
func hasLabel(itemLabels []string, want string) bool {
	if strings.TrimSpace(want) == "" {
		return false
	}
	return labels.Has(itemLabels, want)
}

func autonomousDispatchHeldForNamespace(issueLabels []string, legacyHoldLabel string, namespace labels.Namespace) bool {
	// Coordinator autonomous dispatch is only blocked by the official global hold.
	// Lane-specific official holds apply later in each lane's own discovery gate.
	if hasLabel(issueLabels, namespace.HoldGlobal()) {
		return true
	}
	return hasLabel(issueLabels, strings.TrimSpace(legacyHoldLabel))
}

func dependencyFailureBody(blockers []depgraph.Blocker) string {
	lines := []string{"Coordinator can't dispatch because blocked_by is still unsatisfied:"}
	for _, blocker := range blockers {
		lines = append(lines, fmt.Sprintf("- %s (%s)", blockerReference(blocker), blockerStateReason(blocker)))
	}
	return strings.Join(lines, "\n")
}

func blockerReference(blocker depgraph.Blocker) string {
	repo := strings.TrimSpace(blocker.Repo)
	if repo != "" {
		return fmt.Sprintf("%s#%d", repo, blocker.Number)
	}
	return fmt.Sprintf("#%d", blocker.Number)
}

func blockerStateReason(blocker depgraph.Blocker) string {
	if !blocker.Reachable {
		return "unreachable"
	}
	if reason := strings.TrimSpace(blocker.StateReason); reason != "" {
		return reason
	}
	if state := strings.TrimSpace(blocker.State); state != "" {
		return state
	}
	return "unknown"
}
