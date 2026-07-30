package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/daemonbinary"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/triager"
	"github.com/MumuTW/looper/internal/version"
)

type statusResponse struct {
	Service         statusService       `json:"service"`
	Storage         statusStorage       `json:"storage"`
	Scheduler       statusScheduler     `json:"scheduler"`
	Agent           statusAgent         `json:"agent"`
	WorktreeCleanup any                 `json:"worktreeCleanup"`
	Webhook         statusWebhook       `json:"webhook"`
	Loops           statusLoops         `json:"loops"`
	Safety          statusSafety        `json:"safety"`
	Notifications   statusNotifications `json:"notifications"`
	Tools           statusTools         `json:"tools"`
}

type statusService struct {
	Healthy    bool                  `json:"healthy"`
	Version    string                `json:"version"`
	Build      version.BuildMetadata `json:"build"`
	DaemonMode config.DaemonMode     `json:"daemonMode"`
	// AdmissionState is the single live admission Authority (ADR-0015 R1).
	// HTTP mutations and scheduler claims open only when this is "ready".
	AdmissionState string  `json:"admissionState"`
	StartedAt      *string `json:"startedAt,omitempty"`
	// Recovery mixes the one-shot startup snapshot with live outstanding
	// quarantine/orphan debt under recovery.outstanding.
	Recovery any `json:"recovery"`
	// Triage is a read-only projection of the triage report lifecycle. It does
	// not authorize routing and does not add a materialized awaiting state.
	Triage statusTriage `json:"triage"`
	// DegradedReasons lists sticky ops signals (review publish disabled,
	// quarantine orphan debt). Empty when none apply.
	DegradedReasons []string     `json:"degradedReasons,omitempty"`
	Binary          statusBinary `json:"binary"`
}

type statusTriage struct {
	AwaitingConfirmation triager.AwaitingConfirmationSummary `json:"awaitingConfirmation"`
}

type statusBinary struct {
	Name             string   `json:"name"`
	Path             string   `json:"path,omitempty"`
	InstallDir       string   `json:"installDir"`
	CurrentTarget    string   `json:"currentTarget"`
	ArtifactName     *string  `json:"artifactName"`
	SupportedTargets []string `json:"supportedTargets"`
	// Identity answers "is the file at Path still the build I am executing?".
	// A running daemon whose binary was replaced keeps working on the old image
	// and switches builds at the next restart, so the divergence is only
	// visible here (#154).
	Identity daemonbinary.Status `json:"identity"`
}

type versionResponse struct {
	Version string                `json:"version"`
	Build   version.BuildMetadata `json:"build"`
	Binary  versionBinaryResponse `json:"binary"`
}

type versionBinaryResponse struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// daemonBinaryStatus reports the running-image-versus-on-disk comparison. With
// no reporter wired the answer is "unknown", never "unchanged": a status that
// cannot see a swap must not claim there was none.
func (h *Handler) daemonBinaryStatus() daemonbinary.Status {
	if h.context.DaemonBinaryStatus == nil {
		return daemonbinary.Verify(daemonbinary.Identity{})
	}

	return h.context.DaemonBinaryStatus()
}

func daemonExecutablePath() string {
	executablePath, err := os.Executable()
	if err != nil {
		return ""
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return ""
	}

	resolvedPath, err := filepath.EvalSymlinks(executablePath)
	if err != nil {
		return executablePath
	}

	resolvedPath = strings.TrimSpace(resolvedPath)
	if resolvedPath == "" {
		return executablePath
	}

	return resolvedPath
}

type statusStorage struct {
	Mode              string   `json:"mode"`
	DBPath            string   `json:"dbPath"`
	SchemaVersion     string   `json:"schemaVersion,omitempty"`
	PendingMigrations []string `json:"pendingMigrations"`
	Healthy           bool     `json:"healthy"`
}

type statusScheduler struct {
	Healthy        bool `json:"healthy"`
	QueuedItems    int  `json:"queuedItems"`
	RunningItems   int  `json:"runningItems"`
	CompletedItems int  `json:"completedItems"`
	FailedItems    int  `json:"failedItems"`
	TotalRuns      int  `json:"totalRuns"`
	ActiveRuns     int  `json:"activeRuns"`
}

type statusAgent struct {
	Vendor              *config.AgentVendor `json:"vendor,omitempty"`
	Model               *string             `json:"model,omitempty"`
	NativeResumeEnabled bool                `json:"nativeResumeEnabled"`
	Timeouts            statusAgentTimeouts `json:"timeouts"`
}

type statusAgentTimeouts struct {
	Planner  statusAgentRoleTimeouts `json:"planner"`
	Worker   statusAgentRoleTimeouts `json:"worker"`
	Reviewer statusAgentRoleTimeouts `json:"reviewer"`
	Fixer    statusAgentRoleTimeouts `json:"fixer"`
}

