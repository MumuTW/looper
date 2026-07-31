package main

import (
	"io"
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
