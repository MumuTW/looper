package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMaxOutputBytes = 256 * 1024
	defaultGracefulStop   = 5 * time.Second
	descendantDrainGrace  = 100 * time.Millisecond
	processGroupExitWait  = time.Second
	// startAttempts covers transient Linux ETXTBSY after a binary/script is
	// installed (common when tests write a fake tool then exec it immediately).
	startAttempts      = 8
	startRetryBaseWait = 5 * time.Millisecond
)

type Result struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
	DurationMS      int64
}

type Options struct {
	Command          string
	Args             []string
	CWD              string
	Env              map[string]string
	Stdin            string
	Timeout          time.Duration
	GracefulShutdown time.Duration
	MaxCapturedBytes int
}

type CommandExecutionError struct {
	Message string
	Result  Result
}

func (e *CommandExecutionError) Error() string { return e.Message }

func Run(ctx context.Context, options Options) (Result, error) {
	if strings.TrimSpace(options.Command) == "" {
		return Result{}, fmt.Errorf("command is required")
	}

	startedAt := time.Now()
	maxCapturedBytes := options.MaxCapturedBytes
	if maxCapturedBytes <= 0 {
		maxCapturedBytes = defaultMaxOutputBytes
	}
	gracefulShutdown := options.GracefulShutdown
	if gracefulShutdown <= 0 {
		gracefulShutdown = defaultGracefulStop
	}

	stdoutBuffer := newBoundedBuffer(maxCapturedBytes)
	stderrBuffer := newBoundedBuffer(maxCapturedBytes)

	cmd, err := startCommand(ctx, options, stdoutBuffer, stderrBuffer)
	if err != nil {
		return Result{}, fmt.Errorf("start command: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var (
		waitErr          error
		timedOut         bool
		canceledErr      error
		cleanupErr       error
		forceKillSent    bool
		timeoutTimer     *time.Timer
		killTimer        *time.Timer
		terminationStart <-chan time.Time
		killAt           <-chan time.Time
		terminateOnce    sync.Once
	)

	terminate := func() {
		terminateOnce.Do(func() {
			if cmd.Process == nil {
				return
			}
			if err := signalCommandGroup(cmd, syscall.SIGTERM); err != nil && !isProcessDone(err) {
				forceKillSent = true
				_ = signalCommandGroup(cmd, syscall.SIGKILL)
				return
			}
			if gracefulShutdown <= 0 {
				forceKillSent = true
				_ = signalCommandGroup(cmd, syscall.SIGKILL)
				return
			}
			killTimer = time.NewTimer(gracefulShutdown)
			killAt = killTimer.C
		})
	}

	if options.Timeout > 0 {
		timeoutTimer = time.NewTimer(options.Timeout)
		terminationStart = timeoutTimer.C
	}
	defer stopAndDrainTimer(timeoutTimer)
	defer func() { stopAndDrainTimer(killTimer) }()

	ctxDone := ctx.Done()
	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			// The root can exit while a background descendant remains in the
			// owned group. Resolve the group on every terminal path, not only
			// timeout/cancellation, before returning ownership to the caller.
			var err error
			if forceKillSent {
				err = WaitProcessGroupExit(cmd, processGroupExitWait)
			} else {
				err = KillProcessGroupAndWait(cmd, processGroupExitWait)
			}
			if err != nil && !isProcessDone(err) {
				cleanupErr = err
			}
			waiting = false
		case <-terminationStart:
			timedOut = true
			terminationStart = nil
			terminate()
		case <-killAt:
			killAt = nil
			forceKillSent = true
			_ = signalCommandGroup(cmd, syscall.SIGKILL)
		case <-ctxDone:
			ctxDone = nil
			canceledErr = ctx.Err()
			terminate()
		}
	}

	duration := time.Since(startedAt)
	result := Result{
		ExitCode:        exitCode(cmd),
		Stdout:          stdoutBuffer.String(),
		Stderr:          stderrBuffer.String(),
		StdoutTruncated: stdoutBuffer.Truncated(),
		StderrTruncated: stderrBuffer.Truncated(),
		Duration:        duration,
		DurationMS:      duration.Milliseconds(),
	}

	if timedOut {
		primaryErr := &CommandExecutionError{Message: "Command timed out", Result: result}
		return result, joinProcessGroupCleanupError(primaryErr, cleanupErr)
	}
	if canceledErr != nil {
		return result, joinProcessGroupCleanupError(canceledErr, cleanupErr)
	}
	if result.ExitCode != 0 {
		primaryErr := &CommandExecutionError{Message: commandFailureMessage(result), Result: result}
		return result, joinProcessGroupCleanupError(primaryErr, cleanupErr)
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		waitErr = nil
	}
	if waitErr != nil {
		return result, joinProcessGroupCleanupError(waitErr, cleanupErr)
	}
	return result, joinProcessGroupCleanupError(nil, cleanupErr)
}

