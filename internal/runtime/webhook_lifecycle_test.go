package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/processidentity"
)

func TestPSProcessStartForcesCLocale(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux process identity is read from /proc rather than ps")
	}
	dir := t.TempDir()
	psPath := filepath.Join(dir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nif [ \"${LC_ALL}\" = \"C\" ] && [ \"${TZ}\" = \"UTC\" ]; then\n  printf 'Mon May 18 12:34:56 2026\\n'\nelse\n  printf 'Lun Mai 18 12:34:56 2026\\n'\nfi\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps) error = %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("TZ", "America/Los_Angeles")

	got, err := processidentity.StartTime(4242)
	if err != nil {
		t.Fatalf("psProcessStart() error = %v", err)
	}
	want := time.Date(2026, time.May, 18, 12, 34, 56, 0, time.UTC).UnixNano()
	if got != want {
		t.Fatalf("psProcessStart() = %d, want %d", got, want)
	}
}

func TestAdoptedForwarderProcessWaitIgnoresProbeErrors(t *testing.T) {
	t.Parallel()

	proc := &adoptedForwarderProcess{
		pid:          4242,
		processStart: 99,
		pollInterval: time.Millisecond,
		probe: &sequenceProcessProbe{
			alive: []probeAliveResult{
				{alive: false, err: errors.New("temporary probe failure")},
				{alive: true},
				{alive: true},
				{alive: false},
			},
			start: []probeStartResult{
				{err: errors.New("temporary start failure")},
				{start: 99},
			},
		},
	}

	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() error = %v, want nil after transient probe failures and eventual exit", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Wait() did not recover from transient probe failures")
	}
}

type sequenceProcessProbe struct {
	mu    sync.Mutex
	alive []probeAliveResult
	start []probeStartResult
}

type probeAliveResult struct {
	alive bool
	err   error
}

type probeStartResult struct {
	start int64
	err   error
}

func (p *sequenceProcessProbe) IsAlive(pid int) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.alive) == 0 {
		return false, nil
	}
	result := p.alive[0]
	p.alive = p.alive[1:]
	return result.alive, result.err
}

func (p *sequenceProcessProbe) StartTime(pid int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.start) == 0 {
		return 0, nil
	}
	result := p.start[0]
	p.start = p.start[1:]
	return result.start, result.err
}

func (p *sequenceProcessProbe) Argv(pid int) ([]string, error) { return nil, nil }

func (p *sequenceProcessProbe) ExecutablePath(pid int) (string, error) { return "", nil }
