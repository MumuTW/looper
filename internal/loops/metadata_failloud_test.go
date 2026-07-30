package loops

import (
	"errors"
	"strings"
	"testing"
)

func TestMetadataWritersFailLoudOnMalformedMetadata(t *testing.T) {
	t.Parallel()

	writers := []struct {
		name   string
		mutate func(metadataJSON *string) (string, error)
	}{
		{name: "WriteHITLAsk", mutate: func(m *string) (string, error) {
			return WriteHITLAsk(m, HITLAsk{Question: "q", Status: "awaiting"})
		}},
		{name: "ClearHITLAsk", mutate: ClearHITLAsk},
		{name: "AppendHumanMessage", mutate: func(m *string) (string, error) {
			return AppendHumanMessage(m, HumanMessage{At: "2026-04-11T12:00:00.000Z", Text: "hello"})
		}},
		{name: "ClearHumanInbox", mutate: ClearHumanInbox},
		{name: "WriteTakeoverResume", mutate: func(m *string) (string, error) {
			return WriteTakeoverResume(m, TakeoverResume{SessionID: "s1"})
		}},
		{name: "ClearTakeoverResume", mutate: ClearTakeoverResume},
		{name: "AppendMilestone", mutate: func(m *string) (string, error) {
			return AppendMilestone(m, Milestone{At: "2026-04-11T12:00:00.000Z", Text: "opened PR"})
		}},
	}

	malformed := []struct {
		name  string
		value string
	}{
		{name: "truncated object", value: `{"worktree":"/tmp/wt","hitl":{`},
		{name: "not an object", value: `["worktree"]`},
		{name: "json null", value: `null`},
		{name: "plain text", value: `corrupted`},
		{name: "invalid UTF-8", value: string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})},
	}

	for _, w := range writers {
		for _, m := range malformed {
			value := m.value
			got, err := w.mutate(&value)
			if err == nil {
				t.Fatalf("%s(%s) error = nil (returned %q), want ErrMalformedLoopMetadata: a malformed value must block mutation", w.name, m.name, got)
			}
			if !errors.Is(err, ErrMalformedLoopMetadata) {
				t.Fatalf("%s(%s) error = %v, want errors.Is ErrMalformedLoopMetadata", w.name, m.name, err)
			}
			if got != "" {
				t.Fatalf("%s(%s) = %q, want empty output so the stored value is preserved", w.name, m.name, got)
			}
			if !strings.Contains(err.Error(), value) {
				t.Fatalf("%s(%s) error = %q, want it to carry the original value for diagnosis", w.name, m.name, err)
			}
		}
	}
}

func TestMetadataWritersStillAcceptEmptyAndValidMetadata(t *testing.T) {
	t.Parallel()

	valid := `{"worktree":"/tmp/wt"}`
	out, err := WriteHITLAsk(&valid, HITLAsk{Question: "q", Status: "awaiting"})
	if err != nil {
		t.Fatalf("WriteHITLAsk(valid) error = %v", err)
	}
	if !strings.Contains(out, `"worktree":"/tmp/wt"`) {
		t.Fatalf("WriteHITLAsk(valid) = %q, want other keys preserved", out)
	}

	if _, err := AppendMilestone(nil, Milestone{At: "2026-04-11T12:00:00.000Z", Text: "start"}); err != nil {
		t.Fatalf("AppendMilestone(nil) error = %v", err)
	}
	empty := "   "
	if _, err := ClearHumanInbox(&empty); err != nil {
		t.Fatalf("ClearHumanInbox(blank) error = %v", err)
	}
}

func TestMetadataReadersStayLenientOnMalformedMetadata(t *testing.T) {
	t.Parallel()

	// Reads never destroy state, so malformed metadata reads as absent instead
	// of failing HITL/status surfaces that only display it.
	malformed := `{"hitl":`
	if _, ok := ReadHITLAsk(&malformed); ok {
		t.Fatal("ReadHITLAsk(malformed) ok = true, want false")
	}
	if msgs := ReadHumanInbox(&malformed); msgs != nil {
		t.Fatalf("ReadHumanInbox(malformed) = %v, want nil", msgs)
	}
	if _, ok := ReadTakeoverResume(&malformed); ok {
		t.Fatal("ReadTakeoverResume(malformed) ok = true, want false")
	}
	if milestones := ReadMilestones(&malformed); milestones != nil {
		t.Fatalf("ReadMilestones(malformed) = %v, want nil", milestones)
	}
}
