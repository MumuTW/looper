package runtime

import (
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

// LockLoopRequeue acquires the process-wide per-loop requeue mutex shared by
// API discard+retry and runtime requeue paths. See loops.LockLoopRequeue.
// Call order with LockLoopTarget: take the per-loop lock first, then the
// target lock.
func LockLoopRequeue(loopID string) func() {
	return loops.LockLoopRequeue(loopID)
}

// LoopTargetGuardKey builds the process-wide target mutex key. See
// loops.LoopTargetGuardKey.
func LoopTargetGuardKey(projectID, loopType, targetType, targetKey string) string {
	return loops.LoopTargetGuardKey(projectID, loopType, targetType, targetKey)
}

// LoopTargetGuardKeyFromRecord derives LoopTargetGuardKey from a stored loop.
// See loops.LoopTargetGuardKeyFromRecord.
func LoopTargetGuardKeyFromRecord(loop storage.LoopRecord) string {
	return loops.LoopTargetGuardKeyFromRecord(loop)
}

// PullRequestTargetKey builds the canonical pull-request target key shared by
// runtime and API guard keys. See loops.PullRequestTargetKey.
func PullRequestTargetKey(repo string, prNumber int64) string {
	return loops.PullRequestTargetKey(repo, prNumber)
}

// LockLoopTarget acquires the process-wide same-target mutex. See
// loops.LockLoopTarget.
func LockLoopTarget(key string) func() {
	return loops.LockLoopTarget(key)
}
