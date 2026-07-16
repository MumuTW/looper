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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/forge"
	shellinfra "github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	defaultMaxOutputBytes       = 256 * 1024
	maxPersistedLogReadBytes    = 16 * 1024 * 1024
	completionMarkerEnv         = "LOOPER_COMPLETION_MARKER"
	descendantPipeDrainGrace    = 100 * time.Millisecond
	outputQueueCapacity         = 512
	outputPersistenceTimeout    = 250 * time.Millisecond
	ownershipPersistenceTimeout = 6 * time.Second
	processGroupExitTimeout     = time.Second
	processGroupProbeInterval   = 5 * time.Millisecond
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
	// Config path selector for trusted wrappers (looper review submit, etc.)
	// that load via LOOPER_CONFIG when --config is not passed.
	"LOOPER_CONFIG",
	// Capability socket for daemon-side review submit (not a secret).
	forge.TrustedReviewSockEnv,
}

type ExecutorConfig struct {
	Vendor              config.AgentVendor
	Model               *string
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
	// OnProgress, when set, is called (throttled) while an agent run streams
	// output, so a transport can surface live progress. Vendor-agnostic: it works
	// off the subprocess's stdout tail, whatever agent (codex/opencode/claude) runs.
	OnProgress func(context.Context, ProgressUpdate)
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
	Metadata           map[string]any
	IdempotencyKey     string
	Env                map[string]string
	NativeSessionID    string
}

type Result struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	CompletionSignal             string
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
	PID                          int
}

type completionParse struct {
	ParseStatus      string
	CompletionSignal string
	Summary          string
	Artifacts        []string
	ChangedFiles     []string
	Commits          []string
	Lifecycle        *lifecycle.State
}

type Execution interface {
	Wait(context.Context) (Result, error)
	Kill(string) error
}

type ConfiguredExecutor struct {
	config                ExecutorConfig
	repos                 *storage.Repositories
	logDir                string
	now                   func() time.Time
	onProgress            func(context.Context, ProgressUpdate)
	appendPersistedLog    func(string, []byte) bool
	appendLifecycleRecord func(context.Context, storage.EventLogRecord) error
}