func joinProcessGroupCleanupError(primaryErr, cleanupErr error) error {
	if cleanupErr == nil || isProcessDone(cleanupErr) {
		return primaryErr
	}
	return errors.Join(primaryErr, fmt.Errorf("clean up command process group: %w", cleanupErr))
}

func startCommand(ctx context.Context, options Options, stdout, stderr *boundedBuffer) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < startAttempts; attempt++ {
		if attempt > 0 {
			wait := startRetryBaseWait * time.Duration(attempt)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		cmd := exec.Command(options.Command, options.Args...)
		ConfigureProcessGroup(cmd)
		cmd.WaitDelay = descendantDrainGrace
		cmd.Dir = options.CWD
		if len(options.Env) > 0 {
			cmd.Env = envSlice(options.Env)
		}
		if options.Stdin != "" {
			cmd.Stdin = strings.NewReader(options.Stdin)
		}
		// Fresh buffers each attempt: Start never ran on failure, but reset
		// so a partial Write cannot leak across retries if that ever changes.
		stdout.reset()
		stderr.reset()
		cmd.Stdout = stdout
		cmd.Stderr = stderr

		if err := cmd.Start(); err != nil {
			lastErr = err
			if isTextFileBusy(err) {
				continue
			}
			return nil, err
		}
		return cmd, nil
	}
	return nil, lastErr
}

func stopAndDrainTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func isTextFileBusy(err error) bool {
	return errors.Is(err, syscall.ETXTBSY)
}

func commandFailureMessage(result Result) string {
	message := fmt.Sprintf("Command exited with code %d", result.ExitCode)
	stderr := strings.TrimSpace(result.Stderr)
	stdout := strings.TrimSpace(result.Stdout)
	if stderr != "" {
		message += ": " + stderr
	}
	if stdout != "" {
		if stderr != "" {
			message += "\nstdout: " + stdout
		} else {
			message += ": " + stdout
		}
	}
	return message
}

func envSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	return values
}

func exitCode(cmd *exec.Cmd) int {
	if cmd == nil || cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

func isProcessDone(err error) bool {
	return err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

func signalCommandGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}

// ConfigureProcessGroup gives cmd ownership of a fresh process group so
// cancellation can stop every descendant that remains in the group.
func ConfigureProcessGroup(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

// KillProcessGroup forcibly stops cmd and every descendant remaining in its
// process group. A command that has not started or whose group has exited is
// reported as os.ErrProcessDone.
func KillProcessGroup(cmd *exec.Cmd) error {
	err := signalCommandGroup(cmd, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// KillProcessGroupAndWait sends one terminal SIGKILL using the live command's
// owned process-group id, then waits until that group is no longer signalable.
// The poll never sends another destructive signal, so a numeric group id that
// is reused after disappearance cannot be killed by delayed escalation.
func KillProcessGroupAndWait(cmd *exec.Cmd, timeout time.Duration) error {
	err := KillProcessGroup(cmd)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	return WaitProcessGroupExit(cmd, timeout)
}

// WaitProcessGroupExit waits for a previously signaled owned process group to
// disappear without sending another signal to its numeric id.
func WaitProcessGroupExit(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	if timeout <= 0 {
		timeout = processGroupExitWait
	}
	pid := cmd.Process.Pid
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("probe command process group %d: %w", pid, err)
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return fmt.Errorf("command process group %d remained live after SIGKILL for %s", pid, timeout)
		}
	}
}

type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLen := len(p)
	if len(b.data) >= b.limit {
		if originalLen > 0 {
			b.truncated = true
		}
		return originalLen, nil
	}
	remaining := b.limit - len(b.data)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return originalLen, nil
}

func (b *boundedBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
	b.truncated = false
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
