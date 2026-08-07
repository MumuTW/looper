package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/forge"
	"github.com/MumuTW/looper/internal/lifecycle"
	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/processcontainment"
	"github.com/MumuTW/looper/internal/processidentity"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/validationcmd"
	"github.com/mattn/go-sqlite3"
)

// ErrExecutionPersistence is returned when a hard agent_executions observation
// write fails (initial ownership, mid-life heartbeat/output, or terminal).
// Soft/transient failures (cancel, conflict after terminal won, one busy retry)
// do not produce this error alone.
var ErrExecutionPersistence = errors.New("agent execution persistence failed")

const (
	defaultMaxOutputBytes    = 256 * 1024
	maxPersistedLogReadBytes = 16 * 1024 * 1024
	completionMarkerEnv      = "LOOPER_COMPLETION_MARKER"
)

var unsafeAgentEnvKeys = []string{
	"OLDPWD",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_CONFIG",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY",
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_GRAFT_FILE",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_REPLACE_REF_BASE",
	"GIT_PREFIX",
	"GIT_SHALLOW_FILE",
}

var inheritedAgentEnvKeys = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"TMPDIR",
	"TMP",
	"TEMP",
	"LANG",
	"TERM",
	"COLORTERM",
	"NO_COLOR",
	"FORCE_COLOR",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
	"SSH_AUTH_SOCK",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"NODE_EXTRA_CA_CERTS",
	"GIT_SSL_CAINFO",
	"CODEX_HOME",
	"CLAUDE_CONFIG_DIR",
	"OPENCODE_CONFIG_DIR",
	// Config path selector for agent-invoked looper commands that load via
	// LOOPER_CONFIG when --config is not passed.
	"LOOPER_CONFIG",
	// Capability socket for daemon-side review submit (not a secret).
	forge.TrustedReviewSockEnv,
}

type ExecutorConfig struct {
	Vendor              config.AgentVendor
	Model               *string
	ReasoningEffort     *config.ReasoningEffort
	Params              map[string]any
	Env                 map[string]string
	NativeResumeEnabled bool
	// LiveToolEvents runs codex with `--json` and parses its JSONL event stream so
	// the live status card can show a structured tool-call feed ("✅ <cmd>"). Only
	// affects the codex vendor; the result + session are read from the JSONL. Off
	// by default (env-gated) so it can't disturb the text-mode path until proven.
	LiveToolEvents bool
}

type ExecutorOptions struct {
	Config ExecutorConfig
	Repos  *storage.Repositories
	LogDir string
	Now    func() time.Time
	// ParamsOwnerVendor marks Config.Params as global agent.params owned by that
	// vendor (typically agent.vendor). effectiveConfig always filters command/args
	// via ParamsForRoleVendor against the effective identity (role or sticky
	// snapshot): matching owner keeps wrappers, diverged or nil owner strips
	// vendor-owned command/args so orphan global wrappers cannot ride a role-only
	// vendor. Tests that intentionally pre-bind params.command should set the
	// owner to Config.Vendor so the same-vendor path preserves them.
	ParamsOwnerVendor *config.AgentVendor
	// Owner, when set, admits every agent spawn under the Execution Supervisor
	// before cmd.Start and binds the process containment handle before Start
	// returns (ADR-0015 / #576). Daemon producers must wire Owner; unit tests
	// may leave it nil and still get containment without Supervisor lease.
	Owner SpawnOwner
	// OnHardPersistFailure is invoked on the first hard agent_executions write
	// failure for an execution path (heartbeat/output or terminal storage
	// broken). Daemon wiring must map this to sticky admission degraded
	// (ADR-0015 R5 / #578). Soft/transient errors and pure cancel do not call it.
	OnHardPersistFailure func(error)
	// OnProgress, when set, is called (throttled) while an agent run streams
	// output, so a transport can surface live progress. Vendor-agnostic: it works
	// off the subprocess's stdout tail, whatever agent (codex/opencode/claude) runs.
	OnProgress func(context.Context, ProgressUpdate)
	// OnOutcome, when set, is called once per execution that reached a terminal
	// state the agent itself produced. It is the daemon's provider-agnostic view
	// of "did the agent work", and exists so a health gate can be built without
	// any component having to recognize a specific provider's rate-limit or
	// outage message. Executions looper killed are not reported: shutting an
	// agent down is looper's decision, not evidence about the provider.
	OnOutcome func(Outcome)
}

// Outcome is one terminal agent execution, reduced to whether it worked.
type Outcome struct {
	ProjectID   string
	LoopID      string
	RunID       string
	ExecutionID string
	// Vendor is the effective provider identity for this execution. Runtime
	// health is partitioned by this value so one provider outage does not pause
	// independent providers.
	Vendor string
	// BrownoutProbe is true only when the spawn was admitted during half-open.
	BrownoutProbe bool
	// BrownoutProbeGeneration identifies the half-open round for this outcome.
	BrownoutProbeGeneration uint64
	// BrownoutStickySnapshot is true when this outcome belongs to a retry using
	// a persisted vendor snapshot rather than the current live role config.
	BrownoutStickySnapshot bool
	// Status is the executor's terminal status ("completed", "failed",
	// "timeout").
	Status string
	// Succeeded is true only for a clean completion. A timeout counts as a
	// failure here: an agent that hangs and one that is refused both mean work
	// is not getting done, and both are worth backing off from.
	Succeeded bool
	// StartedAt is when this execution was admitted. A health gate needs it to
	// tell a probe it admitted from a long-running execution that predates it.
	StartedAt time.Time
}

