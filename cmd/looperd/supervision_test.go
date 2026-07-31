package main

import (
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestStdinSupervisionEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "unset", env: map[string]string{}, want: false},
		{name: "one", env: map[string]string{stdinSupervisionEnv: "1"}, want: true},
		{name: "true", env: map[string]string{stdinSupervisionEnv: "true"}, want: true},
		{name: "zero", env: map[string]string{stdinSupervisionEnv: "0"}, want: false},
		{name: "empty", env: map[string]string{stdinSupervisionEnv: ""}, want: false},
		{name: "garbage", env: map[string]string{stdinSupervisionEnv: "yes please"}, want: false},
		{name: "padded", env: map[string]string{stdinSupervisionEnv: " true "}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stdinSupervisionEnabled(tc.env); got != tc.want {
				t.Fatalf("stdinSupervisionEnabled(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestStdinSupervisionRequestsShutdownOnEOFThenForceExits(t *testing.T) {
	reader, writer := io.Pipe()
	shutdownCh := make(chan struct{})
	exitCh := make(chan struct{})
	startStdinSupervision(reader, io.Discard, 50*time.Millisecond,
		func() { close(shutdownCh) },
		func() { close(exitCh) })

	select {
	case <-shutdownCh:
		t.Fatal("shutdown requested before stdin closed")
	case <-time.After(100 * time.Millisecond):
	}

	_ = writer.Close()

	select {
	case <-shutdownCh:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown not requested after stdin closed")
	}
	select {
	case <-exitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("force exit not requested after grace period")
	}
}

// TestForwardingNotifierDeliversSignalBufferedBeforeListenerRegisters covers
// the startup race: the supervision pipe closes while bootstrap is still
// starting the runtime, so the shutdown request lands in the supervision
// channel before bootstrap's listener calls Notify. The forwarder must hand
// that buffered signal to the late listener instead of losing it.
func TestForwardingNotifierDeliversSignalBufferedBeforeListenerRegisters(t *testing.T) {
	source := make(chan os.Signal, 1)
	requestShutdownSignal(source)

	notifier := newForwardingSignalNotifier(source)
	listener := make(chan os.Signal, 1)
	notifier.Notify(listener, os.Interrupt, syscall.SIGTERM)
	defer notifier.Stop(listener)

	select {
	case sig := <-listener:
		if sig != syscall.SIGTERM {
			t.Fatalf("forwarded signal = %v, want SIGTERM", sig)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal buffered before Notify never reached the listener")
	}
}

func TestForwardingNotifierStopEndsForwarding(t *testing.T) {
	source := make(chan os.Signal, 1)
	notifier := newForwardingSignalNotifier(source)
	listener := make(chan os.Signal, 1)
	notifier.Notify(listener, os.Interrupt, syscall.SIGTERM)
	notifier.Stop(listener)

	requestShutdownSignal(source)
	select {
	case sig := <-listener:
		t.Fatalf("received %v after Stop", sig)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRequestShutdownSignalDropsWhenRequestAlreadyPending(t *testing.T) {
	signals := make(chan os.Signal, 1)
	requestShutdownSignal(signals)
	requestShutdownSignal(signals)
	if got := len(signals); got != 1 {
		t.Fatalf("pending signals = %d, want 1", got)
	}
}

func TestStdinSupervisionSurvivesInputBeforeEOF(t *testing.T) {
	reader, writer := io.Pipe()
	shutdownCh := make(chan struct{})
	startStdinSupervision(reader, io.Discard, time.Millisecond,
		func() { close(shutdownCh) },
		func() {})

	if _, err := writer.Write([]byte("still alive\n")); err != nil {
		t.Fatalf("write to supervision pipe: %v", err)
	}
	select {
	case <-shutdownCh:
		t.Fatal("shutdown requested while pipe still open")
	case <-time.After(100 * time.Millisecond):
	}

	_ = writer.Close()
	select {
	case <-shutdownCh:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown not requested after stdin closed")
	}
}