type statusAgentRoleTimeouts struct {
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds"`
	MaxRuntimeSeconds  int `json:"maxRuntimeSeconds"`
}

type statusWebhook struct {
	Enabled                     bool     `json:"enabled"`
	EndpointURL                 string   `json:"endpointUrl"`
	FallbackPollIntervalSeconds int      `json:"fallbackPollIntervalSeconds"`
	Degraded                    bool     `json:"degraded"`
	DegradedReasons             []string `json:"degradedReasons"`
	ConfiguredForwarders        int      `json:"configuredForwarders"`
	RunningForwarders           int      `json:"runningForwarders"`
}

type statusLoopType struct {
	Queued     int `json:"queued"`
	Running    int `json:"running"`
	Waiting    int `json:"waiting"`
	Paused     int `json:"paused"`
	Failed     int `json:"failed"`
	Terminated int `json:"terminated"`
	Stopped    int `json:"stopped"`
}

type statusLoops struct {
	Planner  statusLoopType `json:"planner"`
	Reviewer statusLoopType `json:"reviewer"`
	Worker   statusLoopType `json:"worker"`
	Fixer    statusLoopType `json:"fixer"`
}

type statusSafety struct {
	AllowAutoCommit    bool                  `json:"allowAutoCommit"`
	AllowAutoPush      bool                  `json:"allowAutoPush"`
	AllowAutoApprove   bool                  `json:"allowAutoApprove"`
	AllowRiskyFixes    bool                  `json:"allowRiskyFixes"`
	FixAllPullRequests bool                  `json:"fixAllPullRequests"`
	OpenPRStrategy     config.OpenPRStrategy `json:"openPrStrategy"`
}

type statusNotifications struct {
	InAppEnabled     bool `json:"inAppEnabled"`
	OsascriptEnabled bool `json:"osascriptEnabled"`
}

type statusTools struct {
	Git       bool `json:"git"`
	GH        bool `json:"gh"`
	Osascript bool `json:"osascript"`
	// LooperPath is the configured tools.looperPath used for review publish.
	LooperPath string `json:"looperPath,omitempty"`
	// ReviewPublish surfaces capability-probe readiness for reviewer publishing.
	ReviewPublish looperdruntime.ReviewPublishReadiness `json:"reviewPublish"`
}

func (h *Handler) buildVersionResponse() versionResponse {
	return versionResponse{
		Version: version.Current().Version,
		Build:   version.Current().Metadata,
		Binary: versionBinaryResponse{
			Name: "looperd",
			Path: daemonExecutablePath(),
		},
	}
}

