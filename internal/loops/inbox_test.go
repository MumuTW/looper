package loops

import "testing"

func TestHumanInboxAppendReadClearCap(t *testing.T) {
	base := `{"worker":{"title":"x"}}`
	m1, err := AppendHumanMessage(&base, HumanMessage{At: "t1", Text: "为什么推荐中文?"})
	if err != nil {
		t.Fatalf("append error = %v", err)
	}
	got := ReadHumanInbox(&m1)
	if len(got) != 1 || got[0].Text != "为什么推荐中文?" {
		t.Fatalf("ReadHumanInbox = %+v", got)
	}
	m2, _ := AppendHumanMessage(&m1, HumanMessage{At: "t2", Text: "那就用英文"})
	if len(ReadHumanInbox(&m2)) != 2 {
		t.Fatalf("want 2 messages")
	}
	// Cap.
	cur := m2
	for i := 0; i < humanInboxCap+5; i++ {
		cur, _ = AppendHumanMessage(&cur, HumanMessage{At: "t", Text: string(rune('a' + i))})
	}
	if n := len(ReadHumanInbox(&cur)); n != humanInboxCap {
		t.Fatalf("cap not enforced: %d want %d", n, humanInboxCap)
	}
	// Clear removes the key (and preserves others).
	cleared, _ := ClearHumanInbox(&cur)
	if len(ReadHumanInbox(&cleared)) != 0 {
		t.Fatal("inbox not cleared")
	}
}

func TestAcknowledgeHumanMessagesRemovesOnlyConsumed(t *testing.T) {
	t.Parallel()

	meta, err := AppendHumanMessage(nil, HumanMessage{At: "t1", Text: "first"})
	if err != nil {
		t.Fatalf("AppendHumanMessage(first) error = %v", err)
	}
	meta, err = AppendHumanMessage(&meta, HumanMessage{At: "t2", Text: "second"})
	if err != nil {
		t.Fatalf("AppendHumanMessage(second) error = %v", err)
	}
	consumed := ReadHumanInbox(&meta)

	// A third message arrives after the consumed snapshot was captured.
	meta, err = AppendHumanMessage(&meta, HumanMessage{At: "t3", Text: "mid-run arrival"})
	if err != nil {
		t.Fatalf("AppendHumanMessage(third) error = %v", err)
	}

	meta, err = AcknowledgeHumanMessages(&meta, consumed)
	if err != nil {
		t.Fatalf("AcknowledgeHumanMessages error = %v", err)
	}
	remaining := ReadHumanInbox(&meta)
	if len(remaining) != 1 || remaining[0].Text != "mid-run arrival" {
		t.Fatalf("remaining = %#v, want only the unobserved mid-run arrival", remaining)
	}

	// Acknowledging everything empties the inbox key.
	meta, err = AcknowledgeHumanMessages(&meta, remaining)
	if err != nil {
		t.Fatalf("AcknowledgeHumanMessages(rest) error = %v", err)
	}
	if msgs := ReadHumanInbox(&meta); msgs != nil {
		t.Fatalf("inbox after full acknowledgement = %#v, want empty", msgs)
	}

	// Duplicate messages acknowledge one occurrence each.
	meta, _ = AppendHumanMessage(nil, HumanMessage{At: "t4", Text: "dup"})
	meta, _ = AppendHumanMessage(&meta, HumanMessage{At: "t4", Text: "dup"})
	meta, err = AcknowledgeHumanMessages(&meta, []HumanMessage{{At: "t4", Text: "dup"}})
	if err != nil {
		t.Fatalf("AcknowledgeHumanMessages(dup) error = %v", err)
	}
	if msgs := ReadHumanInbox(&meta); len(msgs) != 1 {
		t.Fatalf("inbox after single-dup acknowledgement = %#v, want one remaining duplicate", msgs)
	}
}
