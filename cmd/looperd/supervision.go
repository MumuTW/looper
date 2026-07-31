package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MumuTW/looper/internal/bootstrap"
)

// stdinSupervisionEnv opts the daemon into tying its lifetime to the process
// holding the write end of stdin. A supervisor (today: the e2e harness) passes
// a pipe as stdin and sets this variable; when the supervisor dies — even via
// SIGKILL or a go-test timeout panic, which skip its cleanups — the kernel
// closes the pipe, stdin reads EOF, and the daemon shuts down instead of
// living on as an orphan. Off by default because a daemonized looperd's stdin
// is /dev/null, which reads EOF immediately.
//
// What this still does not catch: a SIGKILL delivered to looperd itself skips
// both this path and graceful shutdown, so agent process groups the daemon
// spawned survive until a restarted daemon's recovery reconciles them; the
// force-exit below abandons those same groups when graceful shutdown hangs;
// and a supervisor that does not opt in keeps today's orphan behavior.
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

// setupStdinSupervision starts stdin supervision when enabled and returns the
// SignalNotifier bootstrap must use, or nil when disabled (bootstrap falls
// back to its default). Registering signal.Notify here — before bootstrap
// starts the runtime — matters: a shutdown signal that lands mid-startup would
// otherwise hit the default SIGTERM action and terminate the process without
// draining the webhook forwarders and agent executions startup has already
// launched. From this point signals buffer in the channel until bootstrap's
// shutdown listener begins consuming them via the returned notifier.
func setupStdinSupervision(env map[string]string, stderr io.Writer) bootstrap.SignalNotifier {
	if !stdinSupervisionEnabled(env) {
		return nil
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	startStdinSupervision(os.Stdin, stderr, stdinSupervisionForceExitDelay,
		func() { requestShutdownSignal(signals) },
		func() { os.Exit(1) })
	return newForwardingSignalNotifier(signals)
}

// requestShutdownSignal injects a synthetic SIGTERM into the supervision
// signal channel. A full channel means a shutdown request is already pending,
// so dropping the send loses nothing.
func requestShutdownSignal(signals chan<- os.Signal) {
	select {
	case signals <- syscall.SIGTERM:
	default:
	}
}

// startStdinSupervision drains stdin until EOF or a read error, then asks the
// daemon to shut down and force-exits if that takes longer than
// forceExitDelay. requestShutdown and forceExit are injectable for tests; the
// production caller wires them to the supervision signal channel and os.Exit.
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

// forwardingSignalNotifier adapts the early-registered supervision signal
// channel to bootstrap's SignalNotifier seam. Bootstrap's shutdown listener
// registers late (after the runtime is up); forwarding from the channel
// registered in setupStdinSupervision means a signal from the startup window
// is delivered to the listener instead of lost to a channel nobody reads.
// Single-use, matching how bootstrap drives the seam: one Notify, then one
// Stop after the listener is done.
type forwardingSignalNotifier struct {
	source  <-chan os.Signal
	stop    chan struct{}
	done    chan struct{}
	started atomic.Bool
}

func newForwardingSignalNotifier(source <-chan os.Signal) *forwardingSignalNotifier {
	return &forwardingSignalNotifier{source: source, stop: make(chan struct{}), done: make(chan struct{})}
}

// Notify ignores the requested signal set: the source channel is already
// registered for exactly the signals bootstrap's shutdown listener asks for
// (os.Interrupt, SIGTERM).
func (n *forwardingSignalNotifier) Notify(ch chan<- os.Signal, _ ...os.Signal) {
	n.started.Store(true)
	go func() {
		defer close(n.done)
		for {
			select {
			case <-n.stop:
				return
			case sig := <-n.source:
				select {
				case <-n.stop:
					return
				case ch <- sig:
				}
			}
		}
	}()
}

// Stop waits for the forwarding goroutine to exit so that no signal can be
// delivered after Stop returns. Without the wait, a signal already buffered
// in source when Stop closes n.stop could still win the goroutine's select
// and land in a channel whose listener has already gone away.
func (n *forwardingSignalNotifier) Stop(chan<- os.Signal) {
	close(n.stop)
	if n.started.Load() {
		<-n.done
	}
}