func (h *Handler) buildStatusResponse(ctx context.Context) (statusResponse, error) {
	storageState, err := h.loadStorageState(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	services := h.context.Runtime.Services()
	loopCountsByType, err := services.Repositories.Loops.CountByTypeAndStatus(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	runCounts, err := services.Repositories.Runs.CountByStatus(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	queueCounts, err := services.Repositories.Queue.CountByAllStatuses(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	loopCounts := countLoops(loopCountsByType)

	currentTarget := currentLooperdTarget()
	installDir := filepath.Join(homeDirOrEmpty(), ".looper", "bin")
	artifactName := looperdArtifactName(currentTarget)

	reviewPublish := looperdruntime.ReviewPublishReadinessFor(h.effectiveConfig())
	forgeCredential := looperdruntime.ForgeCredentialReadinessFor(h.effectiveConfig())
	outstanding, debtErr := looperdruntime.CountOutstandingQuarantineDebt(ctx, services.Repositories)
	awaitingConfirmation, err := triager.AwaitingConfirmationStatus(ctx, services.Repositories, h.now())
	if err != nil {
		return statusResponse{}, err
	}
	recovery := h.recoveryWithOutstanding(outstanding)
	binaryIdentity := h.daemonBinaryStatus()
	degradedReasons := statusDegradedReasons(reviewPublish, forgeCredential, outstanding, debtErr, binaryIdentity)
	// Snapshot admission once so service and scheduler cannot disagree if the
	// runtime transitions while this response is being assembled.
	admissionState := h.admissionStateString()

	return statusResponse{
		Service: statusService{
			Healthy:         storageState.OK,
			Version:         version.Current().Version,
			Build:           version.Current().Metadata,
			DaemonMode:      h.context.Config.Daemon.Mode,
			AdmissionState:  admissionState,
			StartedAt:       h.startedAtISO(),
			Recovery:        recovery,
			Triage:          statusTriage{AwaitingConfirmation: awaitingConfirmation},
			DegradedReasons: degradedReasons,
			Binary: statusBinary{
				Name:             "looperd",
				Path:             daemonExecutablePath(),
				InstallDir:       installDir,
				CurrentTarget:    currentTarget,
				ArtifactName:     artifactName,
				SupportedTargets: []string{"darwin-arm64", "linux-amd64"},
				Identity:         binaryIdentity,
			},
		},
		Storage: statusStorage{
			Mode:              h.context.Config.Storage.Mode,
			DBPath:            h.context.Config.Storage.DBPath,
			SchemaVersion:     storageState.schemaVersion(),
			PendingMigrations: append([]string{}, storageState.PendingMigrationIDs...),
			Healthy:           storageState.OK,
		},
		Scheduler: statusScheduler{
			Healthy:        admissionState == string(looperdruntime.AdmissionReady),
			QueuedItems:    int(queueCounts["queued"]),
			RunningItems:   int(queueCounts["running"]),
			CompletedItems: int(queueCounts["completed"]),
			FailedItems:    int(queueCounts["failed"] + queueCounts["manual_intervention"]),
			TotalRuns:      sumStatusCounts(runCounts),
			ActiveRuns:     int(runCounts["running"]),
		},
		Agent: statusAgent{
			Vendor:              h.context.Config.Agent.Vendor,
			Model:               h.context.Config.Agent.Model,
			NativeResumeEnabled: h.context.Config.Agent.NativeResume.Enabled,
			Timeouts: statusAgentTimeouts{
				Planner:  statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.PlannerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.PlannerMaxRuntimeSeconds},
				Worker:   statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.WorkerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.WorkerMaxRuntimeSeconds},
				Reviewer: statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.ReviewerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.ReviewerMaxRuntimeSeconds},
				Fixer:    statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.FixerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.FixerMaxRuntimeSeconds},
			},
		},
		WorktreeCleanup: h.buildWorktreeCleanupStatusResponse(),
		Webhook:         summarizeWebhookStatus(h.buildWebhookStatusResponse()),
		Loops:           loopCounts,
		Safety: statusSafety{
			AllowAutoCommit:    h.context.Config.Defaults.AllowAutoCommit,
			AllowAutoPush:      h.context.Config.Defaults.AllowAutoPush,
			AllowAutoApprove:   h.context.Config.Defaults.AllowAutoApprove,
			AllowRiskyFixes:    h.context.Config.Defaults.AllowRiskyFixes,
			FixAllPullRequests: h.context.Config.Defaults.FixAllPullRequests,
			OpenPRStrategy:     h.context.Config.Defaults.OpenPRStrategy,
		},
		Notifications: statusNotifications{
			InAppEnabled:     h.context.Config.Notifications.InApp,
			OsascriptEnabled: h.context.Config.Notifications.Osascript.Enabled,
		},
		Tools: statusTools{
			Git:           hasValue(h.context.Config.Tools.GitPath),
			GH:            hasValue(h.context.Config.Tools.GHPath),
			Osascript:     hasValue(h.context.Config.Tools.OsascriptPath),
			LooperPath:    reviewPublish.LooperPath,
			ReviewPublish: reviewPublish,
		},
	}, nil
}

func (h *Handler) buildWorktreeCleanupStatusResponse() any {
	if runtimeWithCleanup, ok := any(h.context.Runtime).(interface {
		WorktreeCleanupStatus() looperdruntime.WorktreeCleanupStatus
	}); ok {
		return runtimeWithCleanup.WorktreeCleanupStatus()
	}
	return looperdruntime.WorktreeCleanupStatus{
		Enabled:    h.context.Config.Daemon.WorktreeCleanup.Enabled,
		DryRun:     h.context.Config.Daemon.WorktreeCleanup.DryRun,
		LastStatus: "idle",
	}
}

type storageState struct {
	OK                  bool
	LatestAvailableID   string
	LatestAppliedID     string
	PendingMigrationIDs []string
	Details             *string
}

func (h *Handler) loadStorageState(ctx context.Context) (storageState, error) {
	services := h.context.Runtime.Services()
	status, err := services.Coordinator.MigrationRunner().Status(ctx)
	if err != nil {
		return storageState{}, err
	}

	state := storageState{OK: true}
	if len(status.Available) > 0 {
		state.LatestAvailableID = status.Available[len(status.Available)-1].ID
	}
	if len(status.Applied) > 0 {
		state.LatestAppliedID = status.Applied[len(status.Applied)-1].ID
	}
	state.PendingMigrationIDs = make([]string, 0, len(status.Pending))
	for _, migration := range status.Pending {
		state.PendingMigrationIDs = append(state.PendingMigrationIDs, migration.ID)
	}

	return state, nil
}

func (h *Handler) startedAtISO() *string {
	startedAt, ok := h.context.Runtime.StartedAt()
	if !ok {
		return nil
	}

	value := startedAt.UTC().Format(javaScriptISOString)
	return &value
}