// declaresRetryableBlock reports whether a parsed completion marker says the
// agent was blocked by something a retry might clear. The role runners turn
// that into a failed, replayable run, so health accounting must agree: a
// provider that answers "blocked: retryable_transient, rate limited" would
// otherwise be recorded as a success on every attempt while the runner keeps
// retrying — diluting the very ratio meant to notice it.
//
// manual_intervention is deliberately excluded. That is looper or the repo
// needing a human, not the provider failing, and backing off from the provider
// would not help.
func declaresRetryableBlock(completionPayload string) bool {
	payload := strings.TrimSpace(completionPayload)
	if payload == "" {
		return false
	}
	var parsed struct {
		Outcome     string `json:"outcome"`
		FailureKind string `json:"failure_kind"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(parsed.Outcome), "blocked") {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(parsed.FailureKind), "manual_intervention")
}

func validRawJSONObject(stdout string) bool {
	message := strings.TrimSpace(FinalMessage(stdout))
	if message == "" {
		return false
	}
	var object map[string]any
	return json.Unmarshal([]byte(message), &object) == nil && object != nil
}

// validRawJSONObjectEnvelope matches callers whose parser extracts the first
// JSON object from otherwise harmless surrounding prose (for example reviewer
// thread reconciliation). The caller still owns semantic schema validation;
// this only keeps health accounting aligned with its accepted transport shape.
func validRawJSONObjectEnvelope(stdout string) bool {
	message := strings.TrimSpace(FinalMessage(stdout))
	start := strings.Index(message, "{")
	end := strings.LastIndex(message, "}")
	if start < 0 || end < start {
		return false
	}
	var object map[string]any
	return json.Unmarshal([]byte(message[start:end+1]), &object) == nil && object != nil
}

// validMarkerOutcome rejects a marker that advertises an outcome outside the
// shared completion contract. Generic workers still use the summary-only
// marker, so an absent outcome remains valid; once an agent supplies the key,
// only completed or blocked is recognized. This keeps health accounting aligned
// with fixerRepairTaskOutcome instead of treating an unknown role result as a
// provider success.
func validMarkerOutcome(payload string) bool {
	var raw map[string]json.RawMessage
	if strings.TrimSpace(payload) == "" || json.Unmarshal([]byte(payload), &raw) != nil {
		return false
	}
	encoded, ok := raw["outcome"]
	if !ok {
		return true
	}
	var outcome string
	if json.Unmarshal(encoded, &outcome) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "completed", "blocked":
		return true
	default:
		return false
	}
}

// validFixerMarkerOutcome is the stricter marker contract used by the fixer.
// Unlike generic coding roles, the fixer runner requires an explicit outcome
// before it may advance to validation or publish; a summary-only marker is not
// evidence that repair completed.
func validFixerMarkerOutcome(payload string) bool {
	var raw map[string]json.RawMessage
	if strings.TrimSpace(payload) == "" || json.Unmarshal([]byte(payload), &raw) != nil {
		return false
	}
	encoded, ok := raw["outcome"]
	if !ok {
		return false
	}
	var outcome string
	if json.Unmarshal(encoded, &outcome) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "completed", "blocked":
		return true
	default:
		return false
	}
}

// validReviewerMarkerOutcome aligns health accounting with the reviewer
// runner's reviewCompletionOutcome contract. A reviewer may finish cleanly or
// publish actionable feedback as blocking, non_blocking, or legacy actionable;
// an absent outcome preserves the summary-only marker used by clean no-op runs.
func validReviewerMarkerOutcome(payload string) bool {
	var raw map[string]json.RawMessage
	if strings.TrimSpace(payload) == "" || json.Unmarshal([]byte(payload), &raw) != nil {
		return false
	}
	encoded, ok := raw["outcome"]
	if !ok {
		return true
	}
	var outcome string
	if json.Unmarshal(encoded, &outcome) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "clean", "non_blocking", "blocking", "actionable":
		return true
	default:
		return false
	}
}

// ProgressUpdate is a throttled snapshot of a running agent's activity: the last
// few lines it has emitted, plus how long it has been running.
type ProgressUpdate struct {
	LoopID         string
	RunID          string
	ExecutionID    string
	TailLines      []string
	ElapsedSeconds int64
}

type RunInput struct {
	ExecutionID        string
	ProjectID          string
	LoopID             string
	RunID              string
	Prompt             string
	NativeResumePrompt string
	WorkingDirectory   string
	Timeout            time.Duration
	HeartbeatTimeout   time.Duration
	GracefulShutdown   time.Duration
	MaxOutputBytes     int
	// TimeoutObservationBudget bounds the daemon-owned observation callback
	// that runs after a timeout process group is contained. A zero value keeps
	// the conservative default; callers that need a larger Git observation
	// window must set it explicitly rather than relying on a hidden constant.
	TimeoutObservationBudget time.Duration
	Metadata                 map[string]any
	IdempotencyKey           string
	Env                      map[string]string
	// OnBeforeTimeout runs synchronously after an idle or max-runtime timeout
	// terminates this execution's process group and that group is confirmed dead,
	// and before terminal timeout persistence. It is for daemon-owned
	// durability observations, never for business policy or process control.
	// Its error is reported in Result but cannot prevent termination.
	OnBeforeTimeout func(context.Context, TimeoutObservation) error
	// RestrictToolNetwork forces supported coding agents to keep their tool
	// subprocesses inside the writable worktree with network access disabled.
	// The agent process itself may still reach its model provider; the daemon
	// remains the only authority that fetches or publishes repository state.
	RestrictToolNetwork bool
	// Assessment uses the daemon-owned, read-only Codex tool profile that is
	// available before a human authorizes mutation. It is intentionally not a
	// resumable or configurable variant of normal Planner execution.
	Assessment      bool
	NativeSessionID string
	// UseSnapshot, when true with a non-empty SnapshotVendor, overrides the
	// executor's configured vendor/model/reasoning effort for this start only (spawn, native
	// resume vendor checks, and persisted execution vendor). Env and
	// NativeResumeEnabled still come from the executor config. Identity-bearing
	// params are filtered against the frozen vendor: wrappers are kept when the
	// snapshot matches the params owner (or pre-bound base vendor), and model
	// flags in args are stripped so SnapshotModel wins.
	UseSnapshot    bool
	SnapshotVendor string
	// SnapshotModel is used only when UseSnapshot is true. nil means no model
	// flag; a non-nil value (including empty) sets the model override.
	SnapshotModel *string
	// SnapshotReasoningEffort is used only when UseSnapshot is true. nil means
	// no reasoning-effort override.
	SnapshotReasoningEffort *config.ReasoningEffort
	// CompletionContract selects the output contract that determines health
	// success. Coding agents use the marker contract; reviewer runs use the
	// reviewer outcome marker; coordinator and triager classifiers use raw JSON;
	// file-backed callers provide a semantic validator.
	CompletionContract CompletionContract
	// CompletionValidator is the daemon-owned caller validator for contracts
	// whose authority is not the executor's generic syntax check (for example a
	// planner assessment file or a coordinator decision schema). For stdout
	// contracts it receives FinalMessage(stdout); file-backed callers may ignore
	// the argument. It is evaluated only at terminal outcome reporting.
	CompletionValidator func(string) bool
	// CompletionOutcomeValidator is a caller-owned durable publication check.
	// Reviewer runs use it only when the process status or stdout marker is
	// incomplete; a verified remote review marker can therefore count as a
	// successful provider outcome even when the agent exited non-zero.
	CompletionOutcomeValidator func() bool
	// BrownoutProbe is populated by the common spawn admission lease.
	BrownoutProbe bool
	// BrownoutProbeGeneration is copied from the common spawn admission lease.
	BrownoutProbeGeneration uint64
}

type CompletionContract string

const (
	CompletionContractMarker              CompletionContract = "marker"
	CompletionContractRawJSON             CompletionContract = "raw_json"
	CompletionContractRawJSONEnvelope     CompletionContract = "raw_json_envelope"
	CompletionContractReviewerMarker      CompletionContract = "reviewer_marker"
	CompletionContractReviewerPublication CompletionContract = "reviewer_publication"
	CompletionContractWorkerHITL          CompletionContract = "worker_hitl"
	CompletionContractFixerMarker         CompletionContract = "fixer_marker"
	// CompletionContractPlannerMarker matches Planner's optional completion
	// marker. Planner can advance from a successful agent result without a
	// marker, while a present marker is still validated by the Planner caller.
	CompletionContractPlannerMarker CompletionContract = "planner_marker"
	CompletionContractFile          CompletionContract = "file"
)

// TimeoutObservation identifies the timeout that is about to terminate an
// agent process. LastProgressAt is the executor's own observed activity time.
type TimeoutObservation struct {
	TimeoutType    string
	LastProgressAt string
}

type Result struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	CompletionSignal             string
	CompletionPayload            string
	Artifacts                    []string
	ChangedFiles                 []string
	Commits                      []string
	Lifecycle                    *lifecycle.State
	HeartbeatCount               int64
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
	PreTimeoutError              string
	NativeResumeMode             string
	NativeResumeStatus           string
	PID                          int
}

type completionParse struct {
	ParseStatus       string
	CompletionSignal  string
	CompletionPayload string
	Summary           string
	Artifacts         []string
	ChangedFiles      []string
	Commits           []string
	Lifecycle         *lifecycle.State
}

type Execution interface {
	Wait(context.Context) (Result, error)
	Kill(string) error
}

type ConfiguredExecutor struct {
	config               ExecutorConfig
	paramsOwner          *config.AgentVendor
	repos                *storage.Repositories
	logDir               string
	now                  func() time.Time
	owner                SpawnOwner
	onHardPersistFailure func(error)
	onProgress           func(context.Context, ProgressUpdate)
	onOutcome            func(Outcome)
}

func New(options ExecutorOptions) *ConfiguredExecutor {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &ConfiguredExecutor{
		config:               options.Config,
		paramsOwner:          options.ParamsOwnerVendor,
		repos:                options.Repos,
		logDir:               options.LogDir,
		now:                  now,
		owner:                options.Owner,
		onHardPersistFailure: options.OnHardPersistFailure,
		onProgress:           options.OnProgress,
		onOutcome:            options.OnOutcome,
	}
}

// liveProgressInterval throttles OnProgress so a chatty agent doesn't hammer the
// transport: at most one update per window while output streams.
const liveProgressInterval = 5 * time.Second

// liveProgressTailLines is how many recent output lines a progress update carries.
const liveProgressTailLines = 5

type nativeResumeInfo struct {
	Enabled           bool
	SessionID         string
	Mode              string
	Status            string
	SourceExecutionID string
}

// effectiveConfig returns the executor config for this start, applying run
// snapshot identity overrides when UseSnapshot is set.
func (e *ConfiguredExecutor) effectiveConfig(input RunInput) ExecutorConfig {
	cfg := e.config

	if input.UseSnapshot {
		if vendor := strings.TrimSpace(input.SnapshotVendor); vendor != "" {
			cfg.Vendor = config.AgentVendor(vendor)
			cfg.Model = input.SnapshotModel
			cfg.ReasoningEffort = input.SnapshotReasoningEffort
		}
	}

	// Always filter global agent.params against the effective identity (role
	// vendor or sticky snapshot). Construction must not pre-strip against the
	// live role alone: a failed Codex run that still owns params.command would
	// otherwise lose its wrapper after the role is hot-switched and sticky
	// retry restores the snapshot vendor. Nil paramsOwner (agent.vendor unset)
	// still runs ParamsForRoleVendor so orphan command/args cannot launch the
	// wrong binary for a role-only vendor.
	cfg.Params = ParamsForRoleVendor(e.config.Params, e.paramsOwner, cfg.Vendor, cfg.Model)
	return cfg
}

// ParamsForRoleVendor returns executor params for an effective coding-role vendor.
// Global agent.params are owned by agent.vendor. When the effective vendor
// (resolved role or sticky snapshot) differs from that owner — or the owner is
// unset while a role still resolves — command and args are dropped so
// vendor-specific wrappers/flags cannot launch the wrong binary or inject
// foreign CLI shape. Same-vendor identity keeps command and args; model flags
// in args are stripped whenever roleModel is non-nil so roles.*.agent.model /
// profile / global agent.model can win via prependModelFlag, and so an
// explicit empty model binding (suppress → vendor default) does not leave
// params --model/-m in place. When roleModel is nil (unset), params.args
// --model/-m are preserved so existing params-only model configs do not
// silently fall back to vendor defaults.
func ParamsForRoleVendor(params map[string]any, globalVendor *config.AgentVendor, roleVendor config.AgentVendor, roleModel *string) map[string]any {
	if params == nil {
		return nil
	}
	if globalVendor != nil && *globalVendor == roleVendor {
		// Clone so role resolution cannot mutate the shared global params map.
		// Strip model flags when a resolved model binding is present — including
		// non-nil empty (explicit suppress to vendor default).
		if roleModel != nil {
			return cloneParamsForSnapshot(params, false)
		}
		return maps.Clone(params)
	}
	return cloneParamsForSnapshot(params, true)
}

// cloneParamsForSnapshot copies params and strips identity-bearing overrides.
// When stripVendorOwned is true (diverged vendor), params.command and
// params.args are removed entirely — global args are vendor-shaped and must
// not follow a different binary. When false (same vendor), only model flags
// in args are removed so SnapshotModel / role model can win.
func cloneParamsForSnapshot(params map[string]any, stripVendorOwned bool) map[string]any {
	if params == nil {
		return nil
	}
	out := maps.Clone(params)
	if stripVendorOwned {
		delete(out, "command")
		delete(out, "args")
		return out
	}
	if args, ok := out["args"]; ok {
		out["args"] = stripModelFlagsFromArgs(args)
	}
	return out
}

// stripModelFlagsFromArgs removes -m / --model and --model=* style flags (and
// their values when separate) from params args. Supports []string and []any.
func stripModelFlagsFromArgs(args any) any {
	switch typed := args.(type) {
	case []string:
		return stripModelFlags(typed)
	case []any:
		asStrings := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				// Preserve non-string entries by converting via stringArgs path later;
				// only strip when the whole list is string-compatible.
				return args
			}
			asStrings = append(asStrings, text)
		}
		stripped := stripModelFlags(asStrings)
		out := make([]any, len(stripped))
		for i, s := range stripped {
			out[i] = s
		}
		return out
	default:
		return args
	}
}

func stripModelFlags(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	skipNext := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "-m" || arg == "--model" {
			if i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m=") {
			continue
		}
		// Attached short form: -mMODEL (not -m=MODEL, already handled).
		if strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "-m=") && arg != "-m" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func (e *ConfiguredExecutor) resolveNativeResume(ctx context.Context, input RunInput) (nativeResumeInfo, error) {
	// Assessments are a fresh, read-only pre-authorization operation. They must
	// not inherit repository execution state, even when a prior loop execution
	// has a resumable native session.
	if input.Assessment {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "disabled"}, nil
	}
	cfg := e.effectiveConfig(input)
	if !cfg.NativeResumeEnabled {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "disabled"}, nil
	}
	if sessionID := strings.TrimSpace(input.NativeSessionID); sessionID != "" {
		if nativeResumeSupported(cfg.Vendor) {
			return nativeResumeInfo{Enabled: true, SessionID: sessionID, Mode: "native_resume", Status: "started"}, nil
		}
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unsupported"}, nil
	}
	if e.repos == nil || e.repos.AgentExecutions == nil || strings.TrimSpace(input.LoopID) == "" {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unavailable"}, nil
	}
	latest, err := e.repos.AgentExecutions.GetLatestByLoopID(ctx, input.LoopID)
	if err != nil {
		return nativeResumeInfo{}, fmt.Errorf("load latest agent execution for native resume: %w", err)
	}
	if latest == nil || latest.NativeSessionID == nil || strings.TrimSpace(*latest.NativeSessionID) == "" {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unavailable"}, nil
	}
	if latest.Vendor != string(cfg.Vendor) || !nativeResumeSupported(cfg.Vendor) || !isRecoverableNativeResumeSource(latest.Status, latest.NativeResumeStatus) {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "unavailable"}, nil
	}
	return nativeResumeInfo{Enabled: true, SessionID: strings.TrimSpace(*latest.NativeSessionID), Mode: "native_resume", Status: "started", SourceExecutionID: latest.ID}, nil
}

func (e *ConfiguredExecutor) markNativeResumeFailed(ctx context.Context, executionID string, message string) error {
	if executionID == "" || e.repos == nil || e.repos.AgentExecutions == nil {
		return nil
	}
	record, err := e.repos.AgentExecutions.GetByID(ctx, executionID)
	if err != nil || record == nil {
		return err
	}
	nowISO := eventlog.FormatJavaScriptISOString(e.now().UTC())
	record.NativeResumeStatus = stringPtr("failed")
	record.NativeResumeError = stringPtr(message)
	record.UpdatedAt = nowISO
	return e.repos.AgentExecutions.Upsert(ctx, *record)
}

func nativeResumeSupported(vendor config.AgentVendor) bool {
	adapter, ok := runtimeAdapterFor(vendor)
	return ok && adapter.contract.Supports(CapabilityHeadlessResume) && adapter.resolveNativeResumeArgs != nil
}

func isRecoverableNativeResumeSource(status string, resumeStatus *string) bool {
	if resumeStatus == nil || *resumeStatus != "pending" {
		return false
	}
	switch status {
	case "running", "cancelling", "killed", "timeout", "failed", "completed":
		return true
	default:
		return false
	}
}

func (e *ConfiguredExecutor) Start(ctx context.Context, input RunInput) (Execution, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, fmt.Errorf("agent prompt is required")
	}
	if strings.TrimSpace(input.WorkingDirectory) == "" {
		return nil, fmt.Errorf("working directory is required")
	}

	executionID := input.ExecutionID
	if executionID == "" {
		executionID = eventlog.NewEventID("agentexec")
	}
	cfg := e.effectiveConfig(input)
	if input.Assessment {
		if err := validateAssessmentExecution(cfg, input); err != nil {
			return nil, err
		}
	}
	if input.RestrictToolNetwork && !VendorSupportsToolNetworkDenial(cfg.Vendor) {
		return nil, unsupportedToolNetworkDenialError(cfg.Vendor)
	}
	if input.RestrictToolNetwork {
		input.Env = maps.Clone(input.Env)
		if input.Env == nil {
			input.Env = map[string]string{}
		}
		input.Env["GIT_AUTHOR_NAME"] = "Looper Agent"
		input.Env["GIT_AUTHOR_EMAIL"] = "looper-agent@localhost"
		input.Env["GIT_COMMITTER_NAME"] = "Looper Agent"
		input.Env["GIT_COMMITTER_EMAIL"] = "looper-agent@localhost"
	}
	resume, err := e.resolveNativeResume(ctx, input)
	if err != nil {
		return nil, err
	}

	// Supervisor lease before cmd.Start (#576). When Owner is nil (unit tests),
	// containment still binds but there is no live registry entry for stop.
	var lease SpawnLease
	if e.owner != nil {
		lease, err = e.owner.AdmitSpawn(ctx, SpawnMeta{
			LoopID:                 input.LoopID,
			RunID:                  input.RunID,
			ExecutionID:            executionID,
			Vendor:                 string(cfg.Vendor),
			BrownoutStickySnapshot: input.UseSnapshot && strings.TrimSpace(input.SnapshotVendor) != "",
		})
		if err != nil {
			return nil, err
		}
		if probeLease, ok := lease.(interface{ BrownoutProbe() bool }); ok {
			input.BrownoutProbe = probeLease.BrownoutProbe()
		}
		if probeLease, ok := lease.(interface{ BrownoutProbeGeneration() uint64 }); ok {
			input.BrownoutProbeGeneration = probeLease.BrownoutProbeGeneration()
		}
	}
	// The health admission timestamp must be captured after AdmitSpawn returns:
	// a half-open breaker may transition during that call, and a timestamp from
	// before it would make a genuine probe look like a stale execution.
	startedAt := e.now().UTC()
	startedAtISO := eventlog.FormatJavaScriptISOString(startedAt)
	releaseLease := func() {
		if lease != nil {
			lease.Release()
			lease = nil
		}
	}
	// If Start returns success, the execution owns release on terminal Wait.
	// On any failure path below, release immediately.
	defer func() {
		if lease != nil {
			// Only still set when we failed before transferring ownership to x.
			releaseLease()
		}
	}()

	spawnPrompt := input.Prompt
	if resume.Enabled && strings.TrimSpace(input.NativeResumePrompt) != "" {
		spawnPrompt = input.NativeResumePrompt
	}
	var toolSandbox *validationcmd.Sandbox
	if input.Assessment {
		toolSandbox, err = validationcmd.NewAssessmentSandbox(input.WorkingDirectory, "looper-assessment", "looper-assessment-")
		if err != nil {
			return nil, fmt.Errorf("prepare read-only assessment tool sandbox: %w", err)
		}
		defer func() {
			if toolSandbox != nil {
				toolSandbox.Cleanup()
			}
		}()
	} else if input.RestrictToolNetwork {
		toolSandbox, err = validationcmd.NewSandbox(input.WorkingDirectory, "looper-agent", "looper-agent-")
		if err != nil {
			return nil, fmt.Errorf("prepare credential-free agent tool sandbox: %w", err)
		}
		defer func() {
			// On successful Start the execution owns cleanup until terminal Wait.
			if toolSandbox != nil {
				toolSandbox.Cleanup()
			}
		}()
	}
	command, args := ResolveSpawnWithNativeResume(cfg, input.WorkingDirectory, spawnPrompt, resume.SessionID, resume.Enabled)
	if input.Assessment {
		command, args = resolveAssessmentSpawn(cfg, spawnPrompt, toolSandbox)
	} else if input.RestrictToolNetwork {
		args, err = enforceToolNetworkDenied(cfg.Vendor, args, spawnPrompt, toolSandbox)
		if err != nil {
			return nil, err
		}
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = input.WorkingDirectory
	processcontainment.Configure(cmd)
	cmd.Env = buildCommandEnv(input.WorkingDirectory, spawnPrompt, cfg.Env, input.Env)

	maxOutputBytes := input.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	grace := input.GracefulShutdown
	if grace <= 0 {
		grace = 5 * time.Second
	}

	x := &execution{
		executor:           e,
		input:              input,
		executionID:        executionID,
		startedAt:          startedAt,
		command:            command,
		args:               args,
		startedAtISO:       startedAtISO,
		process:            cmd,
		timeout:            input.Timeout,
		heartbeatTimeout:   input.HeartbeatTimeout,
		gracefulShutdown:   grace,
		maxOutputBytes:     maxOutputBytes,
		lastHeartbeatAtISO: startedAtISO,
		lastOutputAt:       startedAt,
		status:             "running",
		nativeSessionID:    resume.SessionID,
		nativeResumeMode:   resume.Mode,
		nativeResumeStatus: resume.Status,
		killCh:             make(chan string, 1),
		doneCh:             make(chan execOutcome, 1),
		lease:              lease,
		toolSandbox:        toolSandbox,
	}
	x.stdoutLogPath, x.stderrLogPath = e.executionLogPaths(input, executionID)
	x.initializePersistedLogs()
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}

	spawnCtx := ctx
	if lease != nil {
		spawnCtx = lease.Context()
	}
	if err := spawnCtx.Err(); err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		if resume.Enabled {
			// Cancellation must not start a fallback process (#576).
			if err := spawnCtx.Err(); err != nil {
				return nil, err
			}
			if markErr := e.markNativeResumeFailed(ctx, resume.SourceExecutionID, err.Error()); markErr == nil && e.logDir != "" {
				// best-effort marker only; command fallback is the important recovery behavior
			}
			command, args = ResolveSpawn(cfg, input.WorkingDirectory, input.Prompt)
			if input.Assessment {
				command, args = resolveAssessmentSpawn(cfg, input.Prompt, toolSandbox)
			} else if input.RestrictToolNetwork {
				restricted, restrictErr := enforceToolNetworkDenied(cfg.Vendor, args, input.Prompt, toolSandbox)
				if restrictErr != nil {
					return nil, fmt.Errorf("%w (native resume fallback after: %v)", restrictErr, err)
				}
				args = restricted
			}
			cmd = exec.Command(command, args...)
			cmd.Dir = input.WorkingDirectory
			processcontainment.Configure(cmd)
			cmd.Env = buildCommandEnv(input.WorkingDirectory, input.Prompt, cfg.Env, input.Env)
			cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
			cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}
			x.mu.Lock()
			x.command = command
			x.args = args
			x.process = cmd
			x.nativeSessionID = ""
			x.nativeResumeMode = "checkpoint_restart"
			x.nativeResumeStatus = "fallback_started"
			x.nativeResumeError = err.Error()
			x.mu.Unlock()
			if err := spawnCtx.Err(); err != nil {
				return nil, err
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start agent command: %w (native resume fallback after: %v)", startErr, err)
			}
		} else {
			return nil, fmt.Errorf("start agent command: %w", err)
		}
	}
	x.captureProcessStart()

	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  grace,
		DrainTimeout: grace + 15*time.Second,
	})
	if err != nil {
		_ = killStartedCmd(cmd)
		return nil, fmt.Errorf("bind agent containment handle: %w", err)
	}
	x.handle = handle

	if lease != nil {
		if err := lease.BindHandle(handle, x.Kill); err != nil {
			// BindHandle already confirmed-drained on stop race.
			return nil, err
		}
	}

	// Initial ownership observation is hard: fail Start loud and do not leave
	// an unowned live process (#578). Persist before transferring lease so the
	// deferred Release still drains Supervisor ownership when reap confirms dead.
	if err := x.persistStatus(ctx, "running", nil, nil, nil); err != nil {
		outErr := x.reapOnOwnershipPersistFailure(cmd, grace, err, "persist initial agent execution ownership")
		// When Kill cannot confirm death, keep the registry handle: dropping the
		// lease here would leave a still-live agent with neither a durable row
		// nor an owner (Start returns no Execution).
		if x.handle != nil && !x.handle.ConfirmedDead() {
			lease = nil
		}
		return nil, outErr
	}

	// Transfer lease ownership to execution; defer must not Release on success.
	lease = nil

	resumeSessionID, resumeMode, resumeStatus, _ := x.nativeResumeSnapshot()
	e.appendLifecycleEvent("agent.invoked", input, executionID, map[string]any{"command": command, "args": args, "cwd": input.WorkingDirectory, "nativeResumeMode": resumeMode, "nativeResumeStatus": resumeStatus, "nativeSessionId": resumeSessionID}, startedAtISO)

	go x.run(ctx)
	toolSandbox = nil
	return x, nil
}

func killStartedCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	return nil
}

type execOutcome struct {
	result Result
	err    error
}

type execution struct {
	executor           *ConfiguredExecutor
	input              RunInput
	executionID        string
	startedAt          time.Time
	command            string
	args               []string
	startedAtISO       string
	process            *exec.Cmd
	handle             *processcontainment.Handle
	lease              SpawnLease
	timeout            time.Duration
	heartbeatTimeout   time.Duration
	gracefulShutdown   time.Duration
	maxOutputBytes     int
	lastHeartbeatAtISO string
	lastOutputAt       time.Time
	lastProgressAt     time.Time

	mu        sync.Mutex
	persistMu sync.Mutex // one ordered writer per execution (#578)
	// lastPersisted* (under persistMu) form the monotonic gate for live
	// observations: stdout/stderr handlers snapshot under mu but persist under
	// persistMu, so an older cumulative snapshot can arrive after a newer one
	// persisted and must not overwrite it. Status is sampled at persist-call
	// time — independent of the snapshot — so a stale-by-count observation can
	// still carry the one-way running→cancelling transition, which is then
	// written with the retained newer snapshot values.
	lastPersistedHeartbeatCount int64
	lastPersistedHeartbeatAt    string
	lastPersistedOutputJSON     *string
	lastPersistedStatus         string
	terminalPersisted           bool
	hardPersistReported         bool
	status                      string
	stdout                      []byte
	stderr                      []byte
	stdoutLogPath               string
	stderrLogPath               string
	persistedLogWriteFailed     bool
	heartbeatCount              int64
	nativeSessionID             string
	nativeResumeMode            string
	nativeResumeStatus          string
	nativeResumeError           string
	processBirth                processidentity.Birth
	leaseReleased               bool
	toolSandbox                 *validationcmd.Sandbox

	killCh chan string
	doneCh chan execOutcome
}

func (x *execution) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case out := <-x.doneCh:
		x.doneCh <- out
		return out.result, out.err
	}
}

func (x *execution) Kill(reason string) error {
	select {
	case x.killCh <- reason:
	default:
	}
	return nil
}

func (x *execution) signalProcessGroup(signal syscall.Signal) error {
	if x.handle != nil {
		return x.handle.SignalGroup(signal)
	}
	if x.process == nil || x.process.Process == nil {
		return os.ErrProcessDone
	}
	pid := x.process.Process.Pid
	if pid <= 0 {
		return x.process.Process.Signal(signal)
	}
	if err := syscall.Kill(-pid, signal); err != nil {
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func (x *execution) killProcessGroup() error {
	if x.handle != nil {
		ctx, cancel := context.WithTimeout(context.Background(), x.gracefulShutdown+15*time.Second)
		defer cancel()
		return x.handle.Kill(ctx)
	}
	if x.process == nil || x.process.Process == nil {
		return os.ErrProcessDone
	}
	pid := x.process.Process.Pid
	if pid > 0 {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
			return nil
		}
	}
	return x.process.Process.Kill()
}

func (x *execution) releaseLease() {
	x.mu.Lock()
	if x.leaseReleased || x.lease == nil {
		x.mu.Unlock()
		return
	}
	// Prefer a durable registry orphan over an unowned live process: do not drop
	// Supervisor ownership while containment may still be live (e.g. Kill timeout
	// after failed ownership persist on native-resume fallback).
	if x.handle != nil && !x.handle.ConfirmedDead() {
		x.mu.Unlock()
		return
	}
	x.leaseReleased = true
	lease := x.lease
	x.mu.Unlock()
	lease.Release()
}

func (x *execution) waitLeader() error {
	if x.handle != nil {
		return x.handle.Wait(context.Background())
	}
	if x.process == nil {
		return os.ErrProcessDone
	}
	return x.process.Wait()
}

func (x *execution) run(ctx context.Context) {
	defer x.releaseLease()
	defer x.cleanupToolSandbox()

	waitCh := make(chan error, 1)
	go func() { waitCh <- x.waitLeader() }()

	// Merge caller ctx with lease cancellation so stop during run is observed.
	runCtx := ctx
	if x.lease != nil {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithCancel(ctx)
		go func() {
			select {
			case <-x.lease.Context().Done():
				cancel()
			case <-runCtx.Done():
			}
		}()
		defer cancel()
	}

	var (
		waitErr         error
		timedOut        bool
		timeoutType     string
		killed          bool
		killReason      string
		preTimeoutError string
		graceKillTimer  <-chan time.Time
		timeoutTimer    <-chan time.Time
		inactivityTimer *time.Ticker
		termDelivered   bool
		terminateOnce   sync.Once
		terminateSignal = func() {
			terminateOnce.Do(func() {
				if x.handle == nil && (x.process == nil || x.process.Process == nil) {
					return
				}
				if err := x.signalProcessGroup(syscall.SIGTERM); err != nil {
					if err != os.ErrProcessDone {
						_ = x.killProcessGroup()
					}
					return
				}
				termDelivered = true
				grace := x.gracefulShutdown
				if grace <= 0 {
					grace = 5 * time.Second
				}
				graceKillTimer = time.After(grace)
			})
		}
	)

	if x.timeout > 0 {
		timeoutTimer = time.After(x.timeout)
	}
	if x.heartbeatTimeout > 0 {
		interval := x.heartbeatTimeout
		if interval > time.Second {
			interval = time.Second
		}
		inactivityTimer = time.NewTicker(interval)
		defer inactivityTimer.Stop()
	}

	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-timeoutTimer:
			timeoutTimer = nil
			if timedOut || killed {
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			if killReason == "" {
				killReason = fmt.Sprintf("agent max runtime timed out after %s", x.timeout)
			}
			select {
			case reason := <-x.killCh:
				killed = true
				killReason = reason
				x.setStatus("killed")
				terminateSignal()
				continue
			default:
			}
			if runCtx.Err() != nil {
				killed = true
				killReason = runCtx.Err().Error()
				x.setStatus("killed")
				terminateSignal()
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			timedOut = true
			timeoutType = "max_runtime"
			x.setStatus("timeout")
			terminateSignal()
		case <-tickerChan(inactivityTimer):
			if timedOut || killed {
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			if x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			if killReason == "" {
				killReason = fmt.Sprintf("agent idle timed out after %s without observable progress", x.heartbeatTimeout)
			}
			select {
			case reason := <-x.killCh:
				killed = true
				killReason = reason
				x.setStatus("killed")
				terminateSignal()
				continue
			default:
			}
			if runCtx.Err() != nil {
				killed = true
				killReason = runCtx.Err().Error()
				x.setStatus("killed")
				terminateSignal()
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			if x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			timedOut = true
			timeoutType = "idle"
			x.setStatus("timeout")
			terminateSignal()
		case reason := <-x.killCh:
			killed = true
			killReason = reason
			x.setStatus("killed")
			terminateSignal()
		case <-runCtx.Done():
			killed = true
			if killReason == "" {
				if runCtx.Err() != nil {
					killReason = runCtx.Err().Error()
				} else {
					killReason = context.Canceled.Error()
				}
			}
			x.setStatus("killed")
			terminateSignal()
		case <-graceKillTimer:
			graceKillTimer = nil
			_ = x.killProcessGroup()
		}
	}
	if termDelivered && (killed || timedOut) {
		_ = x.killProcessGroup()
	}
	if timedOut {
		// The callback persists the timeout snapshot. Run it only after the
		// process group is confirmed dead, so no writer can change the worktree
		// between the snapshot and the retry preservation check.
		if err := x.ensureConfirmedDeadBeforeTerminal(); err != nil {
			preTimeoutError = err.Error()
		} else {
			preTimeoutError = x.observeBeforeTimeout(timeoutType)
		}
	}

	stdout, stderr := x.resolveOutputLogs()
	status := x.finalStatus(timedOut, killed)
	if waitErr != nil && status == "failed" && strings.TrimSpace(stderr) == "" {
		stderr = waitErr.Error()
		if x.appendPersistedLog(x.stderrLogPath, []byte(stderr)) {
			x.markPersistedLogWriteFailed()
		}
	}
	errorMessage := ""
	if status == "failed" || status == "timeout" || status == "killed" {
		errorMessage = strings.TrimSpace(stderr)
		if errorMessage == "" {
			errorMessage = killReason
		}
	}
	completion := parseCompletion(stdout, stderr)
	if x.jsonMode() {
		// codex --json: stdout is JSONL. The completion marker + final message live
		// inside agent_message / command-output events, and the session is the
		// thread id — read both from the parsed event stream instead of raw stdout.
		tr := newCodexJSONLTranslator()
		tr.ingestAll(stdout)
		completion = parseCompletion(tr.combinedText(), stderr)
		// Assessments must never become resumable native sessions.
		if tr.threadID != "" && !x.input.Assessment {
			x.nativeSessionID = tr.threadID
		}
	}
	if status != "completed" {
		completion = completionParse{ParseStatus: "missing"}
	}
	if completion.Summary == "" {
		completion.Summary = errorMessage
		if completion.Summary == "" {
			completion.Summary = summarizeLogs(stdout, stderr)
		}
	}
	endedAtISO := eventlog.FormatJavaScriptISOString(x.executor.now().UTC())
	lastProgressAt := x.lastProgressAtISO()
	_, nativeResumeMode, nativeResumeStatus, _ := x.nativeResumeSnapshot()
	result := Result{
		Status:                       status,
		Summary:                      completion.Summary,
		Stdout:                       stdout,
		Stderr:                       stderr,
		ParseStatus:                  completion.ParseStatus,
		CompletionSignal:             completion.CompletionSignal,
		CompletionPayload:            completion.CompletionPayload,
		Artifacts:                    append([]string(nil), completion.Artifacts...),
		ChangedFiles:                 append([]string(nil), completion.ChangedFiles...),
		Commits:                      append([]string(nil), completion.Commits...),
		Lifecycle:                    completion.Lifecycle,
		HeartbeatCount:               x.heartbeatCountValue(),
		TimeoutType:                  timeoutType,
		ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
		ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
		ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
		LastProgressAt:               lastProgressAt,
		PreTimeoutError:              preTimeoutError,
		NativeResumeMode:             nativeResumeMode,
		NativeResumeStatus:           nativeResumeStatus,
		PID:                          x.leaderPID(),
	}
	if x.shouldFallbackNativeResume(status, stdout, stderr) {
		// Cancellation / stop must not spawn a second process (#576).
		if runCtx.Err() == nil && (x.lease == nil || x.lease.Context().Err() == nil) {
			if fallbackResult, fallbackErrorMessage, ok, fallbackErr := x.runCheckpointFallback(runCtx, errorMessage); ok {
				result = fallbackResult
				status = fallbackResult.Status
				timeoutType = fallbackResult.TimeoutType
				errorMessage = fallbackErrorMessage
				endedAtISO = eventlog.FormatJavaScriptISOString(x.executor.now().UTC())
			} else if fallbackErr != nil {
				// Fallback refused or ownership persist failed. A restriction
				// refusal (ErrStaticConfigMismatch) surfaces as the execution
				// error so the runner classifies it as manual intervention; an
				// ownership persist failure reaps the process only when Kill
				// confirmed dead (releaseLease keeps ownership otherwise).
				x.doneCh <- execOutcome{result: result, err: fallbackErr}
				return
			}
		}
	}
	x.finalizeNativeResumeStatus(status, errorMessage, result.Stderr)
	_, result.NativeResumeMode, result.NativeResumeStatus, _ = x.nativeResumeSnapshot()

	// No terminal observation before containment is confirmed dead for owned
	// executions (ties to #574/#576 / ADR-0015 R5).
	if err := x.ensureConfirmedDeadBeforeTerminal(); err != nil {
		x.reportHardPersistFailure(err)
		x.doneCh <- execOutcome{
			result: result,
			err:    errors.Join(ErrExecutionPersistence, fmt.Errorf("containment not confirmed dead before terminal observation: %w", err)),
		}
		return
	}

	persistErr := x.persistFinal(status, result, errorMessage, endedAtISO)
	// persistFinal may promote a session id extracted from terminal output from
	// unavailable to captured. Refresh the returned metadata after that durable
	// normalization so Worker checkpoints never lag the AgentExecution record.
	_, result.NativeResumeMode, result.NativeResumeStatus, _ = x.nativeResumeSnapshot()
	if hard := x.classifyPersistError(persistErr); hard != nil {
		x.reportHardPersistFailure(hard)
		persistErr = errors.Join(ErrExecutionPersistence, fmt.Errorf("persist terminal agent execution: %w", hard))
	} else if persistErr != nil {
		// Soft terminal write issues still fail loud (do not report success).
		persistErr = errors.Join(ErrExecutionPersistence, fmt.Errorf("persist terminal agent execution: %w", persistErr))
	}

	eventType := "agent.completed"
	if status == "timeout" {
		switch timeoutType {
		case "idle":
			eventType = "agent.idle_timeout"
		case "max_runtime":
			eventType = "agent.max_runtime_timeout"
		default:
			eventType = "agent.timed_out"
		}
	} else if status == "killed" {
		eventType = "agent.killed"
	}
	if persistErr == nil {
		x.executor.appendLifecycleEvent(eventType, x.input, x.executionID, map[string]any{
			"status":                       status,
			"timeoutType":                  timeoutType,
			"configuredIdleTimeoutSeconds": result.ConfiguredIdleTimeoutSeconds,
			"configuredMaxRuntimeSeconds":  result.ConfiguredMaxRuntimeSeconds,
			"elapsedRuntimeSeconds":        result.ElapsedRuntimeSeconds,
			"lastProgressAt":               result.LastProgressAt,
			"parseStatus":                  result.ParseStatus,
			"heartbeatCount":               result.HeartbeatCount,
			"summary":                      result.Summary,
		}, endedAtISO)
	}

	x.reportOutcome(status, result.ParseStatus, result.CompletionPayload, result.Stdout)
	x.doneCh <- execOutcome{result: result, err: persistErr}
}

// finalizeNativeResumeStatus makes the returned terminal result agree with the
// native-resume state that persistFinal records. Fallbacks have already
// replaced this state before this point, so only an attached native session
// that terminates without fallback is finalized here.
func (x *execution) finalizeNativeResumeStatus(status, errorMessage, stderr string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.nativeResumeMode != "native_resume" || status != "failed" {
		return
	}
	x.nativeResumeStatus = "failed"
	x.nativeResumeError = firstNonEmpty(x.nativeResumeError, errorMessage, strings.TrimSpace(stderr))
}

// reportOutcome feeds the daemon's agent-health gate. "killed" is excluded on
// purpose: it means looper stopped the agent (operator stop, shutdown drain),
// which says nothing about whether the provider is answering.
func (x *execution) reportOutcome(status, parseStatus, completionPayload, stdout string) {
	if x.executor == nil || x.executor.onOutcome == nil || status == "killed" {
		return
	}
	succeeded := status == "completed"
	if x.input.CompletionContract == CompletionContractFile {
		succeeded = succeeded && x.input.CompletionValidator != nil && x.input.CompletionValidator("")
	} else if x.input.CompletionContract == CompletionContractRawJSON {
		succeeded = succeeded && validRawJSONObject(stdout)
		if succeeded && x.input.CompletionValidator != nil {
			succeeded = x.input.CompletionValidator(FinalMessage(stdout))
		}
	} else if x.input.CompletionContract == CompletionContractRawJSONEnvelope {
		succeeded = succeeded && validRawJSONObjectEnvelope(stdout)
		if succeeded && x.input.CompletionValidator != nil {
			succeeded = x.input.CompletionValidator(FinalMessage(stdout))
		}
	} else if x.input.CompletionContract == CompletionContractReviewerMarker {
		succeeded = succeeded && parseStatus == "parsed" && validReviewerMarkerOutcome(completionPayload)
	} else if x.input.CompletionContract == CompletionContractReviewerPublication {
		succeeded = succeeded && parseStatus == "parsed" && validReviewerMarkerOutcome(completionPayload)
		if !succeeded && x.input.CompletionOutcomeValidator != nil {
			succeeded = x.input.CompletionOutcomeValidator()
		}
	} else if x.input.CompletionContract == CompletionContractWorkerHITL {
		succeeded = succeeded && parseStatus == "parsed" && validMarkerOutcome(completionPayload)
		if !succeeded && status == "completed" && x.input.CompletionOutcomeValidator != nil {
			succeeded = x.input.CompletionOutcomeValidator()
		}
	} else if x.input.CompletionContract == CompletionContractFixerMarker {
		succeeded = succeeded && parseStatus == "parsed" && validFixerMarkerOutcome(completionPayload)
	} else if x.input.CompletionContract == CompletionContractPlannerMarker {
		// Planner's runner treats the marker as optional. A malformed marker is
		// still a failed completion, while a caller validator owns the optional
		// workGraph schema when a marker is present (or receives an empty payload
		// when the marker is absent).
		succeeded = succeeded && (parseStatus == "missing" || (parseStatus == "parsed" && validMarkerOutcome(completionPayload)))
		if succeeded && x.input.CompletionValidator != nil {
			succeeded = x.input.CompletionValidator(completionPayload)
		}
	} else {
		succeeded = succeeded && parseStatus == "parsed" && validMarkerOutcome(completionPayload)
	}
	// Planner's runner treats a valid blocked marker as a completed Planner
	// step (it may still advance the checkpoint); do not apply the generic
	// retryable-block classification to that caller-owned contract. Other role
	// runners replay retryable blocks, so those remain provider-health failures.
	healthSucceeded := succeeded
	if x.input.CompletionContract != CompletionContractPlannerMarker {
		healthSucceeded = healthSucceeded && !declaresRetryableBlock(completionPayload)
	}
	x.executor.onOutcome(Outcome{
		ProjectID:               x.input.ProjectID,
		LoopID:                  x.input.LoopID,
		RunID:                   x.input.RunID,
		ExecutionID:             x.executionID,
		Vendor:                  string(x.executor.effectiveConfig(x.input).Vendor),
		BrownoutProbe:           x.input.BrownoutProbe,
		BrownoutProbeGeneration: x.input.BrownoutProbeGeneration,
		BrownoutStickySnapshot:  x.input.UseSnapshot && strings.TrimSpace(x.input.SnapshotVendor) != "",
		Status:                  status,
		// A zero exit code is not a valid agent completion by itself. The selected
		// structured output contract is the executor's completion authority;
		// missing or malformed output must feed the health gate as a failure so
		// brownout backs off from agents that exit cleanly without doing the work.
		Succeeded: healthSucceeded,
		StartedAt: x.startedAt,
	})
}
func (x *execution) observeBeforeTimeout(timeoutType string) string {
	callback := x.input.OnBeforeTimeout
	if callback == nil {
		return ""
	}
	budget := x.input.TimeoutObservationBudget
	if budget <= 0 {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := callback(ctx, TimeoutObservation{TimeoutType: timeoutType, LastProgressAt: x.lastProgressAtISO()}); err != nil {
		return err.Error()
	}
	return ""
}

func (x *execution) cleanupToolSandbox() {
	x.mu.Lock()
	sandbox := x.toolSandbox
	x.toolSandbox = nil
	x.mu.Unlock()
	if sandbox != nil {
		sandbox.Cleanup()
	}
}

func (x *execution) shouldFallbackNativeResume(status string, stdout string, stderr string) bool {
	_, mode, resumeStatus, _ := x.nativeResumeSnapshot()
	return mode == "native_resume" && resumeStatus == "started" && status == "failed" && isNativeResumeAttachFailure(stdout, stderr)
}

func isNativeResumeAttachFailure(stdout string, stderr string) bool {
	if strings.TrimSpace(stdout) != "" {
		return false
	}
	message := strings.TrimSpace(stderr)
	if message == "" {
		return false
	}
	for _, line := range strings.Split(message, "\n") {
		line = normalizeNativeResumeErrorLine(line)
		switch {
		case line == "resume failed" || strings.HasPrefix(line, "resume failed:"):
			return true
		case strings.HasPrefix(line, "failed to resume session") || strings.HasPrefix(line, "could not resume session") || strings.HasPrefix(line, "cannot resume session"):
			return true
		case strings.HasPrefix(line, "failed to resume conversation") || strings.HasPrefix(line, "could not resume conversation") || strings.HasPrefix(line, "cannot resume conversation"):
			return true
		}
	}
	return false
}

func normalizeNativeResumeErrorLine(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	line = strings.TrimPrefix(line, "error:")
	line = strings.TrimPrefix(strings.TrimSpace(line), "fatal:")
	return strings.TrimSpace(line)
}

func (x *execution) runCheckpointFallback(ctx context.Context, nativeError string) (Result, string, bool, error) {
	// Stop/cancel must not spawn a second process after the first failed attach.
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Result{}, "", false, nil
		}
	}
	if x.lease != nil {
		if err := x.lease.Context().Err(); err != nil {
			return Result{}, "", false, nil
		}
	}

	// Atomic rebind admission with the Supervisor registry: BeginRebind under
	// the registry lock, then Start/Bind/RebindHandle. BeginLoopStop waits for
	// this window so stop cannot return while a second process is live outside
	// the registry between Start and RebindHandle.
	type rebindLease interface {
		BeginRebind() error
		AbortRebind()
		RebindHandle(*processcontainment.Handle, SoftKillFunc) error
	}
	var rebind rebindLease
	if x.lease != nil {
		if r, ok := x.lease.(rebindLease); ok {
			if err := r.BeginRebind(); err != nil {
				return Result{}, "", false, nil
			}
			rebind = r
			defer func() {
				// No-op if RebindHandle already ended the window.
				rebind.AbortRebind()
			}()
		}
	}

	cfg := x.executor.effectiveConfig(x.input)
	command, args := ResolveSpawn(cfg, x.input.WorkingDirectory, x.input.Prompt)
	if x.input.Assessment {
		command, args = resolveAssessmentSpawn(cfg, x.input.Prompt, x.toolSandbox)
	} else if x.input.RestrictToolNetwork {
		restricted, restrictErr := enforceToolNetworkDenied(cfg.Vendor, args, x.input.Prompt, x.toolSandbox)
		if restrictErr != nil {
			// Fail closed: a restart that cannot re-apply the restriction must
			// not spawn an unrestricted process. Propagate the original error
			// (wrapped in ErrStaticConfigMismatch) so the runner classifies
			// this as manual intervention instead of burning retries on a
			// static config mismatch — matching the native-resume fallback.
			x.mu.Lock()
			x.status = "failed"
			x.nativeResumeStatus = "fallback_failed"
			x.nativeResumeError = firstNonEmpty(restrictErr.Error(), nativeError)
			x.mu.Unlock()
			return Result{}, "", false, restrictErr
		}
		args = restricted
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = x.input.WorkingDirectory
	processcontainment.Configure(cmd)
	cmd.Env = buildCommandEnv(x.input.WorkingDirectory, x.input.Prompt, cfg.Env, x.input.Env)
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.onOutput("stderr", chunk) }}

	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.command = command
	x.args = args
	x.process = cmd
	x.status = "running"
	x.stdout = nil
	x.stderr = nil
	x.nativeSessionID = ""
	x.nativeResumeMode = "checkpoint_restart"
	x.nativeResumeStatus = "fallback_started"
	x.nativeResumeError = nativeError
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	x.processBirth = processidentity.Birth{}
	x.mu.Unlock()

	if err := cmd.Start(); err != nil {
		x.mu.Lock()
		x.status = "failed"
		x.nativeResumeStatus = "fallback_failed"
		x.nativeResumeError = firstNonEmpty(err.Error(), nativeError)
		x.mu.Unlock()
		return Result{}, "", false, nil
	}
	x.captureProcessStart()

	grace := x.gracefulShutdown
	if grace <= 0 {
		grace = 5 * time.Second
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  grace,
		DrainTimeout: grace + 15*time.Second,
	})
	if err != nil {
		_ = killStartedCmd(cmd)
		x.mu.Lock()
		x.status = "failed"
		x.nativeResumeStatus = "fallback_failed"
		x.nativeResumeError = firstNonEmpty(err.Error(), nativeError)
		x.mu.Unlock()
		return Result{}, "", false, nil
	}
	// Prior handle already Wait'd in the outer run loop; replace for stop-kill.
	x.mu.Lock()
	x.handle = handle
	x.mu.Unlock()
	if rebind != nil {
		// Re-bind so haltLoop finds the live fallback process. Prior entry was
		// released only on full execution end; update handle in place via a
		// second BindHandle is not valid (lease left pending).
		if err := rebind.RebindHandle(handle, x.Kill); err != nil {
			// Registry already killUnowned'd on stop/admission refuse; ensure
			// local cleanup and surface killed so persistFinal does not keep
			// the stale native-resume attach "failed" over markExecutionCancelling.
			_ = handle.Kill(context.Background())
			errMsg := firstNonEmpty(err.Error(), nativeError)
			x.mu.Lock()
			x.status = "killed"
			x.nativeResumeStatus = "fallback_failed"
			x.nativeResumeError = errMsg
			x.mu.Unlock()
			return Result{
				Status:                       "killed",
				Summary:                      firstNonEmpty(errMsg, "native resume fallback refused during stop"),
				ParseStatus:                  "missing",
				HeartbeatCount:               x.heartbeatCountValue(),
				ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
				ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
				ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
				LastProgressAt:               x.lastProgressAtISO(),
				NativeResumeMode:             "checkpoint_restart",
				NativeResumeStatus:           "fallback_failed",
				PID:                          x.leaderPID(),
			}, errMsg, true, nil
		}
	}

	// Ownership observation after spawn+bind: fail loud and reap if storage is broken.
	// Join Kill failures and keep registry ownership unless ConfirmedDead so a
	// deferred run releaseLease cannot drop the only live handle on drain timeout.
	if err := x.persistStatus(ctx, "running", nil, nil, nil); err != nil {
		return Result{}, "", false, x.reapOnOwnershipPersistFailure(cmd, grace, err, "persist fallback agent execution ownership")
	}
	x.executor.appendLifecycleEvent("agent.native_resume_fallback_started", x.input, x.executionID, map[string]any{"command": command, "args": args, "nativeResumeError": nativeError}, nowISO)

	waitCh := make(chan error, 1)
	go func() { waitCh <- handle.Wait(context.Background()) }()
	var (
		waitErr         error
		timedOut        bool
		killed          bool
		timeoutType     string
		killReason      string
		preTimeoutError string
		timeoutTimer    <-chan time.Time
		graceKillTimer  <-chan time.Time
		idleTicker      *time.Ticker
		termDelivered   bool
	)
	if x.timeout > 0 {
		timeoutTimer = time.After(x.timeout)
	}
	if x.heartbeatTimeout > 0 {
		interval := x.heartbeatTimeout
		if interval > time.Second {
			interval = time.Second
		}
		idleTicker = time.NewTicker(interval)
		defer idleTicker.Stop()
	}
	terminate := func() {
		if handle == nil {
			return
		}
		if err := handle.SignalGroup(syscall.SIGTERM); err != nil {
			if err != os.ErrProcessDone {
				ctx, cancel := context.WithTimeout(context.Background(), grace+15*time.Second)
				_ = handle.Kill(ctx)
				cancel()
			}
			return
		}
		termDelivered = true
		graceKillTimer = time.After(grace)
	}
	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-timeoutTimer:
			timeoutTimer = nil
			if timedOut || killed {
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			killReason = fmt.Sprintf("agent max runtime timed out after %s", x.timeout)
			select {
			case reason := <-x.killCh:
				killed = true
				killReason = reason
				terminate()
				continue
			default:
			}
			if ctx.Err() != nil {
				killed = true
				killReason = ctx.Err().Error()
				terminate()
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			timedOut = true
			timeoutType = "max_runtime"
			terminate()
		case <-tickerChan(idleTicker):
			if timedOut || killed || x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			killReason = fmt.Sprintf("agent idle timed out after %s without observable progress", x.heartbeatTimeout)
			select {
			case reason := <-x.killCh:
				killed = true
				killReason = reason
				terminate()
				continue
			default:
			}
			if ctx.Err() != nil {
				killed = true
				killReason = ctx.Err().Error()
				terminate()
				continue
			}
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			if x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			timedOut = true
			timeoutType = "idle"
			terminate()
		case reason := <-x.killCh:
			killed = true
			killReason = reason
			terminate()
		case <-ctx.Done():
			killed = true
			killReason = ctx.Err().Error()
			terminate()
		case <-graceKillTimer:
			graceKillTimer = nil
			ctx, cancel := context.WithTimeout(context.Background(), grace+15*time.Second)
			_ = handle.Kill(ctx)
			cancel()
		}
	}
	if termDelivered && (killed || timedOut) {
		ctx, cancel := context.WithTimeout(context.Background(), grace+15*time.Second)
		_ = handle.Kill(ctx)
		cancel()
	}
	if timedOut {
		// A native-resume fallback has a fresh containment handle. Do not let
		// its timeout snapshot race descendants that survived the leader exit.
		if err := x.ensureConfirmedDeadBeforeTerminal(); err != nil {
			preTimeoutError = err.Error()
		} else {
			preTimeoutError = x.observeBeforeTimeout(timeoutType)
		}
	}
	stdout := x.stdoutString()
	stderr := x.stderrString()
	now = x.executor.now().UTC()
	nowISO = eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.stdout = []byte(stdout)
	x.stderr = []byte(stderr)
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	x.heartbeatCount++
	x.mu.Unlock()
	status := "completed"
	if timedOut {
		status = "timeout"
	} else if killed {
		status = "killed"
	} else if waitErr != nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0) {
		status = "failed"
	}
	errorMessage := ""
	if status != "completed" {
		errorMessage = strings.TrimSpace(stderr)
		if errorMessage == "" && waitErr != nil {
			errorMessage = waitErr.Error()
		}
		if errorMessage == "" {
			errorMessage = killReason
		}
	}
	completion := parseCompletion(stdout, stderr)
	if status != "completed" {
		completion = completionParse{ParseStatus: "missing"}
	}
	if completion.Summary == "" {
		completion.Summary = firstNonEmpty(errorMessage, summarizeLogs(stdout, stderr))
	}
	x.mu.Lock()
	x.status = status
	x.nativeResumeStatus = "fallback_completed"
	if status != "completed" {
		x.nativeResumeStatus = "fallback_failed"
	}
	x.mu.Unlock()
	_, nativeResumeMode, nativeResumeStatus, _ := x.nativeResumeSnapshot()
	return Result{
		Status:                       status,
		Summary:                      completion.Summary,
		Stdout:                       stdout,
		Stderr:                       stderr,
		ParseStatus:                  completion.ParseStatus,
		CompletionSignal:             completion.CompletionSignal,
		CompletionPayload:            completion.CompletionPayload,
		Artifacts:                    append([]string(nil), completion.Artifacts...),
		ChangedFiles:                 append([]string(nil), completion.ChangedFiles...),
		Commits:                      append([]string(nil), completion.Commits...),
		Lifecycle:                    completion.Lifecycle,
		HeartbeatCount:               x.heartbeatCountValue(),
		TimeoutType:                  timeoutType,
		ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
		ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
		ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
		LastProgressAt:               x.lastProgressAtISO(),
		PreTimeoutError:              preTimeoutError,
		NativeResumeMode:             nativeResumeMode,
		NativeResumeStatus:           nativeResumeStatus,
		PID:                          x.leaderPID(),
	}, errorMessage, true, nil
}

func (x *execution) onOutput(stream string, chunk []byte) {
	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.heartbeatCount++
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	if stream == "stdout" {
		if x.appendPersistedLog(x.stdoutLogPath, chunk) {
			x.persistedLogWriteFailed = true
		}
		x.stdout = appendTailBounded(x.stdout, chunk, x.maxOutputBytes)
	} else {
		if x.appendPersistedLog(x.stderrLogPath, chunk) {
			x.persistedLogWriteFailed = true
		}
		x.stderr = appendTailBounded(x.stderr, chunk, x.maxOutputBytes)
	}
	heartbeatCount := x.heartbeatCount
	stdout := string(x.stdout)
	stderr := string(x.stderr)
	// Capture the native session id AS SOON as it appears, so it's persisted while
	// the run is live (a human taking over mid-run needs it — completion is too
	// late). Text-mode ids can stream in across chunks, so re-extract each time; the
	// codex --json thread id arrives whole in a thread.started line, so capture it
	// once (only when text extraction found nothing and it's not already known).
	// Assessments never capture a session id — they are not resumable.
	if !x.input.Assessment {
		if nativeSessionID := extractNativeSessionID(stdout, stderr); nativeSessionID != "" {
			x.nativeSessionID = nativeSessionID
		} else if x.jsonMode() && strings.TrimSpace(x.nativeSessionID) == "" {
			if threadID := extractCodexThreadID(stdout); threadID != "" {
				x.nativeSessionID = threadID
			}
		}
	}
	x.mu.Unlock()

	outputJSON := x.outputJSON(stdout, stderr)
	// Mid-life output must not publish terminal observations: timeout/kill may
	// already be set in-memory while the process group is still draining, and
	// terminal rows are immutable (ADR-0015 R5 / ensureConfirmedDeadBeforeTerminal).
	if err := x.persistStatus(context.Background(), x.liveObservationStatus(), &heartbeatCount, &nowISO, &outputJSON); err != nil {
		if hard := x.classifyPersistError(err); hard != nil {
			// First hard mid-life failure closes admission (degraded). Soft
			// cancel/conflict/busy-after-retry do not sticky-degrade.
			x.reportHardPersistFailure(hard)
		}
	}
	x.bumpRunHeartbeat(nowISO)
	x.maybeEmitProgress(now, stdout, stderr)
}

// maybeEmitProgress hands a throttled activity snapshot (last few output lines +
// elapsed) to the injected OnProgress callback, at most once per interval. It
// reads both stdout and stderr because agents narrate on different streams
// (codex logs activity to stderr; the final answer lands on stdout).
func (x *execution) maybeEmitProgress(now time.Time, stdout, stderr string) {
	if x.executor == nil || x.executor.onProgress == nil {
		return
	}
	x.mu.Lock()
	if !x.lastProgressAt.IsZero() && now.Sub(x.lastProgressAt) < liveProgressInterval {
		x.mu.Unlock()
		return
	}
	x.lastProgressAt = now
	x.mu.Unlock()
	elapsed := int64(now.Sub(x.startedAt).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	var tail []string
	if x.jsonMode() {
		// Structured codex tool-call feed ("✅ <cmd>") parsed from the JSONL stream.
		tail = codexToolTail(stdout, liveProgressTailLines)
	} else {
		// Combine streams; the last N lines skew toward whichever is actively writing.
		tail = lastNonEmptyLines(stdout+"\n"+stderr, liveProgressTailLines)
	}
	x.executor.onProgress(context.Background(), ProgressUpdate{
		LoopID:         x.input.LoopID,
		RunID:          x.input.RunID,
		ExecutionID:    x.input.ExecutionID,
		TailLines:      tail,
		ElapsedSeconds: elapsed,
	})
}

// jsonMode reports whether this run is a codex `--json` run (structured events).
func (x *execution) jsonMode() bool {
	if x.executor == nil {
		return false
	}
	cfg := x.executor.effectiveConfig(x.input)
	return cfg.LiveToolEvents && runtimeCapabilitySupported(cfg.Vendor, CapabilityStructuredLiveEvents)
}

// codexToolTail renders the last n command executions from a codex JSONL blob.
func codexToolTail(stdout string, n int) []string {
	tr := newCodexJSONLTranslator()
	tr.ingestAll(stdout)
	return tr.recentToolLines(n)
}

var ansiEscapeRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// lastNonEmptyLines returns the final n meaningful lines of s, in order: ANSI
// colour codes stripped, and pure-punctuation / diff-fragment / lifecycle-hook
// noise skipped so the tail reads as activity rather than terminal spew.
func lastNonEmptyLines(s string, n int) []string {
	if n <= 0 || strings.TrimSpace(s) == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(ansiEscapeRe.ReplaceAllString(lines[i], ""))
		if !meaningfulProgressLine(line) {
			continue
		}
		out = append([]string{line}, out...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// meaningfulProgressLine drops lines that are pure diff/bracket punctuation or
// lifecycle-hook noise, which otherwise dominate a raw agent stream.
func meaningfulProgressLine(line string) bool {
	if len(line) < 4 {
		return false
	}
	if strings.Trim(line, "+-}{[]()<> ,;:.\"'`|") == "" {
		return false
	}
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "hook:") || strings.Contains(lower, " hook:") || strings.HasPrefix(lower, "+ ") || strings.HasPrefix(lower, "- ") {
		return false
	}
	return true
}

