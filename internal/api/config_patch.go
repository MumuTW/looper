package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// maxConfigPatchBodyBytes caps the dashboard's config patch request body.
const maxConfigPatchBodyBytes = 1 << 20

// ConfigPatchRequest is the dashboard's field-level mutation contract. Set
// values stay as raw JSON so the configuration authority performs all typing
// and validation; Unset removes values from the file layer.
type ConfigPatchRequest struct {
	Revision string                     `json:"revision"`
	Set      map[string]json.RawMessage `json:"set"`
	Unset    []string                   `json:"unset"`
}

type ConfigRequestErrorKind string

const (
	ConfigRequestErrorKindValidation  ConfigRequestErrorKind = "validation"
	ConfigRequestErrorKindConflict    ConfigRequestErrorKind = "conflict"
	ConfigRequestErrorKindUnsupported ConfigRequestErrorKind = "unsupported"
	ConfigRequestErrorKindTooLarge    ConfigRequestErrorKind = "too_large"
)

// ConfigPatchIssue identifies one rejected field-level mutation.
type ConfigPatchIssue struct {
	Path    string `json:"path,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ConfigRequestError lets the configuration authority report stable field
// issues while the HTTP layer owns status codes and envelope formatting.
type ConfigRequestError struct {
	Kind    ConfigRequestErrorKind
	Message string
	Issues  []ConfigPatchIssue
}

func (e ConfigRequestError) Error() string {
	return e.Message
}

func decodeConfigPatchRequest(w http.ResponseWriter, r *http.Request) (ConfigPatchRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigPatchBodyBytes)
	raw, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(readErr, &maxBytesError) {
			return ConfigPatchRequest{}, ConfigRequestError{
				Kind:    ConfigRequestErrorKindTooLarge,
				Message: "Configuration patch is too large",
				Issues:  []ConfigPatchIssue{{Code: "request_too_large", Message: fmt.Sprintf("Request body exceeds %d bytes", maxConfigPatchBodyBytes)}},
			}
		}
		return ConfigPatchRequest{}, ConfigRequestError{Kind: ConfigRequestErrorKindValidation, Message: "Invalid configuration patch", Issues: []ConfigPatchIssue{{Code: "invalid_json", Message: readErr.Error()}}}
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := config.ValidateUniqueJSONNames(raw); err != nil {
			return ConfigPatchRequest{}, ConfigRequestError{
				Kind:    ConfigRequestErrorKindValidation,
				Message: "Invalid configuration patch",
				Issues:  []ConfigPatchIssue{{Code: "duplicate_json_name", Message: err.Error()}},
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var decoded *ConfigPatchRequest
	if err := decoder.Decode(&decoded); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return ConfigPatchRequest{}, ConfigRequestError{
				Kind:    ConfigRequestErrorKindTooLarge,
				Message: "Configuration patch is too large",
				Issues: []ConfigPatchIssue{{
					Code:    "request_too_large",
					Message: fmt.Sprintf("Request body exceeds %d bytes", maxConfigPatchBodyBytes),
				}},
			}
		}
		message := "Request body must be a JSON object"
		code := "invalid_json"
		if errors.Is(err, io.EOF) {
			message = "Request body is required"
			code = "request_body_required"
		}
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: code, Message: message}},
		}
	}
	if decoded == nil {
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: "invalid_json", Message: "Request body must be a JSON object"}},
		}
	}
	patch := *decoded
	if strings.TrimSpace(patch.Revision) == "" {
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Path: "revision", Code: "missing_revision", Message: "revision is required; refresh configuration and try again"}},
		}
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				return ConfigPatchRequest{}, ConfigRequestError{
					Kind:    ConfigRequestErrorKindTooLarge,
					Message: "Configuration patch is too large",
					Issues:  []ConfigPatchIssue{{Code: "request_too_large", Message: fmt.Sprintf("Request body exceeds %d bytes", maxConfigPatchBodyBytes)}},
				}
			}
		}
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: "trailing_json", Message: "Request body must contain exactly one JSON object"}},
		}
	}

	issues := validateConfigPatchRequest(patch)
	if len(issues) > 0 {
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  issues,
		}
	}
	if patch.Set == nil {
		patch.Set = map[string]json.RawMessage{}
	}
	if patch.Unset == nil {
		patch.Unset = []string{}
	}
	return patch, nil
}

func validateConfigPatchRequest(patch ConfigPatchRequest) []ConfigPatchIssue {
	issues := make([]ConfigPatchIssue, 0)
	if len(patch.Set) == 0 && len(patch.Unset) == 0 {
		issues = append(issues, ConfigPatchIssue{Code: "empty_patch", Message: "At least one set or unset operation is required"})
	}

	setPaths := make(map[string]struct{}, len(patch.Set))
	for path, raw := range patch.Set {
		setPaths[path] = struct{}{}
		if path == "" || path != strings.TrimSpace(path) {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "invalid_path", Message: "Set paths must be non-empty and contain no surrounding whitespace"})
		}
		if len(raw) == 0 {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "missing_value", Message: "Set operations require a JSON value"})
		}
	}

	unsetPaths := make(map[string]struct{}, len(patch.Unset))
	for _, path := range patch.Unset {
		if path == "" || path != strings.TrimSpace(path) {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "invalid_path", Message: "Unset paths must be non-empty and contain no surrounding whitespace"})
		}
		if _, exists := unsetPaths[path]; exists {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "duplicate_unset", Message: "Unset paths must be unique"})
		}
		unsetPaths[path] = struct{}{}
		if _, exists := setPaths[path]; exists {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "conflicting_operation", Message: "A path cannot be both set and unset"})
		}
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path == issues[j].Path {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Path < issues[j].Path
	})
	return issues
}

func configRequestAPIError(err error) apiError {
	requestError, ok := asConfigRequestError(err)
	if !ok {
		return internalServerError(err)
	}

	status := http.StatusBadRequest
	code := pkgapi.ErrorCodeValidationFailed
	switch requestError.Kind {
	case ConfigRequestErrorKindValidation, ConfigRequestErrorKindUnsupported:
	case ConfigRequestErrorKindConflict:
		status = http.StatusConflict
		code = pkgapi.ErrorCodeConfigConflict
	case ConfigRequestErrorKindTooLarge:
		status = http.StatusRequestEntityTooLarge
		code = pkgapi.ErrorCodeRequestTooLarge
	default:
		return internalServerError(err)
	}

	message := strings.TrimSpace(requestError.Message)
	if message == "" {
		message = "Configuration update failed"
	}
	issues := append([]ConfigPatchIssue{}, requestError.Issues...)
	return apiError{
		code:    code,
		status:  status,
		message: message,
		details: struct {
			Issues []ConfigPatchIssue `json:"issues"`
		}{Issues: issues},
	}
}

func asConfigRequestError(err error) (ConfigRequestError, bool) {
	if err == nil {
		return ConfigRequestError{}, false
	}
	var pointer *ConfigRequestError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	var value ConfigRequestError
	if errors.As(err, &value) {
		return value, true
	}
	return ConfigRequestError{}, false
}
