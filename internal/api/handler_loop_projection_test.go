package api

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

func TestLogsProjectionRejectsMissingStorage(t *testing.T) {
	h := NewHandler(Context{})
	_, _, err := h.buildLogsStateForRun(context.Background(), storage.LoopRecord{ID: "loop_missing_storage"}, nil, false)
	if err == nil {
		t.Fatal("buildLogsStateForRun() error = nil, want storage configuration error")
	}
	var typed apiError
	if !asAPIError(err, &typed) || typed.status != 500 || typed.message != "Storage is not configured" {
		t.Fatalf("error = %#v, want HTTP 500 Storage is not configured", err)
	}
}
