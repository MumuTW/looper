package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// stdinSupervisionEnv opts the daemon into tying its lifetime to the process
// holding the write end of stdin. A supervisor (today: the e2e harness) passes
// a pipe as stdin and sets this variable; when the supervisor dies — even via
// SIGKILL or a go-test timeout panic, which skip its cleanups — the kernel
// closes the pipe, stdin reads EOF, and the daemon shuts down instead of
// living on as an orphan. Off by default because a daemonized looperd's stdin
// is /dev/null, which reads EOF immediately.
const stdinSupervisionEnv = "LOOPER_EXIT_ON_STDIN_CLOSE"

// stdinSupervisionForceExitDelay bounds graceful shutdown once stdin closes.
// With the supervisor gone nothing else will reap the process, so a hung
// shutdown must not keep the orphan alive.
const stdinSupervisionForceExitDelay = 30 * time.Second

func stdinSupervisionEnabled(env map[string]string) bool {
	value, ok := lookupEnvValue(env, stdinSupervisionEnv)
	if !ok {
		return false
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && enabled
}

// lookupEnvValue mirrors how runWithDeps treats deps.env: a non-nil map is the
// complete environment (tests), nil falls through to the process environment.
func lookupEnvValue(env map[string]string, key string) (string, bool) {
	if env != nil {
		value, ok := env[key]
		return value, ok
	}
	return os.LookupEnv(key)
}

// startStdinSupervision drains stdin until EOF or a read error, then asks the
// daemon to shut down and force-exits if that takes longer than
// forceExitDelay. requestShutdown and forceExit are injectable for tests; the
// production caller wires them to a self-signal and os.Exit.
func startStdinSupervision(stdin io.Reader, stderr io.Writer, forceExitDelay time.Duration, requestShutdown func(), forceExit func()) {
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		_, _ = fmt.Fprintln(stderr, "looperd: stdin closed; supervising process exited; shutting down")
		requestShutdown()
		time.Sleep(forceExitDelay)
		_, _ = fmt.Fprintf(stderr, "looperd: graceful shutdown still running %s after stdin closed; forcing exit\n", forceExitDelay)
		forceExit()
	}()
}
