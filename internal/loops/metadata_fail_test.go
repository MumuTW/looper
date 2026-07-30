package loops

import (
	"strings"
	"testing"
)

func TestParseMetadataObjectRejectsMalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"trailing garbage", `{"a":1}garbage`},
		{"invalid escape", `{"a":"\x"}`},
		{"truncated", `{"a":`},
		{"not json", `not-json-at-all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMetadataObject(&tc.raw)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "parse loop metadata") {
				t.Fatalf("error = %v, want 'parse loop metadata'", err)
			}
		})
	}
}

func TestParseMetadataObjectAcceptsEmptyAndNil(t *testing.T) {
	if m, err := parseMetadataObject(nil); err != nil || len(m) != 0 {
		t.Fatalf("nil: meta=%v err=%v", m, err)
	}
	if m, err := parseMetadataObject(rawptr("")); err != nil || len(m) != 0 {
		t.Fatalf("empty: meta=%v err=%v", m, err)
	}
	if m, err := parseMetadataObject(rawptr("  ")); err != nil || len(m) != 0 {
		t.Fatalf("spaces: meta=%v err=%v", m, err)
	}
}

func TestWriteHITLAskRejectsMalformedMetadata(t *testing.T) {
	malformed := `{"a":1}garbage`
	_, err := WriteHITLAsk(&malformed, HITLAsk{Question: "q"})
	if err == nil {
		t.Fatal("WriteHITLAsk should reject malformed metadata")
	}
}

func TestAppendHumanMessageRejectsMalformedMetadata(t *testing.T) {
	malformed := `{"a":1}garbage`
	_, err := AppendHumanMessage(&malformed, HumanMessage{At: "t", Text: "m"})
	if err == nil {
		t.Fatal("AppendHumanMessage should reject malformed metadata")
	}
}

func TestAppendMilestoneRejectsMalformedMetadata(t *testing.T) {
	malformed := `{"a":1}garbage`
	_, err := AppendMilestone(&malformed, Milestone{At: "t", Text: "m"})
	if err == nil {
		t.Fatal("AppendMilestone should reject malformed metadata")
	}
}

func TestWriteTakeoverResumeRejectsMalformedMetadata(t *testing.T) {
	malformed := `{"a":1}garbage`
	_, err := WriteTakeoverResume(&malformed, TakeoverResume{SessionID: "s"})
	if err == nil {
		t.Fatal("WriteTakeoverResume should reject malformed metadata")
	}
}

func TestClearTakeoverResumeRejectsMalformedMetadata(t *testing.T) {
	malformed := `{"a":1}garbage`
	_, err := ClearTakeoverResume(&malformed)
	if err == nil {
		t.Fatal("ClearTakeoverResume should reject malformed metadata")
	}
}

func rawptr(s string) *string { return &s }