func New(options ExecutorOptions) *ConfiguredExecutor {
	rawNow := options.Now
	if rawNow == nil {
		rawNow = time.Now
	}
	var nowMu sync.Mutex
	now := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return rawNow()
	}
	var appendLifecycleRecord func(context.Context, storage.EventLogRecord) error
	if options.Repos != nil && options.Repos.Events != nil {
		appendLifecycleRecord = options.Repos.Events.Append
	}
	return &ConfiguredExecutor{
		config:                options.Config,
		repos:                 options.Repos,
		logDir:                options.LogDir,
		now:                   now,
		onProgress:            options.OnProgress,
		appendPersistedLog:    appendPersistedLogFile,
		appendLifecycleRecord: appendLifecycleRecord,
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

func (e *ConfiguredExecutor) resolveNativeResume(ctx context.Context, input RunInput) (nativeResumeInfo, error) {
	if !e.config.NativeResumeEnabled {
		return nativeResumeInfo{Mode: "checkpoint_restart", Status: "disabled"}, nil
	}
	if sessionID := strings.TrimSpace(input.NativeSessionID); sessionID != "" {
		if nativeResumeSupported(e.config.Vendor) {
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
	if latest.Vendor != string(e.config.Vendor) || !nativeResumeSupported(e.config.Vendor) || !isRecoverableNativeResumeSource(latest.Status, latest.NativeResumeStatus) {
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
	switch vendor {
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI:
		return true
	default:
		return false
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	startedAt := e.now().UTC()
	startedAtISO := eventlog.FormatJavaScriptISOString(startedAt)
	resume, err := e.resolveNativeResume(ctx, input)
	if err != nil {
		return nil, err
	}
	spawnPrompt := input.Prompt
	if resume.Enabled && strings.TrimSpace(input.NativeResumePrompt) != "" {
		spawnPrompt = input.NativeResumePrompt
	}
	command, args := ResolveSpawnWithNativeResume(e.config, input.WorkingDirectory, spawnPrompt, resume.SessionID, resume.Enabled)

	cmd := exec.Command(command, args...)
	cmd.Dir = input.WorkingDirectory
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = descendantPipeDrainGrace
	cmd.Env = buildCommandEnv(input.WorkingDirectory, spawnPrompt, e.config.Env, input.Env)

	maxOutputBytes := input.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	progressCtx, progressCancel := context.WithCancel(ctx)
	outputCtx, outputCancel := context.WithCancel(context.Background())
	outputPersistenceCtx, outputPersistenceCancel := context.WithCancel(context.Background())
	ownershipTransferred := false
	defer func() {
		if !ownershipTransferred {
			progressCancel()
		}
	}()

	x := &execution{
		executor:                e,
		input:                   input,
		executionID:             executionID,
		startedAt:               startedAt,
		command:                 command,
		args:                    args,
		startedAtISO:            startedAtISO,
		process:                 cmd,
		timeout:                 input.Timeout,
		heartbeatTimeout:        input.HeartbeatTimeout,
		gracefulShutdown:        input.GracefulShutdown,
		maxOutputBytes:          maxOutputBytes,
		lastHeartbeatAtISO:      startedAtISO,
		lastOutputAt:            startedAt,
		status:                  "running",
		nativeSessionID:         resume.SessionID,
		nativeResumeMode:        resume.Mode,
		nativeResumeStatus:      resume.Status,
		progressCtx:             progressCtx,
		progressCancel:          progressCancel,
		outputCtx:               outputCtx,
		outputCancel:            outputCancel,
		outputCh:                make(chan outputMessage, outputQueueCapacity),
		outputDone:              make(chan struct{}),
		outputPersistenceCtx:    outputPersistenceCtx,
		outputPersistenceCancel: outputPersistenceCancel,
		outputPersistenceCh:     make(chan outputPersistence, 1),
		outputPersistenceDone:   make(chan struct{}),
		killCh:                  make(chan string, 1),
		doneCh:                  make(chan execOutcome, 1),
	}
	go x.processOutput()
	go x.processOutputPersistence()
	defer func() {
		if !ownershipTransferred {
			x.stopOutput()
			x.stopOutputPersistence()
		}
	}()
	if input.Timeout > 0 {
		x.maxRuntimeDeadline = time.Now().Add(input.Timeout)
	}
	x.stdoutLogPath, x.stderrLogPath = e.executionLogPaths(input, executionID)
	x.initializePersistedLogs()
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.enqueueOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.enqueueOutput("stderr", chunk) }}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		if resume.Enabled {
			if markErr := e.markNativeResumeFailed(ctx, resume.SourceExecutionID, err.Error()); markErr == nil && e.logDir != "" {
				// best-effort marker only; command fallback is the important recovery behavior
			}
			command, args = ResolveSpawn(e.config, input.WorkingDirectory, input.Prompt)
			cmd = exec.Command(command, args...)
			cmd.Dir = input.WorkingDirectory
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.WaitDelay = descendantPipeDrainGrace
			cmd.Env = buildCommandEnv(input.WorkingDirectory, input.Prompt, e.config.Env, input.Env)
			cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.enqueueOutput("stdout", chunk) }}
			cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.enqueueOutput("stderr", chunk) }}
			x.mu.Lock()
			x.command = command
			x.args = args
			x.process = cmd
			x.processGroupResolved = false
			x.processGroupKillSent = false
			x.processGroupSignalsDone = false
			x.nativeSessionID = ""
			x.nativeResumeMode = "checkpoint_restart"
			x.nativeResumeStatus = "fallback_started"
			x.nativeResumeError = err.Error()
			x.mu.Unlock()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start agent command: %w (native resume fallback after: %v)", startErr, err)
			}
		} else {
			return nil, fmt.Errorf("start agent command: %w", err)
		}
	}
	if reason, stopped := x.pendingStop(ctx); stopped {
		cleanupErr := x.reapStartedProcess(cmd)
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		return nil, errors.Join(fmt.Errorf("agent execution stopped before ownership persisted: %s", reason), cleanupErr)
	}

	resumeSessionID, resumeMode, resumeStatus, _ := x.nativeResumeSnapshot()
	stopReason, err := x.persistRunningOwnership(ctx, cmd)
	if stopReason != "" {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(ctxErr, err)
		}
		return nil, errors.Join(fmt.Errorf("agent execution stopped before ownership persisted: %s", stopReason), err)
	}
	if err != nil {
		return nil, fmt.Errorf("persist running agent execution: %w", err)
	}
	x.mu.Lock()
	x.progressReady = true
	x.outputPersistenceReady = true
	x.mu.Unlock()
	go x.run(ctx)
	ownershipTransferred = true
	// The live run owns cancellation/reaping before non-authoritative event I/O.
	// A slow event sink must never leave a spawned process without a run goroutine.
	e.appendLifecycleEvent("agent.invoked", input, executionID, map[string]any{"command": command, "args": args, "cwd": input.WorkingDirectory, "nativeResumeMode": resumeMode, "nativeResumeStatus": resumeStatus, "nativeSessionId": resumeSessionID}, startedAtISO)
	return x, nil
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
	timeout            time.Duration
	heartbeatTimeout   time.Duration
	gracefulShutdown   time.Duration
	maxOutputBytes     int
	lastHeartbeatAtISO string
	lastOutputAt       time.Time
	lastProgressAt     time.Time
	maxRuntimeDeadline time.Time

	mu                      sync.Mutex
	status                  string
	stdout                  []byte
	stderr                  []byte
	stdoutLogPath           string
	stderrLogPath           string
	persistedLogWriteFailed bool
	heartbeatCount          int64
	nativeSessionID         string
	nativeResumeMode        string
	nativeResumeStatus      string
	nativeResumeError       string
	progressCtx             context.Context
	progressCancel          context.CancelFunc
	progressReady           bool
	progressInFlight        bool
	outputCtx               context.Context
	outputCancel            context.CancelFunc
	outputCh                chan outputMessage
	outputDone              chan struct{}
	outputStopOnce          sync.Once
	outputPersistenceCtx    context.Context
	outputPersistenceCancel context.CancelFunc
	outputPersistenceCh     chan outputPersistence
	outputPersistenceDone   chan struct{}
	outputPersistenceReady  bool
	outputPersistenceOnce   sync.Once
	killRequested           bool
	killRequestedReason     string
	processGroupResolved    bool
	processGroupKillSent    bool
	processGroupSignalsDone bool

	killCh chan string
	doneCh chan execOutcome
}

func (x *execution) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		_ = x.Kill(ctx.Err().Error())
		out := <-x.doneCh
		x.doneCh <- out
		return out.result, out.err
	case out := <-x.doneCh:
		x.doneCh <- out
		return out.result, out.err
	}
}

func (x *execution) Kill(reason string) error {
	x.mu.Lock()
	x.killRequested = true
	if x.killRequestedReason == "" {
		x.killRequestedReason = reason
	}
	x.mu.Unlock()
	select {
	case x.killCh <- reason:
	default:
	}
	return nil
}

// ForceKill escalates an already-requested stop to SIGKILL for the whole
// process group and confirms that the group is no longer signalable. SIGKILL is
// sent at most once; after an earlier cleanup barrier timed out, later calls
// only re-probe the still-owned group. A nil result therefore authorizes the
// live-handle registry to release ownership safely.
func (x *execution) ForceKill() error {
	killErr := x.killProcessGroup()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	x.mu.Lock()
	cmd := x.process
	x.mu.Unlock()
	return processGroupCleanupError(killErr, x.awaitProcessGroupExit(cmd))
}

