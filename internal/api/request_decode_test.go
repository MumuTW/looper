package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	pkgapi "github.com/MumuTW/looper/pkg/api"
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

	t.Run("oversized body is rejected with 413 and its canonical code", func(t *testing.T) {
		var dst decodeProbe
		huge := `{"name":"` + strings.Repeat("x", maxJSONMutationBodyBytes) + `"}`
		aerr := decodeJSONMutationBody(newRequest(huge), &dst, true)
		if aerr == nil || aerr.status != http.StatusRequestEntityTooLarge || aerr.code != pkgapi.ErrorCodeRequestTooLarge {
			t.Fatalf("decode error = %#v, want 413 REQUEST_TOO_LARGE", aerr)
		}
	})

	t.Run("duplicate member names are rejected", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"force":true,"force":false}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "duplicate field") {
			t.Fatalf("decode error = %#v, want duplicate-field rejection", aerr)
		}
	})

	t.Run("lone noncanonical casing is rejected", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"Force":true}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, `canonical spelling "force"`) {
			t.Fatalf("decode error = %#v, want canonical-casing rejection naming the exact field", aerr)
		}
	})

	t.Run("unicode-fold duplicate is rejected", func(t *testing.T) {
		// encoding/json matches with Unicode simple folding: U+017F LATIN
		// SMALL LETTER LONG S folds to "s", so both members hit the same field.
		var dst struct {
			Status string `json:"status"`
		}
		aerr := decodeJSONMutationBody(newRequest(`{"status":"paused","ſtatus":"running"}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "duplicate field") {
			t.Fatalf("decode error = %#v, want unicode-fold duplicate rejection", aerr)
		}
	})

	t.Run("nested noncanonical casing is rejected", func(t *testing.T) {
		var dst struct {
			Inner decodeProbe `json:"inner"`
		}
		aerr := decodeJSONMutationBody(newRequest(`{"inner":{"NAME":"a"}}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, `canonical spelling "name"`) {
			t.Fatalf("decode error = %#v, want nested canonical-casing rejection", aerr)
		}
	})

	t.Run("case-variant duplicate aliases are rejected", func(t *testing.T) {
		// encoding/json matches struct fields case-insensitively, so a
		// case-variant alias would silently let the last spelling win.
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"force":true,"Force":false}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "duplicate field") {
			t.Fatalf("decode error = %#v, want case-folded duplicate rejection", aerr)
		}
	})

	t.Run("same name in sibling objects is legal", func(t *testing.T) {
		var dst struct {
			A map[string]any `json:"a"`
			B map[string]any `json:"b"`
		}
		if aerr := decodeJSONMutationBody(newRequest(`{"a":{"name":"x"},"b":{"name":"y"}}`), &dst, true); aerr != nil {
			t.Fatalf("decode error = %v, want same names in different objects accepted", aerr)
		}
	})

	t.Run("free-form metadata retains its key semantics", func(t *testing.T) {
		var dst createLoopRequest
		payload := `{"metadata":{"id":1,"ID":2,"":1,"":2}}`
		if aerr := decodeJSONMutationBody(newRequest(payload), &dst, true); aerr != nil {
			t.Fatalf("decode error = %v, want free-form metadata accepted", aerr)
		}
		if string(dst.Metadata) != `{"id":1,"ID":2,"":1,"":2}` {
			t.Fatalf("metadata = %s, want original free-form object", dst.Metadata)
		}
	})

	t.Run("large free-form metadata is accepted without field-by-field comparison", func(t *testing.T) {
		const members = 40000
		var body strings.Builder
		body.Grow(members * 16)
		body.WriteString(`{"metadata":{`)
		for i := 0; i < members; i++ {
			if i > 0 {
				body.WriteByte(',')
			}
			body.WriteString(`"key`)
			body.WriteString(strconv.Itoa(i))
			body.WriteString(`":true`)
		}
		body.WriteString(`}}`)
		var dst createLoopRequest
		if aerr := decodeJSONMutationBody(newRequest(body.String()), &dst, true); aerr != nil {
			t.Fatalf("decode error = %v, want large free-form metadata accepted", aerr)
		}
	})

	t.Run("empty body required", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(""), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "required") {
			t.Fatalf("decode error = %#v, want body-required rejection", aerr)
		}
	})

	t.Run("non-canonical field casing is rejected", func(t *testing.T) {
		var dst decodeProbe
		aerr := decodeJSONMutationBody(newRequest(`{"Force":true}`), &dst, true)
		if aerr == nil || !strings.Contains(aerr.message, "canonical spelling") {
			t.Fatalf("decode error = %#v, want non-canonical field casing rejection", aerr)
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

	bodyRead := make(chan error, 1)
	timeoutServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		bodyRead <- err
	}))
	timeoutServer.Config = (&Server{handler: timeoutServer.Config.Handler}).newHTTPServer()
	timeoutServer.Config.ReadTimeout = 100 * time.Millisecond
	timeoutServer.Start()
	defer timeoutServer.Close()

	conn, err := net.Dial("tcp", timeoutServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial slow-body test server: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 10\r\n\r\n{"); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	select {
	case err := <-bodyRead:
		if err == nil {
			t.Fatal("slow body read completed without a timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("server did not terminate a stalled body within the read timeout window")
	}
}