func (x *execution) bumpRunHeartbeat(nowISO string) {
	if x.input.RunID == "" || x.executor.repos == nil || x.executor.repos.Runs == nil {
		return
	}
	// Targeted forward-only column update: a full-record read/modify/upsert
	// here could resurrect stale run state captured before a concurrent writer,
	// and an out-of-order heartbeat must not move liveness evidence backward.
	_ = x.executor.repos.Runs.TouchHeartbeat(context.Background(), x.input.RunID, nowISO)
}

// persistStatus writes a live (or initial) observation. One ordered writer per
// execution (persistMu). After a terminal observation is recorded, live writes
// are no-ops so a stale heartbeat cannot race terminal immutability.
func (x *execution) persistStatus(ctx context.Context, status string, heartbeatCount *int64, heartbeatAt *string, outputJSON *string) error {
	x.persistMu.Lock()
	defer x.persistMu.Unlock()
	if x.terminalPersisted {
		return nil
	}
	// Snapshots are cumulative (bounded output tails plus a heartbeat count
	// that only grows under mu), so a snapshot older than the last persisted
	// one carries strictly less information: drop it instead of moving durable
	// heartbeat/output state backward. The exception is a fresher status
	// sample: the heartbeat count orders output snapshots but is not the
	// authority for the independently sampled lifecycle status, so a
	// running→cancelling transition is written with the retained newer
	// snapshot values instead of being lost for the rest of the drain.
	if heartbeatCount != nil && *heartbeatCount < x.lastPersistedHeartbeatCount {
		if status != "cancelling" || x.lastPersistedStatus == "cancelling" {
			return nil
		}
		retainedCount := x.lastPersistedHeartbeatCount
		retainedAt := x.lastPersistedHeartbeatAt
		heartbeatCount = &retainedCount
		heartbeatAt = &retainedAt
		outputJSON = x.lastPersistedOutputJSON
	}
	// The live status is one-way regardless of snapshot order: a callback that
	// sampled running before the kill can reach here after cancelling
	// persisted (with an equal or newer count) and must not downgrade it.
	if status == "running" && x.lastPersistedStatus == "cancelling" {
		status = "cancelling"
	}
	if x.executor.repos == nil || x.executor.repos.AgentExecutions == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	nativeSessionID, nativeResumeMode, nativeResumeStatus, nativeResumeError := x.nativeResumeSnapshot()
	metadata := mustJSON(x.executionMetadata(""))
	commandJSON := mustJSON(map[string]any{"command": x.command, "args": x.args})
	pid := int64(pidOrZero(x.process.Process))
	cfg := x.executor.effectiveConfig(x.input)
	record := storage.AgentExecutionRecord{
		ID:                 x.executionID,
		ProjectID:          emptyToNil(x.input.ProjectID),
		LoopID:             emptyToNil(x.input.LoopID),
		RunID:              emptyToNil(x.input.RunID),
		Vendor:             string(cfg.Vendor),
		Status:             status,
		PID:                int64PtrIfPositive(pid),
		CommandJSON:        &commandJSON,
		CWD:                &x.input.WorkingDirectory,
		HeartbeatCount:     0,
		LastHeartbeatAt:    &x.startedAtISO,
		NativeSessionID:    emptyToNil(nativeSessionID),
		NativeResumeMode:   emptyToNil(nativeResumeMode),
		NativeResumeStatus: emptyToNil(nativeResumeStatus),
		NativeResumeError:  emptyToNil(nativeResumeError),
		StartedAt:          x.startedAtISO,
		MetadataJSON:       &metadata,
		CreatedAt:          x.startedAtISO,
		UpdatedAt:          x.startedAtISO,
	}
	if heartbeatCount != nil {
		record.HeartbeatCount = *heartbeatCount
		record.UpdatedAt = *heartbeatAt
		record.LastHeartbeatAt = heartbeatAt
	}
	if outputJSON != nil {
		record.OutputJSON = outputJSON
	}
	err := x.upsertAgentExecutionWithRetry(ctx, record)
	if err == nil {
		x.lastPersistedStatus = status
		if heartbeatCount != nil {
			x.lastPersistedHeartbeatCount = *heartbeatCount
			x.lastPersistedHeartbeatAt = *heartbeatAt
		}
		if outputJSON != nil {
			x.lastPersistedOutputJSON = outputJSON
		}
	}
	return err
}

