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

func TestClearHumanInboxMessagesAcknowledgesOnlyDrainedIDs(t *testing.T) {
	base := `{"worker":{"title":"x"}}`
	m, _ := AppendHumanMessage(&base, HumanMessage{ID: "drained-1", At: "t1", Text: "m1"})
	m, _ = AppendHumanMessage(&m, HumanMessage{ID: "drained-2", At: "t2", Text: "m2"})
	drained := ReadHumanInbox(&m)

	// Fill past the cap while the agent is running. The drained messages are
	// evicted, but the just-arrived messages must not be acknowledged by count.
	for i := 0; i < humanInboxCap+1; i++ {
		m, _ = AppendHumanMessage(&m, HumanMessage{ID: string(rune('a' + i)), At: "late", Text: string(rune('a' + i))})
	}
	m2, err := ClearHumanInboxMessages(&m, drained)
	if err != nil {
		t.Fatalf("ClearHumanInboxMessages error = %v", err)
	}
	got := ReadHumanInbox(&m2)
	if len(got) != humanInboxCap || got[0].Text != "b" {
		t.Fatalf("after acknowledgement: %+v, want all capped late messages", got)
	}

	unchanged, err := ClearHumanInboxMessages(&m2, nil)
	if err != nil || unchanged != m2 {
		t.Fatalf("zero-drain acknowledgement = (%q, %v), want unchanged metadata", unchanged, err)
	}
}
