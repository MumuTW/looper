package runtime

import (
	"context"
	"testing"
)

func TestPollFeishuHITLInboxOnce(t *testing.T) {
	// feishu_threads: root om_root_1 -> loop-a (this looper); om_other -> "" (another looper).
	rootToLoop := map[string]string{"om_root_1": "loop-a"}
	seqToLoop := map[int64]string{71: "loop-seq71"}
	var answers, messages []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, root string) string { return rootToLoop[root] },
		loopBySeq:  func(_ contextType, seq int64) string { return seqToLoop[seq] },
		deliverAnswer: func(_ contextType, loopID, answer, _, _ string) error {
			answers = append(answers, loopID+"="+answer)
			return nil
		},
		enqueueMessage: func(_ contextType, loopID, text string) error {
			messages = append(messages, loopID+"="+text)
			return nil
		},
	}
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root_1", Text: "用 A,改 resize handle"}, // typed -> enqueue
		{ID: 11, Kind: "message", RootID: "om_other", Text: "not ours"},             // another looper -> skip
		{ID: 12, Kind: "message", RootID: "om_root_1", Text: "   "},                 // empty -> skip
		mustCardAction(15, "71", "redis"),                                           // button -> deliver by seq
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 2 {
		t.Fatalf("handled = %d, want 2", n)
	}
	if maxID != 15 {
		t.Fatalf("maxID = %d, want 15", maxID)
	}
	// A typed reply is queued (conversational), a button click is a decision.
	if len(messages) != 1 || messages[0] != "loop-a=用 A,改 resize handle" {
		t.Fatalf("enqueued messages = %v, want the typed reply", messages)
	}
	if len(answers) != 1 || answers[0] != "loop-seq71=redis" {
		t.Fatalf("delivered answers = %v, want the button click", answers)
	}
}

func mustCardAction(id int64, seq, answer string) feishuInboxEvent {
	e := feishuInboxEvent{ID: id, Kind: "card_action"}
	e.Value.LoopSeq = seq
	e.Value.Answer = answer
	// Feishu card actions require generation tokens (API route + poll lane).
	e.Value.ExecutionID = "agent-poll"
	e.Value.AskedAt = "2026-04-11T12:00:00.000Z"
	return e
}

func TestPollFeishuHITLInboxOnce_RejectsMissingGenerationTokens(t *testing.T) {
	t.Parallel()
	var delivered []string
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string {
			if seq == 71 {
				return "loop-71"
			}
			return ""
		},
		deliverAnswer: func(_ contextType, loopID, answer, _, _ string) error {
			delivered = append(delivered, loopID+":"+answer)
			return nil
		},
	}
	// Pre-upgrade card: loopSeq+answer only.
	legacy := feishuInboxEvent{ID: 1, Kind: "card_action"}
	legacy.Value.LoopSeq = "71"
	legacy.Value.Answer = "stale-option"
	n, _ := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{legacy}, deps)
	if n != 0 || len(delivered) != 0 {
		t.Fatalf("tokenless card delivered=%v count=%d, want none", delivered, n)
	}
	// Partial tokens still rejected (both required).
	partial := mustCardAction(2, "71", "partial")
	partial.Value.AskedAt = ""
	n, _ = pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{partial}, deps)
	if n != 0 || len(delivered) != 0 {
		t.Fatalf("partial-token card delivered=%v count=%d, want none", delivered, n)
	}
}
