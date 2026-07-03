package runtime

import (
	"context"
	"testing"
)

func TestPollFeishuHITLInboxOnce(t *testing.T) {
	// feishu_threads: root om_root_1 -> loop-a (this looper); om_other -> "" (another looper).
	rootToLoop := map[string]string{"om_root_1": "loop-a"}
	seqToLoop := map[int64]string{71: "loop-seq71"}
	var delivered []string
	deps := feishuHITLPollDeps{
		loopByRoot:    func(_ contextType, root string) string { return rootToLoop[root] },
		loopBySeq:     func(_ contextType, seq int64) string { return seqToLoop[seq] },
		deliverAnswer: func(_ contextType, loopID, answer string) error { delivered = append(delivered, loopID+"="+answer); return nil },
	}
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root_1", Text: "用 A,改 resize handle"}, // ours -> deliver
		{ID: 11, Kind: "message", RootID: "om_other", Text: "not ours"},              // another looper -> skip
		{ID: 12, Kind: "message", RootID: "om_root_1", Text: "   "},                  // empty -> skip
		mustCardAction(15, "71", "redis"),                                            // card action -> deliver by seq
	}
	n, maxID := pollFeishuHITLInboxOnce(context.Background(), events, deps)
	if n != 2 {
		t.Fatalf("delivered = %d, want 2", n)
	}
	if maxID != 15 {
		t.Fatalf("maxID = %d, want 15", maxID)
	}
	want := map[string]bool{"loop-a=用 A,改 resize handle": true, "loop-seq71=redis": true}
	for _, d := range delivered {
		if !want[d] {
			t.Fatalf("unexpected delivery %q; got %v", d, delivered)
		}
	}
}

func mustCardAction(id int64, seq, answer string) feishuInboxEvent {
	e := feishuInboxEvent{ID: id, Kind: "card_action"}
	e.Value.LoopSeq = seq
	e.Value.Answer = answer
	return e
}
