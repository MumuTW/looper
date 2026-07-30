package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// mustMarshalJSON is test-only shorthand. Production event and queue paths use
// marshalJSON and propagate errors instead of panicking.
func mustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestAppendSystemEventWithPayloadReturnsMarshalError(t *testing.T) {
	err := appendSystemEventWithPayload(context.Background(), nil, storage.EventLogRecord{
		EventType: "looperd.test.invalid_payload",
	}, map[string]any{
		"unsupported": func() {},
	})
	if err == nil {
		t.Fatal("expected unsupported payload to return an error")
	}
	if !strings.Contains(err.Error(), "marshal looperd.test.invalid_payload event payload") {
		t.Fatalf("unexpected error: %v", err)
	}
}