func (x *execution) signalProcessGroup(signal syscall.Signal) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.processGroupResolved || x.processGroupSignalsDone || x.process == nil || x.process.Process == nil {
		return os.ErrProcessDone
	}
	process := x.process.Process
	pid := process.Pid
	if pid <= 0 {
		return process.Signal(signal)
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
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.processGroupResolved || x.processGroupSignalsDone || x.process == nil || x.process.Process == nil {
		return os.ErrProcessDone
	}
	if x.processGroupKillSent {
		return nil
	}
	process := x.process.Process
	pid := process.Pid
	if pid > 0 {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
			x.processGroupKillSent = true
			return nil
		} else if err == syscall.ESRCH {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	if err := process.Kill(); err == nil {
		x.processGroupKillSent = true
		return nil
	} else {
		return err
	}
}

func (x *execution) retireUnstartedProcess(cmd *exec.Cmd) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.process == cmd && (cmd == nil || cmd.Process == nil) {
		x.processGroupResolved = true
		x.processGroupSignalsDone = true
	}
}

func (x *execution) reapStartedProcess(cmd *exec.Cmd) error {
	killErr := x.killProcessGroup()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	_ = cmd.Wait()
	cleanupErr := x.awaitProcessGroupExit(cmd)
	x.flushOutput()
	return processGroupCleanupError(killErr, cleanupErr)
}

func (x *execution) finishProcessGroup(cmd *exec.Cmd) error {
	killErr := x.killProcessGroup()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	return processGroupCleanupError(killErr, x.awaitProcessGroupExit(cmd))
}

func processGroupCleanupError(killErr, barrierErr error) error {
	// A failed signal can race the group's natural exit. Once the barrier proves
	// ESRCH, ownership is resolved and the earlier signal error is no longer a
	// cleanup failure. Without that proof, retain both diagnostics.
	if barrierErr == nil {
		return nil
	}
	return errors.Join(killErr, barrierErr)
}

func (x *execution) awaitProcessGroupExit(cmd *exec.Cmd) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.process != cmd {
		return fmt.Errorf("process group cleanup lost command ownership")
	}
	if x.processGroupResolved {
		return nil
	}
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		x.processGroupResolved = true
		x.processGroupSignalsDone = true
		return nil
	}

	// Hold the execution lock across the bounded probe barrier. ForceKill cannot
	// race a successful "no runnable members" observation and signal a newly
	// reused numeric PGID. SIGKILL was sent before entering this function;
	// probes never resend it. Zombie-only groups count as resolved so unreaped
	// orphans cannot fail an otherwise completed agent.
	pid := cmd.Process.Pid
	deadline := time.Now().Add(processGroupExitTimeout)
	for {
		live, err := shellinfra.ProcessGroupRunnable(pid)
		if err != nil {
			x.processGroupSignalsDone = true
			return fmt.Errorf("probe process group %d after SIGKILL: %w", pid, err)
		}
		if !live {
			x.processGroupResolved = true
			x.processGroupSignalsDone = true
			return nil
		}
		if !time.Now().Before(deadline) {
			x.processGroupSignalsDone = true
			return fmt.Errorf("process group %d still has runnable members %s after SIGKILL", pid, processGroupExitTimeout)
		}
		time.Sleep(processGroupProbeInterval)
	}
}

