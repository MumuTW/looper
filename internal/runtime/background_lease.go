package runtime

import (
	"context"
	"errors"
	"sync"
)

// BackgroundOperationMeta identifies a Supervisor-owned operation that is not
// a durable queue claim but still performs work that must finish before storage
// closes (for example, an operator-triggered Coordinator backfill).
type BackgroundOperationMeta struct {
	Name string
}

// BackgroundOperation is the ownership token for a live non-agent operation.
// Its Context is canceled when shutdown begins; callers must Release after the
// operation has stopped so DrainSnapshot can reach zero.
type BackgroundOperation interface {
	Context() context.Context
	Release()
}

type backgroundOperation struct {
	registry *ActiveExecutionRegistry
	id       uint64
	meta     BackgroundOperationMeta
	ctx      context.Context
	cancel   context.CancelCauseFunc

	mu       sync.Mutex
	released bool
	done     chan struct{}
	once     sync.Once
}

func (o *backgroundOperation) Context() context.Context {
	if o == nil || o.ctx == nil {
		return context.Background()
	}
	return o.ctx
}

func (o *backgroundOperation) Release() {
	if o == nil {
		return
	}
	r := o.registry
	if r != nil {
		r.mu.Lock()
		o.mu.Lock()
		if o.released {
			o.mu.Unlock()
			r.mu.Unlock()
			return
		}
		o.released = true
		o.mu.Unlock()
		delete(r.backgroundOps, o.id)
		r.mu.Unlock()
	} else {
		o.mu.Lock()
		if o.released {
			o.mu.Unlock()
			return
		}
		o.released = true
		o.mu.Unlock()
	}
	if o.cancel != nil {
		o.cancel(nil)
	}
	o.once.Do(func() { close(o.done) })
}

// AdmitBackground acquires Supervisor ownership for one non-agent operation.
// It shares the daemon's AllowClaim authority, so a drain refuses new
// backfills, while an already-admitted operation remains visible to
// DrainSnapshot and receives shutdown cancellation.
func (r *ActiveExecutionRegistry) AdmitBackground(ctx context.Context, meta BackgroundOperationMeta) (BackgroundOperation, error) {
	if r == nil {
		return nil, ErrOperationAdmissionClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	allow := r.allowSpawn
	closed := r.admissionClosed
	r.mu.Unlock()
	if allow != nil {
		if err := allow(); err != nil {
			return nil, errors.Join(ErrOperationAdmissionClosed, err)
		}
	}
	if closed {
		return nil, ErrOperationAdmissionClosed
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionClosed {
		return nil, ErrOperationAdmissionClosed
	}
	if r.backgroundOps == nil {
		r.backgroundOps = make(map[uint64]*backgroundOperation)
	}
	r.nextBackgroundID++
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &backgroundOperation{
		registry: r,
		id:       r.nextBackgroundID,
		meta:     meta,
		ctx:      leaseCtx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	r.backgroundOps[lease.id] = lease
	return lease, nil
}

// cancelBackgroundOperationsLocked cancels every live non-agent operation and
// returns completion channels. Caller must hold r.mu.
func (r *ActiveExecutionRegistry) cancelBackgroundOperationsLocked(cause error) []<-chan struct{} {
	if r == nil {
		return nil
	}
	wait := make([]<-chan struct{}, 0, len(r.backgroundOps))
	for _, operation := range r.backgroundOps {
		if operation == nil {
			continue
		}
		if cause != nil {
			operation.cancel(cause)
		}
		if operation.done != nil {
			wait = append(wait, operation.done)
		}
	}
	return wait
}

// CancelBackgroundOperations cancels already-admitted non-agent operations
// without waiting for their owner callbacks. Runtime.MarkDegraded uses this
// alongside scheduler/cleanup cancellation so a live backfill cannot continue
// LLM or GitHub work after the single admission Authority becomes degraded.
func (r *ActiveExecutionRegistry) CancelBackgroundOperations(cause error) {
	if r == nil {
		return
	}
	if cause == nil {
		cause = ErrOperationAdmissionClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelBackgroundOperationsLocked(cause)
}
