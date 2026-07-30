package runtime

import (
	"strings"
	"sync"
)

// deployScheduler is the daemon-local single-flight for deploy commands. It
// deliberately has no persisted state: GitHub Deployments remain the record of
// what ran, while this guard only prevents overlapping scheduler ticks in this
// one looperd process from observing the same missing record and both starting
// the command.
type deployScheduler struct {
	mu      sync.Mutex
	running map[string]struct{}
}

func newDeployScheduler() *deployScheduler {
	return &deployScheduler{running: make(map[string]struct{})}
}

// Schedule admits fn once for projectID and hands it to the scheduler's
// lifecycle-owned async runner. It returns false when the project is already
// running or no live lifecycle owner exists.
func (s *deployScheduler) Schedule(projectID string, runner schedulerAsyncRunner, fn func()) bool {
	projectID = strings.TrimSpace(projectID)
	if s == nil || projectID == "" || runner == nil || fn == nil {
		return false
	}
	s.mu.Lock()
	if _, exists := s.running[projectID]; exists {
		s.mu.Unlock()
		return false
	}
	s.running[projectID] = struct{}{}
	s.mu.Unlock()

	runner.Go(func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, projectID)
			s.mu.Unlock()
		}()
		fn()
	})
	return true
}