func (s storageState) schemaVersion() string {
	if s.LatestAppliedID == "" {
		return "uninitialized"
	}

	return s.LatestAppliedID
}

func countLoops(countsByType map[string]map[string]int64) statusLoops {
	counts := statusLoops{}
	for loopType, statuses := range countsByType {
		var target *statusLoopType
		switch loopType {
		case "planner":
			target = &counts.Planner
		case "reviewer":
			target = &counts.Reviewer
		case "worker":
			target = &counts.Worker
		case "fixer":
			target = &counts.Fixer
		default:
			continue
		}

		for status, count := range statuses {
			switch status {
			case "queued":
				target.Queued = int(count)
			case "running":
				target.Running = int(count)
			case "waiting":
				target.Waiting = int(count)
			case "paused":
				target.Paused = int(count)
			case "failed":
				target.Failed = int(count)
			case "terminated":
				target.Terminated = int(count)
			case "stopped":
				target.Stopped = int(count)
			}
		}
	}

	return counts
}

func sumStatusCounts(counts map[string]int64) int {
	total := 0
	for _, count := range counts {
		total += int(count)
	}
	return total
}

func generateRequestID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}

	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80

	hexValue := hex.EncodeToString(buffer)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[0:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:32])
}

func hasValue(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != ""
}

func homeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return home
}

func currentLooperdTarget() string {
	return fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
}

func normalizeRecoverySummary(summary looperdruntime.RecoverySummary) map[string]any {
	normalized := map[string]any{}
	if summary.StartedAt != "" {
		normalized["startedAt"] = summary.StartedAt
	}
	if summary.CompletedAt != "" {
		normalized["completedAt"] = summary.CompletedAt
	}
	if summary.OrphanAgentCleanup.Attempted || summary.OrphanAgentCleanup.CleanedCount != 0 || summary.OrphanAgentCleanup.QuarantinedCount != 0 || summary.OrphanAgentCleanup.Warning != "" {
		orphan := map[string]any{
			"attempted":        summary.OrphanAgentCleanup.Attempted,
			"cleanedCount":     summary.OrphanAgentCleanup.CleanedCount,
			"quarantinedCount": summary.OrphanAgentCleanup.QuarantinedCount,
		}
		if summary.OrphanAgentCleanup.Warning != "" {
			orphan["warning"] = summary.OrphanAgentCleanup.Warning
		}
		normalized["orphanAgentCleanup"] = orphan
	}
	if summary.ExpiredLocksReleased != 0 {
		normalized["expiredLocksReleased"] = summary.ExpiredLocksReleased
	}
	if summary.InterruptedRunsMarked != 0 {
		normalized["interruptedRunsMarked"] = summary.InterruptedRunsMarked
	}
	if summary.LoopsRequeued != 0 {
		normalized["loopsRequeued"] = summary.LoopsRequeued
	}
	if summary.EventsWritten != 0 {
		normalized["eventsWritten"] = summary.EventsWritten
	}

	return normalized
}

func (h *Handler) recoveryWithOutstanding(outstanding looperdruntime.OutstandingQuarantineDebt) any {
	recovery := h.recoverySummary()
	normalized, ok := recovery.(map[string]any)
	if !ok {
		if recovery == nil {
			normalized = map[string]any{}
		} else {
			// Preserve non-map recovery payloads from test doubles.
			return map[string]any{
				"snapshot":    recovery,
				"outstanding": outstanding,
			}
		}
	} else {
		// Copy so we do not mutate a shared recovery map.
		copied := make(map[string]any, len(normalized)+1)
		for key, value := range normalized {
			copied[key] = value
		}
		normalized = copied
	}
	normalized["outstanding"] = outstanding
	return normalized
}

func statusDegradedReasons(reviewPublish looperdruntime.ReviewPublishReadiness, forgeCredential looperdruntime.ForgeCredentialReadiness, outstanding looperdruntime.OutstandingQuarantineDebt, debtErr error, binaryIdentity daemonbinary.Status) []string {
	var reasons []string
	if binaryIdentity.Swapped {
		reasons = append(reasons, daemonbinary.SwappedDegradedReason)
	}
	if reviewPublish.Known && reviewPublish.PublishingDisabled {
		reasons = append(reasons, "review_publish_disabled")
	}
	if forgeCredential.Degraded() {
		reasons = append(reasons, looperdruntime.ForgeCredentialDegradedReason)
	}
	if outstanding.QuarantinedActiveExecutions > 0 || outstanding.QuarantinedRunningRuns > 0 {
		reasons = append(reasons, "quarantine_orphan_debt")
	}
	if debtErr != nil {
		reasons = append(reasons, "quarantine_debt_unavailable")
	}
	return reasons
}