// persistFinal writes the terminal observation after containment is confirmed
// dead. Failures must surface to Wait; callers degrade on hard storage errors.
func (x *execution) persistFinal(status string, result Result, errorMessage, endedAtISO string) error {
	x.persistMu.Lock()
	defer x.persistMu.Unlock()
	if x.executor.repos == nil || x.executor.repos.AgentExecutions == nil {
		x.terminalPersisted = true
		return nil
	}
	nativeSessionID, nativeResumeMode, nativeResumeStatus, nativeResumeError := x.nativeResumeSnapshot()
	commandJSON := mustJSON(map[string]any{"command": x.command, "args": x.args})
	metadata := mustJSON(x.executionMetadata(result.TimeoutType))
	embeddedStdout := x.stdoutString()
	embeddedStderr := x.stderrString()
	if embeddedStderr == "" && result.Stderr != "" {
		embeddedStderr = result.Stderr
	}
	output := x.outputPayload(embeddedStdout, embeddedStderr)
	output["gitPrLifecycle"] = result.Lifecycle
	outputJSON := mustJSON(output)
	pid := int64(pidOrZero(x.process.Process))
	parseStatus := result.ParseStatus
	completionSignal := emptyToNil(result.CompletionSignal)
	if !x.input.Assessment {
		if extractedNativeSessionID := extractNativeSessionID(embeddedStdout, embeddedStderr); extractedNativeSessionID != "" {
			nativeSessionID = extractedNativeSessionID
		}
	} else {
		// Fail closed: assessments never persist a resumable native session id.
		nativeSessionID = ""
	}
	if nativeSessionID != "" && (nativeResumeStatus == "" || nativeResumeStatus == "unavailable") {
		nativeResumeStatus = "captured"
	}
	// Keep the in-memory authority in sync with the terminal record. The result
	// is refreshed by run() after persistFinal returns, including when the
	// session id was discovered only while assembling the terminal payload.
	x.mu.Lock()
	if !x.input.Assessment {
		if nativeSessionID != "" {
			x.nativeSessionID = nativeSessionID
		}
		x.nativeResumeStatus = nativeResumeStatus
		x.nativeResumeError = nativeResumeError
	}
	x.mu.Unlock()
	cfg := x.executor.effectiveConfig(x.input)
	record := storage.AgentExecutionRecord{
		ID:                 x.executionID,
		ProjectID:          emptyToNil(x.input.ProjectID),
		LoopID:             emptyToNil(x.input.LoopID),
		RunID:              emptyToNil(x.input.RunID),
		Vendor:             string(cfg.Vendor),
		Status:             status,
		PID:                int64PtrIfPositive(pid),
		CommandJSON:        &commandJSON,
		CWD:                &x.input.WorkingDirectory,
		Summary:            emptyToNil(result.Summary),
		ParseStatus:        &parseStatus,
		CompletionSignal:   completionSignal,
		HeartbeatCount:     result.HeartbeatCount,
		LastHeartbeatAt:    &x.lastHeartbeatAtISO,
		OutputJSON:         &outputJSON,
		ErrorMessage:       emptyToNil(errorMessage),
		NativeSessionID:    emptyToNil(nativeSessionID),
		NativeResumeMode:   emptyToNil(nativeResumeMode),
		NativeResumeStatus: emptyToNil(nativeResumeStatus),
		NativeResumeError:  emptyToNil(nativeResumeError),
		StartedAt:          x.startedAtISO,
		EndedAt:            &endedAtISO,
		MetadataJSON:       &metadata,
		CreatedAt:          x.startedAtISO,
		UpdatedAt:          endedAtISO,
	}
	err := x.upsertAgentExecutionWithRetry(context.Background(), record)
	// Mark terminal attempted so live writers stop; conflict after another
	// terminal observation also counts as terminal settled for mid-life writes.
	if err == nil || errors.Is(err, storage.ErrAgentExecutionConflict) {
		x.terminalPersisted = true
	}
	// Surface terminal conflicts: a competing terminal with a different status
	// is not durable finalize success (storage contract / #578). Callers fail
	// loud without sticky-degrade (classifyPersistError treats conflict as soft).
	return err
}

