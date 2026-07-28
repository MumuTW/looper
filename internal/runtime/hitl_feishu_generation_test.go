package runtime

import (
	"context"
	"testing"
)

// Stale Feishu card actions (prior ask generation) must not deliver answers.
func TestPollFeishuHITLInboxOnce_RejectsStaleAskGeneration(t *testing.T) {
	t.Parallel()
	var delivered []string
	deps := feishuHITLPollDeps{
		loopBySeq: func(ctx contextType, seq int64) string {
			if seq == 71 {
				return "loop-71"
			}
			return ""
		},
		loopAskGeneration: func(ctx contextType, loopID string) (executionID, askedAt string, ok bool) {
			return "exec-new", "2026-07-28T02:00:00Z", true
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+":"+answer)
			return nil
		},
	}

	stale := mustCardAction(1, "71", "old-option")
	stale.Value.ExecutionID = "exec-old"
	stale.Value.AskedAt = "2026-07-28T01:00:00Z"
	n, _ := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{stale}, deps)
	if n != 0 || len(delivered) != 0 {
		t.Fatalf("stale generation delivered=%v count=%d, want none", delivered, n)
	}

	fresh := mustCardAction(2, "71", "new-option")
	fresh.Value.ExecutionID = "exec-new"
	fresh.Value.AskedAt = "2026-07-28T02:00:00Z"
	n, _ = pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{fresh}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != "loop-71:new-option" {
		t.Fatalf("matching generation delivered=%v count=%d", delivered, n)
	}
}
