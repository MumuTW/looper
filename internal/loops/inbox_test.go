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

func TestClearHumanInboxNPreservesNewMessages(t *testing.T) {
	base := `{"worker":{"title":"x"}}`
	// Seed with 3 messages.
	m, _ := AppendHumanMessage(&base, HumanMessage{At: "t1", Text: "m1"})
	m, _ = AppendHumanMessage(&m, HumanMessage{At: "t2", Text: "m2"})
	m, _ = AppendHumanMessage(&m, HumanMessage{At: "t3", Text: "m3"})

	// Clear first 2 ("drained into prompt").
	m2, err := ClearHumanInboxN(&m, 2)
	if err != nil {
		t.Fatalf("ClearHumanInboxN error = %v", err)
	}
	got := ReadHumanInbox(&m2)
	if len(got) != 1 || got[0].Text != "m3" {
		t.Fatalf("after clearing 2: %+v, want only m3", got)
	}

	// n larger than length clears all.
	m3, _ := ClearHumanInboxN(&m, 10)
	if len(ReadHumanInbox(&m3)) != 0 {
		t.Fatal("ClearHumanInboxN(10) on 3 messages should clear all")
	}

	// n=0 clears all (defensive).
	m4, _ := ClearHumanInboxN(&m, 0)
	if len(ReadHumanInbox(&m4)) != 0 {
		t.Fatal("ClearHumanInboxN(0) should clear all")
	}
}