// upsertAgentExecutionWithRetry performs one soft retry on SQLITE_BUSY /
// locked storage. Cancel/deadline are not retried.
func (x *execution) upsertAgentExecutionWithRetry(ctx context.Context, record storage.AgentExecutionRecord) error {
	err := x.executor.repos.AgentExecutions.Upsert(ctx, record)
	if err == nil || !isSQLiteBusyPersistError(err) {
		return err
	}
	// Soft/transient: single retry only. Do not sticky-degrade on pure busy.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(25 * time.Millisecond):
	}
	return x.executor.repos.AgentExecutions.Upsert(ctx, record)
}

// classifyPersistError returns a non-nil hard error when infrastructure must
// fail loud and/or degrade. Soft errors (cancel, deadline, conflict after
// terminal won) return nil so callers do not sticky-degrade.
func (x *execution) classifyPersistError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	if errors.Is(err, storage.ErrAgentExecutionConflict) {
		return nil
	}
	return err
}

func isSQLiteBusyPersistError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

func (x *execution) reportHardPersistFailure(err error) {
	if err == nil || x == nil || x.executor == nil {
		return
	}
	x.mu.Lock()
	if x.hardPersistReported {
		x.mu.Unlock()
		return
	}
	x.hardPersistReported = true
	fn := x.executor.onHardPersistFailure
	x.mu.Unlock()
	if fn != nil {
		fn(err)
	}
}

