package runtime

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrExecutionAdmissionClosed    = errors.New("execution admission is closed")
	ErrExecutionLoopStopping       = errors.New("execution loop is stopping")
	ErrExecutionSupervisorDegraded = errors.New("execution supervisor is degraded")
)

type activeExecution interface {
	Kill(string) error
}

type activeExecutionEntry struct {
	loopID    string
	execution activeExecution
}

type ExecutionSupervisor struct {
	mu              sync.Mutex
	executions      map[string]activeExecutionEntry
	reservations    map[uint64]*ExecutionReservation
	stoppingLoops   map[string]int
	nextReservation uint64
	admissionClosed bool
	shutdownReason  string
	degradedErr     error
	stateChanged    chan struct{}
}

// ActiveExecutionRegistry is retained as a source-compatible name while
// callers migrate to the deeper ExecutionSupervisor Module.
type ActiveExecutionRegistry = ExecutionSupervisor

func NewExecutionSupervisor() *ExecutionSupervisor {
	return &ExecutionSupervisor{
		executions:    make(map[string]activeExecutionEntry),
		reservations:  make(map[uint64]*ExecutionReservation),
		stoppingLoops: make(map[string]int),
		stateChanged:  make(chan struct{}),
	}
}

func NewActiveExecutionRegistry() *ExecutionSupervisor { return NewExecutionSupervisor() }

// ExecutionReservation is the Supervisor's ownership of work admitted for one
// loop. Its context is cancelled before daemon shutdown waits for the work to
// release ownership.
type ExecutionReservation struct {
	supervisor *ExecutionSupervisor
	id         uint64
	loopID     string
	ctx        context.Context
	cancel     context.CancelCauseFunc
	once       sync.Once
}

func (r *ExecutionReservation) Context() context.Context {
	if r == nil || r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// BindLoop attaches a pre-claim reservation to the loop selected by the
// durable queue claim. A concurrent stop cannot leave the claim unowned: the
// reservation remains its owner and is cancelled immediately.
func (r *ExecutionReservation) BindLoop(loopID string) {
	if r == nil || r.supervisor == nil {
		return
	}
	supervisor := r.supervisor
	supervisor.mu.Lock()
	if supervisor.reservations[r.id] != r {
		supervisor.mu.Unlock()
		return
	}
	r.loopID = loopID
	closing := supervisor.admissionClosed
	stopping := supervisor.stoppingLoops[loopID] > 0
	supervisor.notifyStateChangedLocked()
	supervisor.mu.Unlock()
	if closing {
		r.cancel(ErrExecutionAdmissionClosed)
	} else if stopping {
		r.cancel(ErrExecutionLoopStopping)
	}
}

func (r *ExecutionReservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.cancel(nil)
		r.supervisor.releaseReservation(r.id)
	})
}

// Reserve admits work before it acquires durable queue ownership.
func (r *ActiveExecutionRegistry) Reserve(loopID string) (*ExecutionReservation, error) {
	if r == nil {
		return nil, ErrExecutionAdmissionClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionClosed {
		if r.degradedErr != nil {
			return nil, errors.Join(ErrExecutionSupervisorDegraded, r.degradedErr)
		}
		return nil, ErrExecutionAdmissionClosed
	}
	if r.stoppingLoops[loopID] > 0 {
		return nil, ErrExecutionLoopStopping
	}
	r.nextReservation++
	ctx, cancel := context.WithCancelCause(context.Background())
	reservation := &ExecutionReservation{supervisor: r, id: r.nextReservation, loopID: loopID, ctx: ctx, cancel: cancel}
	r.reservations[reservation.id] = reservation
	r.notifyStateChangedLocked()
	return reservation, nil
}

// MarkDegraded closes admission after an infrastructure failure. Existing
// reservations are cancelled so no new durable work can overlap storage or
// ownership state that the daemon failed to publish.
func (r *ExecutionSupervisor) MarkDegraded(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	if r.degradedErr == nil {
		r.degradedErr = err
	}
	r.admissionClosed = true
	reservations := make([]*ExecutionReservation, 0, len(r.reservations))
	for _, reservation := range r.reservations {
		reservations = append(reservations, reservation)
	}
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	for _, reservation := range reservations {
		reservation.cancel(errors.Join(ErrExecutionSupervisorDegraded, err))
	}
}

func (r *ExecutionSupervisor) Failure() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.degradedErr
}