func (x *execution) run(ctx context.Context) {
	cmd := x.process
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var (
		waitErr          error
		timedOut         bool
		timeoutType      string
		killed           bool
		killReason       string
		graceKillTimer   *time.Timer
		graceKillTimerCh <-chan time.Time
		timeoutTimer     *time.Timer
		timeoutTimerCh   <-chan time.Time
		inactivityTimer  *time.Ticker
		terminateOnce    sync.Once
		terminateSignal  = func() {
			terminateOnce.Do(func() {
				if x.process.Process == nil {
					return
				}
				if err := x.signalProcessGroup(syscall.SIGTERM); err != nil {
					if err != os.ErrProcessDone {
						_ = x.killProcessGroup()
					}
					return
				}
				grace := x.gracefulShutdown
				if grace <= 0 {
					grace = 5 * time.Second
				}
				graceKillTimer = time.NewTimer(grace)
				graceKillTimerCh = graceKillTimer.C
			})
		}
	)

	if x.timeout > 0 {
		timeoutTimer = time.NewTimer(x.remainingMaxRuntime())
		timeoutTimerCh = timeoutTimer.C
		defer timeoutTimer.Stop()
	}
	defer func() {
		if graceKillTimer != nil {
			graceKillTimer.Stop()
		}
	}()
	if x.heartbeatTimeout > 0 {
		interval := x.heartbeatTimeout
		if interval > time.Second {
			interval = time.Second
		}
		inactivityTimer = time.NewTicker(interval)
		defer inactivityTimer.Stop()
	}

	ctxDone := ctx.Done()
	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-timeoutTimerCh:
			timeoutTimerCh = nil
			select {
			case waitErr = <-waitCh:
				waiting = false
				continue
			default:
			}
			if !timedOut {
				timedOut = true
				timeoutType = "max_runtime"
			}
			if killReason == "" {
				killReason = fmt.Sprintf("agent max runtime timed out after %s", x.timeout)
			}
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
			timedOut = true
			timeoutType = "idle"
			if killReason == "" {
				killReason = fmt.Sprintf("agent idle timed out after %s without observable progress", x.heartbeatTimeout)
			}
			x.setStatus("timeout")
			terminateSignal()
		case reason := <-x.killCh:
			killed = true
			killReason = reason
			x.setStatus("killed")
			terminateSignal()
		case <-ctxDone:
			ctxDone = nil
			killed = true
			if killReason == "" {
				killReason = ctx.Err().Error()
			}
			x.setStatus("killed")
			terminateSignal()
		case <-graceKillTimerCh:
			graceKillTimerCh = nil
			_ = x.killProcessGroup()
		}
	}
	stopTimer(timeoutTimer)
	stopTimer(graceKillTimer)
	cleanupErr := x.finishProcessGroup(cmd)
	x.flushOutput()

	stdout, stderr := x.resolveOutputLogs()
	status := x.finalStatus(timedOut, killed)
	if cleanupErr != nil && status == "completed" {
		status = "failed"
		x.setStatus(status)
	}
	if waitErr != nil && status == "failed" && strings.TrimSpace(stderr) == "" {
		stderr = waitErr.Error()
		// The result retains this diagnostic in memory/final persistence. Do not
		// perform synchronous log I/O on the lifecycle teardown path.
		x.markPersistedLogWriteFailed()
	}
	errorMessage := ""
	if status == "failed" || status == "timeout" || status == "killed" {
		errorMessage = strings.TrimSpace(stderr)
		if errorMessage == "" {
			errorMessage = killReason
		}
		if cleanupErr != nil {
			errorMessage = joinReasonAndError(errorMessage, cleanupErr)
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
		if tr.threadID != "" {
			// Guard against concurrent live persistence reads of nativeSessionID.
			x.mu.Lock()
			x.nativeSessionID = tr.threadID
			x.mu.Unlock()
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
	result := Result{
		Status:                       status,
		Summary:                      completion.Summary,
		Stdout:                       stdout,
		Stderr:                       stderr,
		ParseStatus:                  completion.ParseStatus,
		CompletionSignal:             completion.CompletionSignal,
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
		PID:                          pidOrZero(cmd.Process),
	}
	if cleanupErr == nil && x.shouldFallbackNativeResume(status, stdout, stderr) {
		if fallbackResult, fallbackErrorMessage, fallbackCleanupErr, ok := x.runCheckpointFallback(ctx, errorMessage); ok {
			result = fallbackResult
			status = fallbackResult.Status
			timeoutType = fallbackResult.TimeoutType
			errorMessage = fallbackErrorMessage
			endedAtISO = eventlog.FormatJavaScriptISOString(x.executor.now().UTC())
			// Fallback owns a new process group; its cleanup barrier is the
			// authority for Wait, not the original native-resume cleanupErr.
			cleanupErr = fallbackCleanupErr
		}
	}

	x.stopOutput()
	x.stopOutputPersistence()
	// Terminal AgentExecutions.Upsert is authoritative for ListActive/startup
	// recovery. Propagate failures so ActiveExecutionRegistry keeps the live
	// handle instead of releasing ownership while the durable row stays active.
	persistErr := x.persistFinal(status, result, errorMessage, endedAtISO)
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

	x.stopProgress()
	x.doneCh <- execOutcome{result: result, err: errors.Join(cleanupErr, persistErr)}
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

func (x *execution) runCheckpointFallback(ctx context.Context, nativeError string) (Result, string, error, bool) {
	if reason, stopped := x.pendingStop(ctx); stopped {
		return x.stoppedFallbackResult(reason, nil)
	}
	command, args := ResolveSpawn(x.executor.config, x.input.WorkingDirectory, x.input.Prompt)
	cmd := exec.Command(command, args...)
	cmd.Dir = x.input.WorkingDirectory
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = descendantPipeDrainGrace
	cmd.Env = buildCommandEnv(x.input.WorkingDirectory, x.input.Prompt, x.executor.config.Env, x.input.Env)
	cmd.Stdout = &streamCapture{onChunk: func(chunk []byte) { x.enqueueOutput("stdout", chunk) }}
	cmd.Stderr = &streamCapture{onChunk: func(chunk []byte) { x.enqueueOutput("stderr", chunk) }}

	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.command = command
	x.args = args
	x.process = cmd
	x.processGroupResolved = false
	x.processGroupKillSent = false
	x.processGroupSignalsDone = false
	x.status = "running"
	x.stdout = nil
	x.stderr = nil
	x.nativeSessionID = ""
	x.nativeResumeMode = "checkpoint_restart"
	x.nativeResumeStatus = "fallback_started"
	x.nativeResumeError = nativeError
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	x.mu.Unlock()
	if reason, stopped := x.pendingStop(ctx); stopped {
		x.retireUnstartedProcess(cmd)
		return x.stoppedFallbackResult(reason, nil)
	}

	if err := cmd.Start(); err != nil {
		x.retireUnstartedProcess(cmd)
		x.mu.Lock()
		x.status = "failed"
		x.nativeResumeStatus = "fallback_failed"
		x.nativeResumeError = firstNonEmpty(err.Error(), nativeError)
		x.mu.Unlock()
		return Result{}, "", nil, false
	}
	if reason, stopped := x.pendingStop(ctx); stopped {
		cleanupErr := x.reapStartedProcess(cmd)
		return x.stoppedFallbackResult(joinReasonAndError(reason, cleanupErr), cleanupErr)
	}
	stopReason, err := x.persistRunningOwnership(ctx, cmd)
	if stopReason != "" {
		// persistRunningOwnership already reaped on stop; only an unresolved
		// process-group barrier must become Wait's cleanup error (not ctx cancel).
		cleanupErr := x.finishProcessGroup(cmd)
		return x.stoppedFallbackResult(joinReasonAndError(stopReason, err), cleanupErr)
	}
	if err != nil {
		// Reap already ran for the persistence failure path; confirm the barrier.
		cleanupErr := x.finishProcessGroup(cmd)
		message := fmt.Sprintf("persist running checkpoint fallback: %v", err)
		x.mu.Lock()
		x.status = "failed"
		x.nativeResumeStatus = "fallback_failed"
		x.nativeResumeError = message
		x.mu.Unlock()
		return Result{
			Status:                       "failed",
			Summary:                      message,
			ParseStatus:                  "missing",
			ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
			ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
			ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
			LastProgressAt:               x.lastProgressAtISO(),
			PID:                          pidOrZero(cmd.Process),
		}, joinReasonAndError(message, cleanupErr), cleanupErr, true
	}
	x.executor.appendLifecycleEvent("agent.native_resume_fallback_started", x.input, x.executionID, map[string]any{"command": command, "args": args, "nativeResumeError": nativeError}, nowISO)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var (
		waitErr          error
		timedOut         bool
		killed           bool
		timeoutType      string
		killReason       string
		timeoutTimer     *time.Timer
		timeoutTimerCh   <-chan time.Time
		graceKillTimer   *time.Timer
		graceKillTimerCh <-chan time.Time
		idleTicker       *time.Ticker
		terminateOnce    sync.Once
	)
	if x.timeout > 0 {
		timeoutTimer = time.NewTimer(x.remainingMaxRuntime())
		timeoutTimerCh = timeoutTimer.C
		defer timeoutTimer.Stop()
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
		terminateOnce.Do(func() {
			if cmd.Process == nil {
				return
			}
			if err := x.signalProcessGroup(syscall.SIGTERM); err != nil {
				if err != os.ErrProcessDone {
					_ = x.killProcessGroup()
				}
				return
			}
			grace := x.gracefulShutdown
			if grace <= 0 {
				grace = 5 * time.Second
			}
			graceKillTimer = time.NewTimer(grace)
			graceKillTimerCh = graceKillTimer.C
		})
	}
	defer func() {
		if graceKillTimer != nil {
			graceKillTimer.Stop()
		}
	}()
	ctxDone := ctx.Done()
	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-timeoutTimerCh:
			timeoutTimerCh = nil
			if !timedOut {
				timedOut = true
				timeoutType = "max_runtime"
				killReason = fmt.Sprintf("agent max runtime timed out after %s", x.timeout)
			}
			terminate()
		case <-tickerChan(idleTicker):
			if timedOut || killed || x.timeSinceLastOutput() < x.heartbeatTimeout {
				continue
			}
			timedOut = true
			timeoutType = "idle"
			killReason = fmt.Sprintf("agent idle timed out after %s without observable progress", x.heartbeatTimeout)
			terminate()
		case reason := <-x.killCh:
			killed = true
			killReason = reason
			terminate()
		case <-ctxDone:
			ctxDone = nil
			killed = true
			if killReason == "" {
				killReason = ctx.Err().Error()
			}
			terminate()
		case <-graceKillTimerCh:
			graceKillTimerCh = nil
			_ = x.killProcessGroup()
		}
	}
	stopTimer(timeoutTimer)
	stopTimer(graceKillTimer)
	cleanupErr := x.finishProcessGroup(cmd)
	x.flushOutput()
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
	} else if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0) {
		status = "failed"
	}
	if cleanupErr != nil {
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
		if cleanupErr != nil {
			errorMessage = joinReasonAndError(errorMessage, cleanupErr)
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
	return Result{
		Status:                       status,
		Summary:                      completion.Summary,
		Stdout:                       stdout,
		Stderr:                       stderr,
		ParseStatus:                  completion.ParseStatus,
		CompletionSignal:             completion.CompletionSignal,
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
		PID:                          pidOrZero(cmd.Process),
	}, errorMessage, cleanupErr, true
}

func (x *execution) pendingStop(ctx context.Context) (string, bool) {
	if err := ctx.Err(); err != nil {
		return err.Error(), true
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.killRequested {
		return "", false
	}
	return firstNonEmpty(x.killRequestedReason, "agent execution stopped"), true
}

func (x *execution) stoppedFallbackResult(reason string, cleanupErr error) (Result, string, error, bool) {
	x.setStatus("killed")
	return Result{
		Status:                       "killed",
		Summary:                      reason,
		ParseStatus:                  "missing",
		ConfiguredIdleTimeoutSeconds: durationSeconds(x.heartbeatTimeout),
		ConfiguredMaxRuntimeSeconds:  durationSeconds(x.timeout),
		ElapsedRuntimeSeconds:        durationSeconds(x.executor.now().UTC().Sub(x.startedAt)),
		LastProgressAt:               x.lastProgressAtISO(),
	}, reason, cleanupErr, true
}

func joinReasonAndError(reason string, err error) string {
	if err == nil {
		return reason
	}
	if strings.TrimSpace(reason) == "" {
		return err.Error()
	}
	return reason + "; " + err.Error()
}

func (x *execution) captureOutput(stream string, chunk []byte) {
	now := x.executor.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	x.mu.Lock()
	x.heartbeatCount++
	x.lastHeartbeatAtISO = nowISO
	x.lastOutputAt = now
	if stream == "stdout" {
		x.stdout = appendTailBounded(x.stdout, chunk, x.maxOutputBytes)
	} else {
		x.stderr = appendTailBounded(x.stderr, chunk, x.maxOutputBytes)
	}
	stdout := string(x.stdout)
	stderr := string(x.stderr)
	x.mu.Unlock()

	// Capture the native session id AS SOON as it appears, so it's persisted while
	// the run is live (a human taking over mid-run needs it — completion is too
	// late). Text-mode ids can stream in across chunks, so re-extract each time; the
	// codex --json thread id arrives whole in a thread.started line, so capture it
	// once (only when text extraction found nothing and it's not already known).
	nativeSessionID := extractNativeSessionID(stdout, stderr)
	jsonSessionID := ""
	if nativeSessionID == "" && x.jsonMode() {
		jsonSessionID = extractCodexThreadID(stdout)
	}
	if nativeSessionID == "" && jsonSessionID == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if nativeSessionID != "" {
		x.nativeSessionID = nativeSessionID
	} else if jsonSessionID != "" && strings.TrimSpace(x.nativeSessionID) == "" {
		x.nativeSessionID = jsonSessionID
	}
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
	if !x.progressReady || x.progressInFlight || (!x.lastProgressAt.IsZero() && now.Sub(x.lastProgressAt) < liveProgressInterval) {
		x.mu.Unlock()
		return
	}
	x.lastProgressAt = now
	x.progressInFlight = true
	progressCtx := x.progressCtx
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
	update := ProgressUpdate{
		LoopID:         x.input.LoopID,
		RunID:          x.input.RunID,
		ExecutionID:    x.executionID,
		TailLines:      tail,
		ElapsedSeconds: elapsed,
	}
	go func() {
		defer func() {
			x.mu.Lock()
			x.progressInFlight = false
			x.mu.Unlock()
		}()
		x.executor.onProgress(progressCtx, update)
	}()
}

func (x *execution) stopProgress() {
	if x.progressCancel != nil {
		x.progressCancel()
	}
}

// jsonMode reports whether this run is a codex `--json` run (structured events).
func (x *execution) jsonMode() bool {
	return x.executor != nil && x.executor.config.LiveToolEvents && x.executor.config.Vendor == config.AgentVendorCodex
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

func (x *execution) bumpRunHeartbeat(ctx context.Context, nowISO string) {
	if x.input.RunID == "" || x.executor.repos == nil || x.executor.repos.Runs == nil {
		return
	}
	run, err := x.executor.repos.Runs.GetByID(ctx, x.input.RunID)
	if err != nil || run == nil {
		return
	}
	updated := *run
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	_ = x.executor.repos.Runs.Upsert(ctx, updated)
}

func (x *execution) persistRunningOwnership(ctx context.Context, cmd *exec.Cmd) (string, error) {
	persistCtx, cancel := context.WithTimeout(ctx, ownershipPersistenceTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- x.persistStatusContext(persistCtx, "running", nil, nil, nil)
	}()

	select {
	case err := <-done:
		if reason, stopped := x.pendingStop(ctx); stopped {
			cleanupErr := x.reapStartedProcess(cmd)
			return reason, errors.Join(ctx.Err(), cleanupErr)
		}
		if err != nil {
			err = errors.Join(err, x.reapStartedProcess(cmd))
		}
		return "", err
	case reason := <-x.killCh:
		cancel()
		cleanupErr := x.reapStartedProcess(cmd)
		return firstNonEmpty(reason, "agent execution stopped"), cleanupErr
	case <-persistCtx.Done():
		persistErr := persistCtx.Err()
		persistErr = errors.Join(persistErr, x.reapStartedProcess(cmd))
		if reason, stopped := x.pendingStop(ctx); stopped {
			return reason, persistErr
		}
		return "", persistErr
	}
}

func (x *execution) persistStatusContext(ctx context.Context, status string, heartbeatCount *int64, heartbeatAt *string, outputJSON *string) error {
	if x.executor.repos == nil || x.executor.repos.AgentExecutions == nil {
		return nil
	}
	x.mu.Lock()
	nativeSessionID := x.nativeSessionID
	nativeResumeMode := x.nativeResumeMode
	nativeResumeStatus := x.nativeResumeStatus
	nativeResumeError := x.nativeResumeError
	command := x.command
	args := append([]string(nil), x.args...)
	process := x.process
	x.mu.Unlock()
	metadata := mustJSON(x.executionMetadata(""))
	commandJSON := mustJSON(map[string]any{"command": command, "args": args})
	var osProcess *os.Process
	if process != nil {
		osProcess = process.Process
	}
	pid := int64(pidOrZero(osProcess))
	record := storage.AgentExecutionRecord{
		ID:                 x.executionID,
		ProjectID:          emptyToNil(x.input.ProjectID),
		LoopID:             emptyToNil(x.input.LoopID),
		RunID:              emptyToNil(x.input.RunID),
		Vendor:             string(x.executor.config.Vendor),
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
	return x.executor.repos.AgentExecutions.Upsert(ctx, record)
}

func (x *execution) persistFinal(status string, result Result, errorMessage, endedAtISO string) error {
	if x.executor.repos == nil || x.executor.repos.AgentExecutions == nil {
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
	if extractedNativeSessionID := extractNativeSessionID(embeddedStdout, embeddedStderr); extractedNativeSessionID != "" {
		nativeSessionID = extractedNativeSessionID
	}
	if nativeResumeMode == "native_resume" && status == "failed" {
		nativeResumeStatus = "failed"
		nativeResumeError = firstNonEmpty(nativeResumeError, errorMessage, strings.TrimSpace(result.Stderr))
	}
	if nativeSessionID != "" && (nativeResumeStatus == "" || nativeResumeStatus == "unavailable") {
		nativeResumeStatus = "captured"
	}
	record := storage.AgentExecutionRecord{
		ID:                 x.executionID,
		ProjectID:          emptyToNil(x.input.ProjectID),
		LoopID:             emptyToNil(x.input.LoopID),
		RunID:              emptyToNil(x.input.RunID),
		Vendor:             string(x.executor.config.Vendor),
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
	// Terminal status is authoritative for startup/reconcile ListActive. Wait for
	// the durable write with the ownership budget instead of the short best-effort
	// output side-effect timeout, which can leave status as running/cancelling
	// after the live handle is released.
	ctx, cancel := context.WithTimeout(context.Background(), ownershipPersistenceTimeout)
	defer cancel()
	if err := x.executor.repos.AgentExecutions.Upsert(ctx, record); err != nil {
		return fmt.Errorf("persist terminal agent execution status: %w", err)
	}
	return nil
}

func (x *execution) currentStatus() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.status
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

func (x *execution) remainingMaxRuntime() time.Duration {
	if x.timeout <= 0 {
		return 0
	}
	if x.maxRuntimeDeadline.IsZero() {
		return x.timeout
	}
	remaining := time.Until(x.maxRuntimeDeadline)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
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

type outputMessage struct {
	stream string
	chunk  []byte
	done   chan struct{}
}

type outputSnapshot struct {
	now            time.Time
	nowISO         string
	heartbeatCount int64
	stdout         string
	stderr         string
	status         string
	persistOutput  bool
}

type outputPersistence struct {
	status         string
	heartbeatCount int64
	heartbeatAt    string
	outputJSON     string
}

func (x *execution) enqueueOutput(stream string, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	x.captureOutput(stream, chunk)
	message := outputMessage{stream: stream, chunk: chunk}
	select {
	case <-x.outputCtx.Done():
		x.markPersistedLogWriteFailed()
	case x.outputCh <- message:
	default:
		x.markPersistedLogWriteFailed()
	}
}

func (x *execution) processOutput() {
	defer close(x.outputDone)
	for {
		select {
		case <-x.outputCtx.Done():
			return
		case message := <-x.outputCh:
			if x.outputCtx.Err() != nil {
				return
			}
			if len(message.chunk) > 0 {
				x.processOutputSideEffects(message)
			}
			if message.done != nil {
				close(message.done)
			}
		}
	}
}

func (x *execution) processOutputSideEffects(message outputMessage) {
	if x.outputCtx.Err() != nil {
		return
	}
	path := x.stderrLogPath
	if message.stream == "stdout" {
		path = x.stdoutLogPath
	}
	if x.appendPersistedLog(path, message.chunk) {
		x.markPersistedLogWriteFailed()
	}
	if x.outputCtx.Err() != nil {
		return
	}
	snapshot := x.currentOutputSnapshot()
	if snapshot.persistOutput {
		x.submitOutputPersistence(outputPersistence{
			status:         snapshot.status,
			heartbeatCount: snapshot.heartbeatCount,
			heartbeatAt:    snapshot.nowISO,
			outputJSON:     x.outputJSON(snapshot.stdout, snapshot.stderr),
		})
	}
	x.maybeEmitProgress(snapshot.now, snapshot.stdout, snapshot.stderr)
}

func (x *execution) currentOutputSnapshot() outputSnapshot {
	x.mu.Lock()
	defer x.mu.Unlock()
	return outputSnapshot{
		now:            x.lastOutputAt,
		nowISO:         x.lastHeartbeatAtISO,
		heartbeatCount: x.heartbeatCount,
		stdout:         string(x.stdout),
		stderr:         string(x.stderr),
		status:         x.status,
		persistOutput:  x.outputPersistenceReady,
	}
}

func (x *execution) flushOutput() {
	done := make(chan struct{})
	select {
	case <-x.outputCtx.Done():
		return
	case x.outputCh <- outputMessage{done: done}:
	default:
		x.markPersistedLogWriteFailed()
		return
	}
	if !waitForWorker(done, outputPersistenceTimeout) {
		x.markPersistedLogWriteFailed()
	}
}

func (x *execution) stopOutput() {
	x.outputStopOnce.Do(func() {
		x.outputCancel()
		_ = waitForWorker(x.outputDone, outputPersistenceTimeout)
	})
}

func (x *execution) submitOutputPersistence(update outputPersistence) {
	select {
	case x.outputPersistenceCh <- update:
		return
	default:
	}
	select {
	case <-x.outputPersistenceCh:
	default:
	}
	select {
	case x.outputPersistenceCh <- update:
	default:
	}
}

func (x *execution) processOutputPersistence() {
	defer close(x.outputPersistenceDone)
	for {
		select {
		case <-x.outputPersistenceCtx.Done():
			return
		case update := <-x.outputPersistenceCh:
			if x.outputPersistenceCtx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(x.outputPersistenceCtx, outputPersistenceTimeout)
			_ = x.persistStatusContext(ctx, update.status, &update.heartbeatCount, &update.heartbeatAt, &update.outputJSON)
			x.bumpRunHeartbeat(ctx, update.heartbeatAt)
			cancel()
		}
	}
}

func (x *execution) stopOutputPersistence() {
	x.outputPersistenceOnce.Do(func() {
		x.outputPersistenceCancel()
		_ = waitForWorker(x.outputPersistenceDone, outputPersistenceTimeout)
	})
}

func waitForWorker(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func runBoundedSideEffect(timeout time.Duration, sideEffect func(context.Context)) bool {
	if sideEffect == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		sideEffect(ctx)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
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
	switch vendor {
	case config.AgentVendorCodex, config.AgentVendorClaudeCode:
		return true
	default:
		return false
	}
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
	command := resolveCommand(cfg)
	var resume string
	switch cfg.Vendor {
	case config.AgentVendorCodex:
		resume = command + " resume " + shellSingleQuote(sessionID)
	case config.AgentVendorClaudeCode:
		resume = command + " --resume " + shellSingleQuote(sessionID)
	default:
		return "", false
	}
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
	switch cfg.Vendor {
	case config.AgentVendorClaudeCode:
		return "claude"
	case config.AgentVendorCursorCLI:
		return "agent"
	case config.AgentVendorGrokBuild:
		return "grok"
	default:
		return string(cfg.Vendor)
	}
}

func resolveArgs(cfg ExecutorConfig, workingDirectory string, prompt string) []string {
	resolvedArgs := stringArgs(cfg.Params["args"])
	switch cfg.Vendor {
	case config.AgentVendorClaudeCode:
		return resolveClaudeArgs(cfg, resolvedArgs, prompt)
	case config.AgentVendorCodex:
		return resolveCodexArgs(cfg, resolvedArgs, prompt)
	case config.AgentVendorOpenCode:
		return resolveOpenCodeArgs(cfg, resolvedArgs, workingDirectory, prompt)
	case config.AgentVendorCursorCLI:
		return resolveCursorArgs(cfg, resolvedArgs, prompt)
	case config.AgentVendorGrokBuild:
		return resolveGrokArgs(cfg, resolvedArgs, workingDirectory, prompt)
	default:
		return append([]string{}, resolvedArgs...)
	}
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
	withModel := prependModelFlag(resolved, cfg.Model, "--model", []string{"--model", "-m"})
	if hasAnyFlag(withModel, []string{"-"}) {
		return withModel
	}
	return append(withModel, prompt)
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

func resolveNativeResumeArgs(cfg ExecutorConfig, workingDirectory string, args []string, sessionID string, prompt string) []string {
	switch cfg.Vendor {
	case config.AgentVendorClaudeCode:
		resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
		if !hasAnyFlag(resolved, []string{"--continue", "--resume"}) {
			resolved = append(resolved, "--resume", sessionID)
		}
		if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
			resolved = append(resolved, "--print", prompt)
		}
		if !hasAnyFlag(resolved, []string{"--dangerously-skip-permissions"}) {
			resolved = append(resolved, "--dangerously-skip-permissions")
		}
		return resolved
	case config.AgentVendorCodex:
		resolved := removeFirstArg(args, "exec")
		resolved = removeFirstArg(resolved, "resume")
		withModel := prependModelFlag(append([]string{"exec"}, resolved...), cfg.Model, "--model", []string{"--model", "-m"})
		base := append(withModel, "resume")
		base = append(base, sessionID)
		if containsArg(withModel, "-") {
			return base
		}
		return append(base, prompt)
	case config.AgentVendorOpenCode:
		resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model", "-m"})
		if !containsArg(resolved, "run") {
			resolved = append([]string{"run"}, resolved...)
		}
		if strings.TrimSpace(workingDirectory) != "" && !hasAnyFlag(resolved, []string{"--dir"}) {
			resolved = appendDirFlag(resolved, workingDirectory)
		}
		if !hasAnyFlag(resolved, []string{"--session", "--continue"}) {
			resolved = append(resolved, "--session", sessionID)
		}
		if !hasAnyFlag(resolved, []string{"-p", "--prompt", "-f", "--file"}) {
			resolved = append(resolved, prompt)
		}
		return resolved
	case config.AgentVendorCursorCLI:
		resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
		if !hasAnyFlag(resolved, []string{"--continue", "--resume"}) {
			resolved = append(resolved, "--resume", sessionID)
		}
		if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
			resolved = append(resolved, "--print", prompt)
		}
		return resolved
	default:
		return append([]string{}, args...)
	}
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
	// Never expose the trusted-env file path to agent processes. Provider tokens
	// for `looper review submit` stay on the daemon-side trusted review proxy;
	// agents may only receive TrustedReviewSockEnv (a non-secret capability path).
	delete(envMap, forge.TrustedEnvFileEnv)
	for _, source := range envSources {
		maps.Copy(envMap, source)
	}
	// Re-delete after source merge so caller env maps cannot reintroduce it.
	delete(envMap, forge.TrustedEnvFileEnv)
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
	if x.executor != nil && x.executor.appendPersistedLog != nil {
		return x.executor.appendPersistedLog(path, chunk)
	}
	return appendPersistedLogFile(path, chunk)
}

func appendPersistedLogFile(path string, chunk []byte) bool {
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
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, CompletionMarkerPrefix) {
			payload := strings.TrimPrefix(line, CompletionMarkerPrefix)
			var parsed map[string]any
			if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
				return completionParse{ParseStatus: "invalid_json", CompletionSignal: CompletionMarkerPrefix}
			}
			result := completionParse{
				ParseStatus:      "parsed",
				CompletionSignal: CompletionMarkerPrefix,
				Artifacts:        asStringSlice(parsed["artifacts"]),
				ChangedFiles:     asStringSlice(parsed["changedFiles"]),
				Commits:          asStringSlice(parsed["commits"]),
			}
			if state, err := lifecycle.FromMap(parsed["git_pr_lifecycle"]); err == nil {
				result.Lifecycle = state
			}
			if summary, ok := parsed["summary"].(string); ok {
				result.Summary = summary
			}
			if isTemplateCompletion(result, parsed) {
				continue
			}
			return result
		}
	}
	return completionParse{ParseStatus: "missing"}
}

func isTemplateCompletion(result completionParse, parsed map[string]any) bool {
	if strings.TrimSpace(result.Summary) != "<one-sentence summary>" {
		return false
	}
	if len(parsed) != 1 {
		return false
	}
	_, ok := parsed["summary"]
	return ok
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
	if e.appendLifecycleRecord == nil {
		return
	}
	record := storage.EventLogRecord{
		ID:               eventlog.NewEventID("event"),
		EventType:        eventType,
		ProjectID:        emptyToNil(input.ProjectID),
		LoopID:           emptyToNil(input.LoopID),
		RunID:            emptyToNil(input.RunID),
		EntityType:       stringPtr("agent_execution"),
		EntityID:         &executionID,
		ActorType:        stringPtr("agent"),
		ActorID:          stringPtr(string(e.config.Vendor)),
		ActorDisplayName: stringPtr(string(e.config.Vendor)),
		PayloadJSON:      mustJSON(payload),
		CreatedAt:        createdAt,
	}
	runBoundedSideEffect(outputPersistenceTimeout, func(ctx context.Context) {
		_ = e.appendLifecycleRecord(ctx, record)
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

func pidOrZero(process *os.Process) int {
	if process == nil {
		return 0
	}
	return process.Pid
}