// reapOnOwnershipPersistFailure kills the just-started process after a failed
// initial/fallback ownership observation, joins drain errors into the return,
// and sticky-degrades on hard storage failure. Soft persist errors still fail
// Start/fallback loud without degrade. Callers must keep Supervisor ownership
// when the containment handle is not ConfirmedDead after this returns.
func (x *execution) reapOnOwnershipPersistFailure(cmd *exec.Cmd, grace time.Duration, persistErr error, msg string) error {
	hard := x.classifyPersistError(persistErr)
	var base error
	if hard != nil {
		x.reportHardPersistFailure(hard)
		base = errors.Join(ErrExecutionPersistence, fmt.Errorf("%s: %w", msg, hard))
	} else {
		base = errors.Join(ErrExecutionPersistence, fmt.Errorf("%s: %w", msg, persistErr))
	}
	if grace <= 0 {
		grace = 5 * time.Second
	}
	killCtx, cancel := context.WithTimeout(context.Background(), grace+15*time.Second)
	defer cancel()
	var killErr error
	if x.handle != nil {
		killErr = x.handle.Kill(killCtx)
	} else if cmd != nil {
		killErr = killStartedCmd(cmd)
	}
	if killErr != nil {
		return errors.Join(base, killErr)
	}
	return base
}

// ensureConfirmedDeadBeforeTerminal drains the containment handle when the
// leader has exited but descendants may still be runnable. Terminal status is
// not published until ConfirmedDead for owned handles (#574/#578).
func (x *execution) ensureConfirmedDeadBeforeTerminal() error {
	if x == nil || x.handle == nil {
		return nil
	}
	if x.handle.ConfirmedDead() {
		return nil
	}
	grace := x.gracefulShutdown
	if grace <= 0 {
		grace = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace+15*time.Second)
	defer cancel()
	if err := x.handle.Drain(ctx); err != nil && !x.handle.ConfirmedDead() {
		return err
	}
	if !x.handle.ConfirmedDead() {
		return processcontainment.ErrNotConfirmedDead
	}
	return nil
}

func (x *execution) currentStatus() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.status
}

// liveObservationStatus maps in-memory lifecycle status to a non-terminal
// durable observation for mid-life heartbeat/output writes. Terminal values
// stay in memory for finalStatus/persistFinal after containment is confirmed
// dead; publishing them early would freeze the durable row while the process
// group can still be live.
func (x *execution) liveObservationStatus() string {
	status := x.currentStatus()
	if !storage.IsTerminalAgentExecutionStatus(status) {
		if status == "" {
			return "running"
		}
		return status
	}
	switch status {
	case "timeout", "killed":
		return "cancelling"
	default:
		return "running"
	}
}

func (x *execution) setStatus(status string) {
	x.mu.Lock()
	x.status = status
	x.mu.Unlock()
}

func (x *execution) finalStatus(timedOut, killed bool) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if timedOut {
		x.status = "timeout"
		return x.status
	}
	if killed {
		x.status = "killed"
		return x.status
	}
	if x.process.ProcessState != nil && x.process.ProcessState.ExitCode() == 0 {
		x.status = "completed"
		return x.status
	}
	x.status = "failed"
	return x.status
}

func (x *execution) stdoutString() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return string(x.stdout)
}

func (x *execution) stderrString() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return string(x.stderr)
}

func (x *execution) heartbeatCountValue() int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.heartbeatCount
}

func (x *execution) lastProgressAtISO() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.lastHeartbeatAtISO
}

func (x *execution) nativeResumeSnapshot() (sessionID string, mode string, status string, resumeError string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.nativeSessionID, x.nativeResumeMode, x.nativeResumeStatus, x.nativeResumeError
}

func (x *execution) executionMetadata(timeoutType string) map[string]any {
	metadata := map[string]any{
		"idempotencyKey": emptyToNil(x.input.IdempotencyKey),
		"metadata":       x.input.Metadata,
		"timeoutPolicy": map[string]any{
			"idleTimeoutSeconds": durationSeconds(x.heartbeatTimeout),
			"maxRuntimeSeconds":  durationSeconds(x.timeout),
		},
	}
	if birth := x.processBirthSnapshot(); birth.StartTime > 0 {
		identity := map[string]any{"startTime": birth.StartTime}
		if birth.BootID != "" {
			identity["bootId"] = birth.BootID
		}
		metadata["processIdentity"] = identity
	}
	if timeoutType != "" {
		metadata["timeout"] = map[string]any{
			"type":                         timeoutType,
			"configuredIdleTimeoutSeconds": durationSeconds(x.heartbeatTimeout),
			"configuredMaxRuntimeSeconds":  durationSeconds(x.timeout),
			"elapsedRuntimeSeconds":        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
			"lastProgressAt":               x.lastProgressAtISO(),
		}
	}
	return metadata
}

func (x *execution) captureProcessStart() {
	if x == nil || x.process == nil || x.process.Process == nil {
		return
	}
	birth, err := processidentity.Read(x.process.Process.Pid)
	if err != nil {
		return
	}
	x.mu.Lock()
	x.processBirth = birth
	x.mu.Unlock()
}

func (x *execution) processBirthSnapshot() processidentity.Birth {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.processBirth
}

func durationSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	seconds := int64(duration / time.Second)
	if seconds == 0 {
		return 1
	}
	return seconds
}

func (x *execution) resolveOutputLogs() (string, string) {
	stdout := x.stdoutString()
	stderr := x.stderrString()
	if x.hasPersistedLogWriteFailure() {
		return stdout, stderr
	}
	if persisted, ok := readPersistedExecutionLog(x.stdoutLogPath); ok {
		stdout = persisted
	}
	if persisted, ok := readPersistedExecutionLog(x.stderrLogPath); ok {
		stderr = persisted
	}
	return stdout, stderr
}

func (x *execution) hasPersistedLogWriteFailure() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.persistedLogWriteFailed
}

func (x *execution) markPersistedLogWriteFailed() {
	x.mu.Lock()
	x.persistedLogWriteFailed = true
	x.mu.Unlock()
}

func (x *execution) timeSinceLastOutput() time.Duration {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.lastOutputAt.IsZero() {
		return 0
	}
	return x.executor.now().UTC().Sub(x.lastOutputAt)
}

type streamCapture struct {
	onChunk func([]byte)
}

func (w *streamCapture) Write(p []byte) (int, error) {
	if len(p) > 0 && w.onChunk != nil {
		chunk := make([]byte, len(p))
		copy(chunk, p)
		w.onChunk(chunk)
	}
	return len(p), nil
}

func ResolveSpawn(cfg ExecutorConfig, workingDirectory string, prompt string) (string, []string) {
	command := resolveCommand(cfg)
	args := resolveArgs(cfg, workingDirectory, prompt)
	return command, args
}

func ResolveSpawnWithNativeResume(cfg ExecutorConfig, workingDirectory string, prompt string, sessionID string, enabled bool) (string, []string) {
	if !enabled || strings.TrimSpace(sessionID) == "" || !nativeResumeSupported(cfg.Vendor) {
		return ResolveSpawn(cfg, workingDirectory, prompt)
	}
	command := resolveCommand(cfg)
	args := resolveNativeResumeArgs(cfg, workingDirectory, stringArgs(cfg.Params["args"]), strings.TrimSpace(sessionID), prompt)
	return command, args
}

// InteractiveTakeoverSupported reports whether a human can take over a loop's
// agent session INTERACTIVELY for the given vendor. Distinct from
// nativeResumeSupported (the daemon's headless resume): only vendors whose
// interactive resume is verified to preserve the session id AND accumulate
// history — so the daemon's later native-resume sees the human's turns — are
// enabled. Verified 2026-07: codex (`codex resume <id>`) and claude-code
// (`claude --resume <id>`) both keep the id and thread the conversation.
// opencode/cursor stay disabled until the same 3-turn check passes for them.
func InteractiveTakeoverSupported(vendor config.AgentVendor) bool {
	adapter, ok := runtimeAdapterFor(vendor)
	return ok && adapter.contract.Supports(CapabilityInteractiveTakeover) && adapter.resolveInteractiveResume != nil
}