// BeginLoopStop closes admission for one loop and cancels every reservation
// already owned by that loop. The returned release function reopens admission
// after the caller has completed the durable stop transition.
func (r *ActiveExecutionRegistry) BeginLoopStop(loopID, reason string) func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.stoppingLoops[loopID]++
	reservations := make([]*ExecutionReservation, 0)
	for _, reservation := range r.reservations {
		if reservation.loopID == loopID {
			reservations = append(reservations, reservation)
		}
	}
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	for _, reservation := range reservations {
		reservation.cancel(errors.New(reason))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.stoppingLoops[loopID] <= 1 {
				delete(r.stoppingLoops, loopID)
			} else {
				r.stoppingLoops[loopID]--
			}
			r.notifyStateChangedLocked()
			r.mu.Unlock()
		})
	}
}

// BeginShutdown atomically closes admission, then cancels every admitted
// reservation and asks every registered live execution to stop.
func (r *ActiveExecutionRegistry) BeginShutdown(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.admissionClosed = true
	r.shutdownReason = reason
	reservations := make([]*ExecutionReservation, 0, len(r.reservations))
	for _, reservation := range r.reservations {
		reservations = append(reservations, reservation)
	}
	executions := make([]activeExecution, 0, len(r.executions))
	for _, entry := range r.executions {
		executions = append(executions, entry.execution)
	}
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	for _, reservation := range reservations {
		reservation.cancel(errors.New(reason))
	}
	for _, execution := range executions {
		_ = execution.Kill(reason)
	}
}

// Wait returns only after all admitted reservations and registered live
// executions release ownership.
func (r *ActiveExecutionRegistry) Wait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	for {
		r.mu.Lock()
		if len(r.reservations) == 0 && len(r.executions) == 0 {
			r.mu.Unlock()
			return nil
		}
		changed := r.stateChanged
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (r *ActiveExecutionRegistry) releaseReservation(id uint64) {
	r.mu.Lock()
	if _, ok := r.reservations[id]; ok {
		delete(r.reservations, id)
		r.notifyStateChangedLocked()
	}
	r.mu.Unlock()
}

func (r *ActiveExecutionRegistry) notifyStateChangedLocked() {
	close(r.stateChanged)
	r.stateChanged = make(chan struct{})
}

func (r *ActiveExecutionRegistry) Register(loopID, runID, executionID string, execution activeExecution) func() {
	if r == nil || execution == nil {
		return func() {}
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	if r.admissionClosed || r.stoppingLoops[loopID] > 0 {
		reason := r.shutdownReason
		if reason == "" {
			reason = "execution admission is closed"
		}
		if r.stoppingLoops[loopID] > 0 {
			reason = "loop is stopping"
		}
		r.mu.Unlock()
		_ = execution.Kill(reason)
		return func() {}
	}
	r.executions[key] = activeExecutionEntry{loopID: loopID, execution: execution}
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if entry, ok := r.executions[key]; ok && entry.execution == execution {
			delete(r.executions, key)
			r.notifyStateChangedLocked()
		}
		r.mu.Unlock()
	}
}

func (r *ActiveExecutionRegistry) Kill(loopID, runID, executionID, reason string) (bool, error) {
	if r == nil {
		return false, nil
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	entry := r.executions[key]
	r.mu.Unlock()
	if entry.execution == nil {
		return false, nil
	}
	return true, entry.execution.Kill(reason)
}

func activeExecutionKey(loopID, runID, executionID string) string {
	return loopID + "\x00" + runID + "\x00" + executionID
}
