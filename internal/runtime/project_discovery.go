package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// projectDiscoveryRunner owns post-commit project discovery for one Runtime.
// It is deliberately in-memory only: SQLite Project metadata remains the
// authority for discovery state, while this runner supplies cancellation and a
// shutdown drain boundary for work started after registration returns.
type projectDiscoveryRunner struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

func newProjectDiscoveryRunner() *projectDiscoveryRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &projectDiscoveryRunner{ctx: ctx, cancel: cancel}
}

func (r *projectDiscoveryRunner) Context() context.Context {
	if r == nil {
		return context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
}

// Go starts fn only while the runner is open. Closing and Wait are serialized
// with WaitGroup.Add so shutdown cannot miss a discovery launched at its edge.
func (r *projectDiscoveryRunner) Go(fn func()) bool {
	if r == nil || fn == nil {
		return false
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		fn()
	}()
	return true
}

func (r *projectDiscoveryRunner) Cancel() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		r.cancel()
	}
	r.mu.Unlock()
}

func (r *projectDiscoveryRunner) Wait(timeout time.Duration) error {
	if r == nil {
		return nil
	}
	r.Cancel()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out waiting for post-commit project discovery after %s", timeout)
	}
}

func (r *Runtime) projectDiscoveryContext() context.Context {
	if r == nil {
		return context.Background()
	}
	r.mu.RLock()
	runner := r.projectDiscovery
	r.mu.RUnlock()
	return runner.Context()
}

func (r *Runtime) scheduleProjectDiscovery(run func()) {
	if r == nil {
		return
	}
	r.mu.RLock()
	runner := r.projectDiscovery
	r.mu.RUnlock()
	if runner == nil || runner.Go(run) {
		return
	}
	if r.logger != nil {
		r.logger.Warn("skipping post-commit project discovery after runtime shutdown", nil)
	}
}

func (r *Runtime) stopProjectDiscovery() {
	if r == nil {
		return
	}
	r.mu.RLock()
	runner := r.projectDiscovery
	timeout := r.shutdownTimeout
	r.mu.RUnlock()
	if err := runner.Wait(timeout); err != nil {
		r.mu.Lock()
		r.shutdownDrainErr = errors.Join(r.shutdownDrainErr, err)
		r.mu.Unlock()
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for post-commit project discovery", map[string]any{"timeoutMs": timeout.Milliseconds()})
		}
	}
}