// InteractiveResumeCommandLine renders the shell command a human runs to take
// over a loop's agent session interactively: the SAME native session id the
// daemon was driving, in the loop's worktree. Because a resume preserves the id
// and appends to the same conversation, the daemon's later native-resume then
// sees everything the human did. Returns ("", false) when takeover isn't
// supported for the vendor or the session id is missing.
func InteractiveResumeCommandLine(cfg ExecutorConfig, workingDirectory, sessionID string) (string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	workingDirectory = strings.TrimSpace(workingDirectory)
	if sessionID == "" || !InteractiveTakeoverSupported(cfg.Vendor) {
		return "", false
	}
	adapter, ok := runtimeAdapterFor(cfg.Vendor)
	if !ok || adapter.resolveInteractiveResume == nil {
		return "", false
	}
	resume := adapter.resolveInteractiveResume(resolveCommand(cfg), cfg, sessionID)
	if workingDirectory != "" {
		return "cd " + shellSingleQuote(workingDirectory) + " && " + resume, true
	}
	return resume, true
}

// shellSingleQuote makes a string safe to paste into a POSIX shell. UUIDs and
// plain paths pass through unquoted; anything with shell-special characters is
// single-quoted with embedded quotes escaped.
func shellSingleQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`&|;<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func resolveCommand(cfg ExecutorConfig) string {
	if override, ok := cfg.Params["command"].(string); ok && strings.TrimSpace(override) != "" {
		return override
	}
	if adapter, ok := runtimeAdapterFor(cfg.Vendor); ok && adapter.contract.DefaultCommand != "" {
		return adapter.contract.DefaultCommand
	}
	return string(cfg.Vendor)
}

func resolveArgs(cfg ExecutorConfig, workingDirectory string, prompt string) []string {
	resolvedArgs := stringArgs(cfg.Params["args"])
	if adapter, ok := runtimeAdapterFor(cfg.Vendor); ok && adapter.resolveStartArgs != nil {
		return adapter.resolveStartArgs(cfg, resolvedArgs, workingDirectory, prompt)
	}
	return append([]string{}, resolvedArgs...)
}

func resolveClaudeArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
		resolved = append(resolved, "--print", prompt)
	}
	if !hasAnyFlag(resolved, []string{"--dangerously-skip-permissions"}) {
		resolved = append(resolved, "--dangerously-skip-permissions")
	}
	return resolved
}

func resolveCodexArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := append([]string{}, args...)
	if !containsArg(resolved, "exec") {
		resolved = append([]string{"exec"}, resolved...)
	}
	if cfg.LiveToolEvents && !containsArg(resolved, "--json") {
		resolved = append(resolved, "--json")
	}
	// `codex exec` picks its own sandbox regardless of the sandbox setting in
	// the user's codex config, and its default denies network access. Every
	// role's prompt tells the agent to verify PR state with `gh` and to fail
	// fast if it cannot reach the forge, so a sandbox without networking does
	// not degrade the run — it stops it before any edit, and the runner then
	// reports the agent's empty output as a contract violation and retries
	// forever against a sandbox that will never change.
	//
	// Set the mode explicitly and grant only networking: writes stay confined
	// to the worktree and the temporary directories.
	resolved = appendCodexSandboxDefaults(resolved)
	withModel := prependModelFlag(resolved, cfg.Model, "--model", []string{"--model", "-m"})
	withReasoning := appendReasoningEffortFlag(withModel, cfg.ReasoningEffort)
	if hasAnyFlag(withReasoning, []string{"-"}) {
		return withReasoning
	}
	return append(withReasoning, prompt)
}

// appendReasoningEffortFlag appends `-c model_reasoning_effort=<value>` when the
// operator configured a Codex-supported effort level. "none" is a Looper
// overlay sentinel that suppresses an inherited setting; it is not a value
// accepted by the Codex CLI, so it must never be forwarded as a literal.
func appendReasoningEffortFlag(args []string, effort *config.ReasoningEffort) []string {
	if effort == nil {
		return args
	}
	if *effort == config.ReasoningEffortNone {
		return stripReasoningEffortFlags(args)
	}
	flag := []string{"-c", fmt.Sprintf("model_reasoning_effort=%s", string(*effort))}
	for i, arg := range args {
		if arg == "-" {
			withFlag := make([]string, 0, len(args)+len(flag))
			withFlag = append(withFlag, args[:i]...)
			withFlag = append(withFlag, flag...)
			return append(withFlag, args[i:]...)
		}
	}
	return append(args, flag...)
}

// stripReasoningEffortFlags removes inherited Codex config assignments when
// the Looper overlay explicitly suppresses reasoning effort. It accepts the
// separate and equals forms supported by Codex without treating unrelated -c
// settings as identity-bearing.
func stripReasoningEffortFlags(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "--config" {
			if i+1 < len(args) && isReasoningEffortConfig(args[i+1]) {
				i++
				continue
			}
			out = append(out, arg)
			continue
		}
		if strings.HasPrefix(arg, "-c=") && isReasoningEffortConfig(strings.TrimPrefix(arg, "-c=")) {
			continue
		}
		if strings.HasPrefix(arg, "--config=") && isReasoningEffortConfig(strings.TrimPrefix(arg, "--config=")) {
			continue
		}
		if strings.HasPrefix(arg, "-c") && isReasoningEffortConfig(strings.TrimPrefix(arg, "-c")) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func isReasoningEffortConfig(arg string) bool {
	return strings.HasPrefix(strings.TrimSpace(arg), "model_reasoning_effort=")
}

// enforceCodexToolNetworkDenied overrides all operator-supplied Codex sandbox
// choices for a validation-gated run. Codex's model transport remains outside
// the tool sandbox, while shell commands cannot reach the network or write
// outside the worktree. Removing the bypass flag is essential: appending a
// safer flag after it would not restore containment.
func enforceCodexToolNetworkDenied(args []string, prompt string, sandbox *validationcmd.Sandbox) []string {
	filtered := make([]string, 0, len(args)+4)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dangerously-bypass-approvals-and-sandbox" || arg == "--dangerously-bypass-hook-trust":
			continue
		case arg == "--search" || strings.HasPrefix(arg, "--search="):
			continue
		case arg == "--add-dir" || arg == "-C" || arg == "--cd" || arg == "-p" || arg == "--profile" || arg == "--enable":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "--add-dir=") || strings.HasPrefix(arg, "-C=") || strings.HasPrefix(arg, "--cd=") || strings.HasPrefix(arg, "--profile=") || strings.HasPrefix(arg, "--enable="):
			continue
		case (strings.HasPrefix(arg, "-C") || strings.HasPrefix(arg, "-p")) && len(arg) > 2:
			continue
		case arg == "-s" || arg == "--sandbox":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "-s=") || strings.HasPrefix(arg, "--sandbox="):
			continue
		case strings.HasPrefix(arg, "-s") && len(arg) > 2:
			continue
		case arg == "-c" || arg == "--config":
			if i+1 < len(args) && unsafeCodexSandboxConfig(args[i+1]) {
				i++
				continue
			}
		case (strings.HasPrefix(arg, "-c=") || strings.HasPrefix(arg, "--config=")) && unsafeCodexSandboxConfig(arg):
			continue
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			continue
		}
		filtered = append(filtered, arg)
	}

	trailingPrompt := len(filtered) > 0 && filtered[len(filtered)-1] == prompt
	if trailingPrompt {
		filtered = filtered[:len(filtered)-1]
	}
	restrictions := []string{
		"--ignore-user-config",
		"--disable", "browser_use",
		"--disable", "browser_use_external",
		"--disable", "browser_use_full_cdp_access",
		"--disable", "in_app_browser",
		"--disable", "standalone_web_search",
	}
	if sandbox != nil {
		restrictions = append(restrictions,
			"-c", sandbox.PermissionConfig(),
			"-c", "permission_profile="+strconv.Quote(sandbox.ProfileName),
			"-c", sandbox.ShellEnvironmentConfig(),
		)
	} else {
		// Unit callers without a prepared sandbox still fail closed on network.
		restrictions = append(restrictions, "-s", "workspace-write", "-c", "sandbox_workspace_write.network_access=false")
	}
	insertAt := len(filtered)
	for i, arg := range filtered {
		if arg == "resume" {
			insertAt = i
			break
		}
	}
	filtered = append(filtered[:insertAt], append(restrictions, filtered[insertAt:]...)...)
	if trailingPrompt {
		filtered = append(filtered, prompt)
	}
	return filtered
}

// validateAssessmentExecution keeps the pre-authorization capability boundary
// independent of normal coding-agent configuration. The native Codex profile
// and its allowlisted tool environment are the authority here, not a prompt or
// a post-run cleanliness check.
func validateAssessmentExecution(cfg ExecutorConfig, input RunInput) error {
	if cfg.Vendor != config.AgentVendorCodex {
		return fmt.Errorf("assessment profile is supported only for codex; refusing %s execution", cfg.Vendor)
	}
	if input.RestrictToolNetwork {
		return fmt.Errorf("assessment profile owns its sandbox policy; RestrictToolNetwork must be false")
	}
	if strings.TrimSpace(input.NativeSessionID) != "" {
		return fmt.Errorf("assessment profile does not permit native resume")
	}
	if command, ok := cfg.Params["command"].(string); ok && strings.TrimSpace(command) != "" {
		return fmt.Errorf("assessment profile rejects configured command wrappers")
	}
	if rawArgs, ok := cfg.Params["args"]; ok && len(stringArgs(rawArgs)) > 0 {
		return fmt.Errorf("assessment profile rejects configured argv overrides")
	}
	return nil
}

// resolveAssessmentSpawn deliberately does not reuse ResolveSpawn: that path
// preserves normal operator args and grants a writable Codex workspace. An
// assessment may inspect the repository, but only its disposable tool root is
// writable and it receives no browser, Apps/MCP, computer-control, search,
// network, or project-document capability.
func resolveAssessmentSpawn(cfg ExecutorConfig, prompt string, sandbox *validationcmd.Sandbox) (string, []string) {
	args := []string{"exec"}
	if cfg.LiveToolEvents {
		args = append(args, "--json")
	}
	args = prependModelFlag(args, cfg.Model, "--model", []string{"--model", "-m"})
	args = append(args,
		"--ignore-user-config",
		"-c", "project_doc_max_bytes=0",
		"--disable", "apps",
		"--disable", "browser_use",
		"--disable", "browser_use_external",
		"--disable", "browser_use_full_cdp_access",
		"--disable", "computer_use",
		"--disable", "in_app_browser",
		"--disable", "standalone_web_search",
	)
	if sandbox != nil {
		args = append(args,
			"-c", sandbox.PermissionConfig(),
			"-c", "permission_profile="+strconv.Quote(sandbox.ProfileName),
			"-c", sandbox.ShellEnvironmentConfig(),
		)
	}
	return "codex", append(args, prompt)
}

func unsafeCodexSandboxConfig(value string) bool {
	for _, key := range []string{"sandbox_workspace_write", "sandbox_permissions", "sandbox_mode", "permission_profile", "permissions.", "shell_environment_policy", "approval_policy", "mcp_servers", "web_search", "features.browser_use", "features.in_app_browser", "features.standalone_web_search"} {
		if strings.Contains(value, key) {
			return true
		}
	}
	return false
}

// appendCodexSandboxDefaults sets the sandbox mode and grants networking
// unless the operator already spoke about either.
func appendCodexSandboxDefaults(args []string) []string {
	if !hasAnyFlag(args, []string{"-s", "--sandbox", "--dangerously-bypass-approvals-and-sandbox"}) {
		args = append(args, "-s", "workspace-write")
	}
	if !codexConfiguresNetworkAccess(args) {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	return args
}

// codexConfiguresNetworkAccess reports whether the operator already said
// something about sandbox networking through agent.params args, in which case
// their choice — including deliberately leaving it off — wins.
func codexConfiguresNetworkAccess(args []string) bool {
	for i, arg := range args {
		if arg != "-c" && arg != "--config" {
			if strings.Contains(arg, "network_access") {
				return true
			}
			continue
		}
		if i+1 < len(args) && strings.Contains(args[i+1], "network_access") {
			return true
		}
	}
	return false
}

func resolveOpenCodeArgs(cfg ExecutorConfig, args []string, workingDirectory string, prompt string) []string {
	resolved := append([]string{}, args...)
	if !containsArg(resolved, "run") {
		resolved = append([]string{"run"}, resolved...)
	}
	if strings.TrimSpace(workingDirectory) != "" && !hasAnyFlag(resolved, []string{"--dir"}) {
		resolved = appendDirFlag(resolved, workingDirectory)
	}
	withModel := prependModelFlag(resolved, cfg.Model, "--model", []string{"--model", "-m"})
	if hasAnyFlag(withModel, []string{"-p", "--prompt", "-f", "--file"}) {
		return withModel
	}
	return append(withModel, prompt)
}

func resolveCursorArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if hasAnyFlag(resolved, []string{"-p", "--print"}) {
		return resolved
	}
	return append(resolved, "--print", prompt)
}

func resolveGrokArgs(cfg ExecutorConfig, args []string, workingDirectory string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"-m", "--model"})
	if !hasAnyFlag(resolved, []string{"-p", "--single"}) {
		resolved = append(resolved, "-p", prompt)
	}
	if strings.TrimSpace(workingDirectory) != "" && !hasAnyFlag(resolved, []string{"--cwd"}) {
		resolved = append(resolved, "--cwd", workingDirectory)
	}
	if !hasAnyFlag(resolved, []string{"--output-format"}) {
		resolved = append(resolved, "--output-format", "plain")
	}
	if !hasAnyFlag(resolved, []string{"--always-approve", "--yolo", "--dangerously-skip-permissions", "--permission-mode"}) {
		resolved = append(resolved, "--always-approve")
	}
	if !hasAnyFlag(resolved, []string{"--sandbox"}) {
		resolved = append(resolved, "--sandbox", "off")
	}
	if !hasAnyFlag(resolved, []string{"--no-auto-update"}) {
		resolved = append(resolved, "--no-auto-update")
	}
	return resolved
}

func resolveDevinArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if !hasAnyFlag(resolved, []string{"--permission-mode"}) {
		resolved = append(resolved, "--permission-mode", "dangerous")
	}
	if !hasAnyFlag(resolved, []string{"--respect-workspace-trust"}) {
		resolved = append(resolved, "--respect-workspace-trust", "false")
	}
	if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
		resolved = append(resolved, "--print")
	}
	return append(resolved, prompt)
}

// enforceToolNetworkDenied applies the resolved vendor's tool-network denial.
// A vendor without an implementation is refused rather than run unrestricted.
func enforceToolNetworkDenied(vendor config.AgentVendor, args []string, prompt string, sandbox *validationcmd.Sandbox) ([]string, error) {
	adapter, ok := runtimeAdapterFor(vendor)
	if !ok || adapter.enforceToolNetworkDenied == nil {
		return nil, unsupportedToolNetworkDenialError(vendor)
	}
	return adapter.enforceToolNetworkDenied(args, prompt, sandbox)
}

// unsupportedToolNetworkDenialError is a static configuration mismatch, not a
// transient one: retrying the same vendor can only fail the same way.
func unsupportedToolNetworkDenialError(vendor config.AgentVendor) error {
	supported := make([]string, 0, 4)
	for _, candidate := range ToolNetworkDenialVendors() {
		supported = append(supported, string(candidate))
	}
	return fmt.Errorf(
		"agent vendor %q cannot deny tool network access; validation-gated execution requires one of: %s. Switch roles.worker.agent.vendor / roles.fixer.agent.vendor, or set the project's validation.optOut=true: %w",
		vendor, strings.Join(supported, ", "), failureclass.ErrStaticConfigMismatch,
	)
}

// resolveHermesArgs makes the scheduler-owned prompt the only one-shot query.
func resolveHermesArgs(cfg ExecutorConfig, args []string, prompt string) []string {
	resolved := removeFlagsWithValues(args, []string{"-z", "--zen", "-q", "--query"})
	resolved = prependModelFlag(resolved, cfg.Model, "--model", []string{"--model", "-m"})
	if !hasAnyFlag(resolved, []string{"--yolo"}) {
		resolved = append(resolved, "--yolo")
	}
	return append(resolved, "-z", prompt)
}

func removeFlagsWithValues(args []string, flags []string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		remove := false
		for _, flag := range flags {
			if arg == flag {
				remove = true
				if index+1 < len(args) {
					index++
				}
				break
			}
			if strings.HasPrefix(arg, flag+"=") {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, arg)
		}
	}
	return result
}

func resolveNativeResumeArgs(cfg ExecutorConfig, workingDirectory string, args []string, sessionID string, prompt string) []string {
	if adapter, ok := runtimeAdapterFor(cfg.Vendor); ok && adapter.resolveNativeResumeArgs != nil {
		return adapter.resolveNativeResumeArgs(cfg, args, workingDirectory, sessionID, prompt)
	}
	return append([]string{}, args...)
}

func buildCommandEnv(workingDirectory string, prompt string, envSources ...map[string]string) []string {
	inherited := envSliceToMap(os.Environ())
	envMap := make(map[string]string, len(inheritedAgentEnvKeys))
	for _, key := range inheritedAgentEnvKeys {
		if value, ok := inherited[key]; ok {
			envMap[key] = value
		}
	}
	for key, value := range inherited {
		if strings.HasPrefix(key, "LC_") {
			envMap[key] = value
		}
	}
	for _, source := range envSources {
		maps.Copy(envMap, source)
	}
	for _, key := range unsafeAgentEnvKeys {
		delete(envMap, key)
	}
	if strings.TrimSpace(workingDirectory) != "" {
		envMap["PWD"] = workingDirectory
	}
	envMap["LOOPER_PROMPT"] = prompt
	envMap[completionMarkerEnv] = CompletionMarkerPrefix
	return envMapToSlice(envMap)
}

func BuildCommandEnv(workingDirectory string, prompt string, envSources ...map[string]string) []string {
	return buildCommandEnv(workingDirectory, prompt, envSources...)
}

func BuildCommandEnvMap(workingDirectory string, prompt string, envSources ...map[string]string) map[string]string {
	return envSliceToMap(buildCommandEnv(workingDirectory, prompt, envSources...))
}

func envSliceToMap(env []string) map[string]string {
	envMap := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		envMap[key] = value
	}
	return envMap
}

func envMapToSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	resolved := make([]string, 0, len(keys))
	for _, key := range keys {
		resolved = append(resolved, key+"="+env[key])
	}
	return resolved
}

func appendDirFlag(args []string, workingDirectory string) []string {
	for idx, arg := range args {
		if arg != "run" {
			continue
		}
		resolved := append([]string{}, args[:idx+1]...)
		resolved = append(resolved, "--dir", workingDirectory)
		return append(resolved, args[idx+1:]...)
	}
	return append([]string{"--dir", workingDirectory}, args...)
}

func prependModelFlag(args []string, model *string, flag string, recognizedFlags []string) []string {
	if model == nil || *model == "" || hasAnyFlag(args, recognizedFlags) {
		return append([]string{}, args...)
	}
	if len(args) > 0 && (args[0] == "exec" || args[0] == "run") {
		return append([]string{args[0], flag, *model}, args[1:]...)
	}
	return append([]string{flag, *model}, args...)
}

func hasAnyFlag(args []string, flags []string) bool {
	for _, flag := range flags {
		for _, arg := range args {
			if arg == flag || (strings.HasPrefix(flag, "--") && strings.HasPrefix(arg, flag+"=")) {
				return true
			}
		}
	}
	return false
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func removeFirstArg(args []string, target string) []string {
	resolved := make([]string, 0, len(args))
	removed := false
	for _, arg := range args {
		if !removed && arg == target {
			removed = true
			continue
		}
		resolved = append(resolved, arg)
	}
	return resolved
}

func stringArgs(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if stringsValue, okStrings := value.([]string); okStrings {
			return append([]string{}, stringsValue...)
		}
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

// ansiEscapePattern matches ANSI CSI escape sequences (e.g. "\x1b[1m", "\x1b[0m").
// Codex prints its session id as a styled human line rather than JSON, so escape
// codes must be stripped before the id can be parsed out.
var ansiEscapePattern = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

func stripANSIEscapes(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiEscapePattern.ReplaceAllString(s, "")
}

func extractNativeSessionID(outputs ...string) string {
	// "session id" (with a space) matches Codex's human-readable line
	// "\x1b[1msession id:\x1b[0m <uuid>"; the others match JSON-emitting vendors.
	keys := []string{"nativeSessionId", "native_session_id", "sessionId", "session_id", "session id", "chatId", "chat_id"}
	for _, output := range outputs {
		for _, line := range strings.Split(output, "\n") {
			// Strip ANSI styling first: Codex wraps the session id in escape codes,
			// and a JSON line without styling passes through unchanged.
			line = strings.TrimSpace(stripANSIEscapes(line))
			if line == "" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(line), &payload); err == nil {
				for _, key := range keys {
					if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
						return strings.TrimSpace(value)
					}
				}
			}
			for _, key := range keys {
				if value := extractKeyValue(line, key); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func extractKeyValue(line string, key string) string {
	lowerLine := strings.ToLower(line)
	lowerKey := strings.ToLower(key)
	idx := strings.Index(lowerLine, lowerKey)
	if idx < 0 {
		return ""
	}
	if idx > 0 {
		prev := lowerLine[idx-1]
		if isSessionKeyChar(prev) {
			return ""
		}
	}
	after := idx + len(key)
	if after < len(lowerLine) && isSessionKeyChar(lowerLine[after]) {
		return ""
	}
	rest := strings.TrimLeft(line[after:], " \t")
	if strings.HasPrefix(rest, "\"") || strings.HasPrefix(rest, "'") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	if rest == "" || (rest[0] != ':' && rest[0] != '=') {
		return ""
	}
	rest = strings.TrimLeft(rest[1:], " \t\"'")
	if rest == "" {
		return ""
	}
	end := len(rest)
	for i, r := range rest {
		if r == ' ' || r == '\t' || r == '\'' || r == '"' || r == ',' || r == '}' {
			end = i
			break
		}
	}
	return strings.Trim(strings.TrimSpace(rest[:end]), "'\"")
}

func isSessionKeyChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
}

func (e *ConfiguredExecutor) executionLogPaths(input RunInput, executionID string) (string, string) {
	if strings.TrimSpace(e.logDir) == "" {
		return "", ""
	}
	runID := safeLogPathSegment(firstNonEmpty(input.RunID, "runless"))
	loopID := safeLogPathSegment(firstNonEmpty(input.LoopID, "loopless"))
	execID := safeLogPathSegment(firstNonEmpty(executionID, "execution"))
	dir := filepath.Join(e.logDir, "loops", loopID, runID)
	return filepath.Join(dir, execID+".stdout.log"), filepath.Join(dir, execID+".stderr.log")
}

func safeLogPathSegment(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return "unknown"
	}
	var builder strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	segment := builder.String()
	if segment == "." || segment == ".." {
		return "unknown"
	}
	return segment
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (x *execution) appendPersistedLog(path string, chunk []byte) bool {
	if path == "" || len(chunk) == 0 {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return true
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return true
	}
	n, err := file.Write(chunk)
	writeFailed := err != nil || n != len(chunk)
	if err := file.Close(); err != nil {
		writeFailed = true
	}
	return writeFailed
}

func (x *execution) initializePersistedLogs() {
	for _, path := range []string{x.stdoutLogPath, x.stderrLogPath} {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			continue
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_ = file.Close()
	}
}

func readPersistedExecutionLog(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	if info.Size() > maxPersistedLogReadBytes {
		if _, err := file.Seek(info.Size()-maxPersistedLogReadBytes, io.SeekStart); err != nil {
			return "", false
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxPersistedLogReadBytes))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func (x *execution) outputJSON(stdout, stderr string) string {
	return mustJSON(x.outputPayload(stdout, stderr))
}

func (x *execution) outputPayload(stdout, stderr string) map[string]any {
	payload := map[string]any{"stdout": stdout, "stderr": stderr}
	if x.stdoutLogPath != "" {
		payload["stdoutLogPath"] = x.stdoutLogPath
	}
	if x.stderrLogPath != "" {
		payload["stderrLogPath"] = x.stderrLogPath
	}
	return payload
}

func appendTailBounded(existing []byte, chunk []byte, maxBytes int) []byte {
	combined := append(existing, chunk...)
	if len(combined) <= maxBytes {
		return combined
	}
	return append([]byte{}, combined[len(combined)-maxBytes:]...)
}

func tickerChan(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}

func parseCompletion(stdout, stderr string) completionParse {
	raw := stdout + "\n" + stderr
	for _, payload := range CompletionMarkerPayloads(raw) {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			return completionParse{ParseStatus: "invalid_json", CompletionSignal: CompletionMarkerPrefix}
		}
		result := completionParse{
			ParseStatus:       "parsed",
			CompletionSignal:  CompletionMarkerPrefix,
			CompletionPayload: payload,
			Artifacts:         asStringSlice(parsed["artifacts"]),
			ChangedFiles:      asStringSlice(parsed["changedFiles"]),
			Commits:           asStringSlice(parsed["commits"]),
		}
		if state, err := lifecycle.FromMap(parsed["git_pr_lifecycle"]); err == nil {
			result.Lifecycle = state
		}
		if summary, ok := parsed["summary"].(string); ok {
			result.Summary = summary
		}
		if isTemplateCompletion(result) {
			continue
		}
		return result
	}
	return completionParse{ParseStatus: "missing"}
}

// isTemplateCompletion rejects an echoed completion template. Every completion
// template — the generic summary-only shape and the fixer's outcome/failure_kind
// shapes — emits the literal "<one-sentence summary>" placeholder, and a real
// agent never leaves the summary as that exact token. Keying on the placeholder
// alone covers the fixer templates, which carry extra keys alongside the summary
// and so slip past a single-key shape check.
func isTemplateCompletion(result completionParse) bool {
	return strings.TrimSpace(result.Summary) == "<one-sentence summary>"
}

func IsAgentSetupFailureMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isCodexVersionSetupFailure(line) || isAgentModelSetupFailure(line) {
			return true
		}
	}
	return false
}

func isCodexVersionSetupFailure(line string) bool {
	return strings.Contains(line, " model requires a newer version of codex") &&
		strings.Contains(line, "please upgrade to the latest app or cli")
}

func isAgentModelSetupFailure(line string) bool {
	modelFailurePhrases := []string{
		"unsupported model",
		"unknown model",
		"invalid model",
		"model is not supported",
		"unrecognized model",
	}
	if !containsAny(line, modelFailurePhrases) {
		return false
	}
	return containsAny(line, []string{"agent setup", "agent configuration", "configured model", "model configuration", "--model", "model:"}) &&
		containsAny(line, []string{"codex", "claude", "opencode", "cursor"})
}

func containsAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func asStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if stringsValue, okStrings := value.([]string); okStrings {
			return append([]string(nil), stringsValue...)
		}
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func summarizeLogs(stdout, stderr string) string {
	combined := strings.TrimSpace(stdout + "\n" + stderr)
	if combined == "" {
		return ""
	}
	parts := strings.Split(combined, "\n")
	for i := len(parts) - 1; i >= 0; i-- {
		line := strings.TrimSpace(parts[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func (e *ConfiguredExecutor) appendLifecycleEvent(eventType string, input RunInput, executionID string, payload any, createdAt string) {
	if e.repos == nil || e.repos.Events == nil {
		return
	}
	vendor := string(e.effectiveConfig(input).Vendor)
	_ = e.repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID:               eventlog.NewEventID("event"),
		EventType:        eventType,
		ProjectID:        emptyToNil(input.ProjectID),
		LoopID:           emptyToNil(input.LoopID),
		RunID:            emptyToNil(input.RunID),
		EntityType:       stringPtr("agent_execution"),
		EntityID:         &executionID,
		ActorType:        stringPtr("agent"),
		ActorID:          stringPtr(vendor),
		ActorDisplayName: stringPtr(vendor),
		PayloadJSON:      mustJSON(payload),
		CreatedAt:        createdAt,
	})
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func emptyToNil(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	v := value
	return &v
}

func int64PtrIfPositive(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	v := value
	return &v
}

func stringPtr(value string) *string {
	return &value
}

func (x *execution) leaderPID() int {
	if x == nil {
		return 0
	}
	if x.handle != nil {
		return x.handle.PID()
	}
	if x.process != nil {
		return pidOrZero(x.process.Process)
	}
	return 0
}

func pidOrZero(process *os.Process) int {
	if process == nil {
		return 0
	}
	return process.Pid
}
