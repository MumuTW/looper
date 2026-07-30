package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type decodeProbe struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

func TestDecodeJSONMutationBodyContract(t *testing.T) {
	t.Parallel()

	newRequest := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/v1/anything", strings.NewReader(body))
	}

	t.Run("valid object decodes", func(t *testing.T) {
		var dst decodeProbe
		if aerr := decodeJSONMutationBody(newRequest(`{"name":"a","force":true}`), &dst, true); aerr != nil {
			t.Fatalf("decode error = %v", aerr)
		}
		if dst.Name != "a" || !dst.Force {
			t.Fatalf("dst = %#v, want decoded fields", dst)
		}
	})

	t.Run("unknown field is a typed validation error", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"name":"a","forcee":true}`), &dst, true)
		if aerr == nil || aerr.status != http.StatusBadRequest || !strings.Contains(aerr.message, "unknown field") {
			t.Fatalf("decode error = %#v, want unknown-field validation error", aerr)
		}
	})

	t.Run("two JSON values are rejected", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"name":"a"}{"name":"b"}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "exactly one JSON value") {
			t.Fatalf("decode error = %#v, want exactly-one-value rejection", aerr)
		}
	})

	t.Run("trailing garbage is rejected", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"name":"a"} trailing`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "exactly one JSON value") {
			t.Fatalf("decode error = %#v, want exactly-one-value rejection", aerr)
		}
	})

	t.Run("incomplete JSON is rejected", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"name":"a"`), &dst, true)
		if aerr == nil || aerr.status != http.StatusBadRequest {
			t.Fatalf("decode error = %#v, want invalid-JSON rejection", aerr)
		}
	})

	t.Run("oversized body is rejected with 413", func(t *testing.T) {
		var dst decodeProbe
		huge := `{"name":"` + strings.Repeat("x", maxJSONMutationBodyBytes) + `"}`
		aerr := decodeJSONMutationBody(newRequest(huge), &dst, true)
		if aerr == nil || aerr.status != http.StatusRequestEntityTooLarge {
			t.Fatalf("decode error = %#v, want 413 too-large rejection", aerr)
		}
	})

	t.Run("empty body required", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(""), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "required") {
			t.Fatalf("decode error = %#v, want body-required rejection", aerr)
		}
	})

	t.Run("empty body optional decodes to zero value", func(t *testing.T) {
		var dst decodeProbe
		if aerr := decodeJSONMutationBody(newRequest("   "), &dst, false); aerr != nil {
			t.Fatalf("decode error = %v, want empty optional body accepted", aerr)
		}
		if dst != (decodeProbe{}) {
			t.Fatalf("dst = %#v, want zero value", dst)
		}
	})
}

func TestServerBoundsRequestReadTime(t *testing.T) {
	t.Parallel()

	// The shared boundary bounds body size per request; the server bounds body
	// read TIME. This pins the configuration so a stalled body cannot hold a
	// goroutine indefinitely.
	server := &Server{}
	httpServer := server.newHTTPServer()
	if httpServer.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout = %v, want a positive bound on request reads", httpServer.ReadTimeout)
	}
	if httpServer.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want a positive bound", httpServer.ReadHeaderTimeout)
	}
}
