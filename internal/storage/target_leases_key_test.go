package storage

import "testing"

func TestTargetLeaseKeyFromQueueMatchesLoop(t *testing.T) {
	projectID := "project_1"
	loopID := "loop_1"
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pull_request:acme/looper:42"
	loop := LoopRecord{ID: loopID, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber}
	item := QueueItemRecord{ID: "queue_1", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber}
	if got, want := TargetLeaseKeyFromQueue(item), TargetLeaseKeyFromLoop(loop); got != want {
		t.Fatalf("TargetLeaseKeyFromQueue() = %q, want %q", got, want)
	}
}
