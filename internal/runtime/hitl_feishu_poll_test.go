package runtime

import (
	"context"
	"fmt"
	"testing"
)

func TestPollFeishuHITLInboxOnceAllSucceed(t *testing.T) {
	rootToLoop := map[string]string{"om_root_1": "loop-a"}
	seqToLoop := map[int64]string{71: "loop-seq71"}
	var answers, messages []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, root string) string { return rootToLoop[root] },
		loopBySeq:  func(_ contextType, seq int64) string { return seqToLoop[seq] },
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			answers = append(answers, loopID+"="+answer)
			return nil
		},
		enqueueMessage: func(_ contextType, loopID, text string) error {
			messages = append(messages, loopID+"="+text)
			return nil
		},
	}
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root_1", Text: "用 A"},
		{ID: 11, Kind: "message", RootID: "om_other", Text: "not ours"},
		{ID: 12, Kind: "message", RootID: "om_root_1", Text: "   "},
		makeCardAction(15, "71", "redis"),
	}
	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, deps, 0)
	if n != 2 {
		t.Fatalf("handled = %d, want 2", n)
	}
	if newCursor != 15 {
		t.Fatalf("newCursor = %d, want 15", newCursor)
	}
	if len(messages) != 1 || messages[0] != "loop-a=用 A" {
		t.Fatalf("enqueued messages = %v", messages)
	}
	if len(answers) != 1 || answers[0] != "loop-seq71=redis" {
		t.Fatalf("delivered answers = %v", answers)
	}
}

func TestPollFeishuHITLInboxCursorBlocksOnFailedDelivery(t *testing.T) {
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string { return "loop-seq" },
	}
	events := []feishuInboxEvent{
		makeCardAction(9, "1", "ok"),
		makeCardAction(10, "1", "ok"),
		makeCardAction(11, "1", "ok"),
		makeCardAction(12, "1", "ok"),
	}

	var callCount int
	deps.deliverAnswer = func(_ contextType, loopID, answer string) error {
		callCount++
		if callCount == 2 {
			return fmt.Errorf("temporary failure")
		}
		return nil
	}

	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, deps, 0)
	if n != 1 {
		t.Fatalf("handled = %d, want 1 before the failed event", n)
	}
	if newCursor != 9 {
		t.Fatalf("newCursor = %d, want 9 (blocked on failed event 10)", newCursor)
	}
	if callCount != 2 {
		t.Fatalf("delivery attempts = %d, want 2 (do not apply events after failure)", callCount)
	}
}

func TestPollFeishuHITLInboxCursorConsumesUnappliedAnswerAndContinues(t *testing.T) {
	callbackAttempts := 0
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, _ int64) string { return "loop-seq" },
		deliverAnswer: func(_ contextType, _, _ string) error {
			callbackAttempts++
			if callbackAttempts == 1 {
				return errHITLAnswerNotApplied
			}
			return nil
		},
	}

	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), []feishuInboxEvent{
		makeCardAction(9, "1", "ok"),
		makeCardAction(10, "1", "later"),
	}, deps, 0)
	if n != 1 || newCursor != 10 || callbackAttempts != 2 {
		t.Fatalf("unapplied answer = (handled:%d, cursor:%d, attempts:%d), want (1, 10, 2)", n, newCursor, callbackAttempts)
	}
}

func TestPollFeishuHITLInboxCursorAdvancesPastSkippedEvents(t *testing.T) {
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root", Text: "   "},
		makeCardAction(11, "not-a-sequence", "yes"),
		{ID: 12, Kind: "unsupported"},
	}

	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, feishuHITLPollDeps{}, 9)
	if n != 0 {
		t.Fatalf("handled = %d, want 0", n)
	}
	if newCursor != 12 {
		t.Fatalf("newCursor = %d, want 12 after skipped events", newCursor)
	}
}

func makeCardAction(id int64, seq, answer string) feishuInboxEvent {
	return feishuInboxEvent{
		ID:   id,
		Kind: "card_action",
		Value: struct {
			LoopSeq string `json:"loopSeq"`
			Answer  string `json:"answer"`
		}{LoopSeq: seq, Answer: answer},
	}
}
