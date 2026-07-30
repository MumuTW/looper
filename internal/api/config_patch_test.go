package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func TestValidateConfigPatchRequest(t *testing.T) {
	tests := []struct {
		name  string
		patch ConfigPatchRequest
		want  []ConfigPatchIssue
	}{
		{
			name:  "empty patch",
			patch: ConfigPatchRequest{Revision: "sha256:test"},
			want: []ConfigPatchIssue{
				{Code: "empty_patch", Message: "At least one set or unset operation is required"},
			},
		},
		{
			name: "valid set only",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set:      map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage(`4`)},
			},
			want: []ConfigPatchIssue{},
		},
		{
			name: "valid unset only",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Unset:    []string{"agent.env.OLD"},
			},
			want: []ConfigPatchIssue{},
		},
		{
			name: "set path empty",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set:      map[string]json.RawMessage{"": json.RawMessage(`1`)},
			},
			want: []ConfigPatchIssue{
				{Path: "", Code: "invalid_path", Message: "Set paths must be non-empty and contain no surrounding whitespace"},
			},
		},
		{
			name: "set path surrounding whitespace",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set:      map[string]json.RawMessage{" scheduler.maxConcurrentRuns": json.RawMessage(`4`)},
			},
			want: []ConfigPatchIssue{
				{Path: " scheduler.maxConcurrentRuns", Code: "invalid_path", Message: "Set paths must be non-empty and contain no surrounding whitespace"},
			},
		},
		{
			name: "set missing value",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set:      map[string]json.RawMessage{"agent.env.KEY": json.RawMessage{}},
			},
			want: []ConfigPatchIssue{
				{Path: "agent.env.KEY", Code: "missing_value", Message: "Set operations require a JSON value"},
			},
		},
		{
			name: "set invalid path and missing value on same path sorted by code",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set:      map[string]json.RawMessage{" x": json.RawMessage{}},
			},
			want: []ConfigPatchIssue{
				{Path: " x", Code: "invalid_path", Message: "Set paths must be non-empty and contain no surrounding whitespace"},
				{Path: " x", Code: "missing_value", Message: "Set operations require a JSON value"},
			},
		},
		{
			name: "unset path whitespace",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Unset:    []string{"agent.env.KEY "},
			},
			want: []ConfigPatchIssue{
				{Path: "agent.env.KEY ", Code: "invalid_path", Message: "Unset paths must be non-empty and contain no surrounding whitespace"},
			},
		},
		{
			name: "duplicate unset",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Unset:    []string{"agent.env.KEY", "agent.env.KEY"},
			},
			want: []ConfigPatchIssue{
				{Path: "agent.env.KEY", Code: "duplicate_unset", Message: "Unset paths must be unique"},
			},
		},
		{
			name: "conflicting set and unset",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set:      map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage(`4`)},
				Unset:    []string{"scheduler.maxConcurrentRuns"},
			},
			want: []ConfigPatchIssue{
				{Path: "scheduler.maxConcurrentRuns", Code: "conflicting_operation", Message: "A path cannot be both set and unset"},
			},
		},
		{
			name: "issues sorted by path then code",
			patch: ConfigPatchRequest{
				Revision: "sha256:test",
				Set: map[string]json.RawMessage{
					"b.path": json.RawMessage{},
					"a.path": json.RawMessage{},
				},
			},
			want: []ConfigPatchIssue{
				{Path: "a.path", Code: "missing_value", Message: "Set operations require a JSON value"},
				{Path: "b.path", Code: "missing_value", Message: "Set operations require a JSON value"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateConfigPatchRequest(tt.patch)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("validateConfigPatchRequest() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAsConfigRequestError(t *testing.T) {
	typed := ConfigRequestError{Kind: ConfigRequestErrorKindConflict, Message: "stale revision"}
	tests := []struct {
		name string
		err  error
		want ConfigRequestError
		ok   bool
	}{
		{name: "nil error", err: nil, ok: false},
		{name: "plain error", err: errors.New("boom"), ok: false},
		{name: "value", err: typed, want: typed, ok: true},
		{name: "pointer", err: &typed, want: typed, ok: true},
		{name: "wrapped value", err: fmt.Errorf("patch: %w", typed), want: typed, ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := asConfigRequestError(tt.err)
			if ok != tt.ok {
				t.Fatalf("asConfigRequestError() ok = %v, want %v", ok, tt.ok)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("asConfigRequestError() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestConfigRequestAPIError(t *testing.T) {
	issues := []ConfigPatchIssue{{Path: "revision", Code: "missing_revision", Message: "revision is required"}}
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    pkgapi.ErrorCode
		wantMessage string
	}{
		{name: "non config error maps to internal", err: errors.New("boom"), wantStatus: http.StatusInternalServerError, wantCode: pkgapi.ErrorCodeInternalError, wantMessage: "boom"},
		{name: "validation maps to 400", err: ConfigRequestError{Kind: ConfigRequestErrorKindValidation, Message: "Invalid configuration patch", Issues: issues}, wantStatus: http.StatusBadRequest, wantCode: pkgapi.ErrorCodeValidationFailed, wantMessage: "Invalid configuration patch"},
		{name: "unsupported maps to 400", err: ConfigRequestError{Kind: ConfigRequestErrorKindUnsupported, Message: "Configuration updates are not supported"}, wantStatus: http.StatusBadRequest, wantCode: pkgapi.ErrorCodeValidationFailed, wantMessage: "Configuration updates are not supported"},
		{name: "conflict maps to 409", err: ConfigRequestError{Kind: ConfigRequestErrorKindConflict, Message: "stale revision"}, wantStatus: http.StatusConflict, wantCode: pkgapi.ErrorCodeConfigConflict, wantMessage: "stale revision"},
		{name: "too large maps to 413", err: ConfigRequestError{Kind: ConfigRequestErrorKindTooLarge, Message: "Configuration patch is too large"}, wantStatus: http.StatusRequestEntityTooLarge, wantCode: pkgapi.ErrorCodeRequestTooLarge, wantMessage: "Configuration patch is too large"},
		{name: "unknown kind maps to internal", err: ConfigRequestError{Kind: "bogus", Message: "weird"}, wantStatus: http.StatusInternalServerError, wantCode: pkgapi.ErrorCodeInternalError, wantMessage: "weird"},
		{name: "blank message falls back", err: ConfigRequestError{Kind: ConfigRequestErrorKindValidation, Message: "  "}, wantStatus: http.StatusBadRequest, wantCode: pkgapi.ErrorCodeValidationFailed, wantMessage: "Configuration update failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configRequestAPIError(tt.err)
			if got.status != tt.wantStatus || got.code != tt.wantCode || got.message != tt.wantMessage {
				t.Fatalf("configRequestAPIError() = {status:%d code:%q message:%q}, want {status:%d code:%q message:%q}",
					got.status, got.code, got.message, tt.wantStatus, tt.wantCode, tt.wantMessage)
			}
		})
	}
}

func TestConfigRequestAPIErrorCarriesIssues(t *testing.T) {
	issues := []ConfigPatchIssue{{Path: "revision", Code: "missing_revision", Message: "revision is required"}}
	got := configRequestAPIError(ConfigRequestError{Kind: ConfigRequestErrorKindValidation, Message: "Invalid configuration patch", Issues: issues})
	details, ok := got.details.(struct {
		Issues []ConfigPatchIssue `json:"issues"`
	})
	if !ok {
		t.Fatalf("details type = %T, want issues struct", got.details)
	}
	if !reflect.DeepEqual(details.Issues, issues) {
		t.Fatalf("details.Issues = %#v, want %#v", details.Issues, issues)
	}
}
