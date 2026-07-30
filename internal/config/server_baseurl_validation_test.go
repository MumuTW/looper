package config

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalizeServerBaseURL(t *testing.T) {
	t.Parallel()

	valid := []struct {
		value string
		want  string
	}{
		{value: "http://localhost:17310", want: "http://localhost:17310"},
		{value: "  HTTP://LocalHost:17310/  ", want: "http://localhost:17310"},
		{value: "https://daemon.example", want: "https://daemon.example"},
		{value: "https://Daemon.Example:8443/looper/", want: "https://daemon.example:8443/looper"},
		{value: "https://daemon.example/a/b", want: "https://daemon.example/a/b"},
		{value: "http://[::1]:17310", want: "http://[::1]:17310"},
		{value: "http://192.168.1.5:17310", want: "http://192.168.1.5:17310"},
	}
	for _, tt := range valid {
		got, err := CanonicalizeServerBaseURL(tt.value)
		if err != nil {
			t.Fatalf("CanonicalizeServerBaseURL(%q) error = %v, want nil", tt.value, err)
		}
		if got != tt.want {
			t.Fatalf("CanonicalizeServerBaseURL(%q) = %q, want %q", tt.value, got, tt.want)
		}
	}

	invalid := []struct {
		value       string
		wantMessage string
	}{
		{value: "", wantMessage: "absolute http(s) URL"},
		{value: "daemon.example", wantMessage: "http or https scheme"},
		{value: "daemon.example:17310", wantMessage: "http or https scheme"},
		{value: "ftp://daemon.example", wantMessage: "http or https scheme"},
		{value: "http://", wantMessage: "must include a host"},
		{value: "http://:17310", wantMessage: "must include a host"},
		{value: "http://user:pass@daemon.example", wantMessage: "userinfo"},
		{value: "http://daemon.example/?admin=1", wantMessage: "query"},
		{value: "http://daemon.example/#fragment", wantMessage: "fragment"},
		{value: "http://daemon.example//double", wantMessage: "empty path segments"},
		{value: "http://daemon.example/a/../b", wantMessage: ". or .. path segments"},
		{value: "http://daemon.example/./a", wantMessage: ". or .. path segments"},
	}
	for _, tt := range invalid {
		got, err := CanonicalizeServerBaseURL(tt.value)
		if err == nil {
			t.Fatalf("CanonicalizeServerBaseURL(%q) = %q, want error containing %q", tt.value, got, tt.wantMessage)
		}
		if !strings.Contains(err.Error(), tt.wantMessage) {
			t.Fatalf("CanonicalizeServerBaseURL(%q) error = %q, want it to contain %q", tt.value, err, tt.wantMessage)
		}
	}
}

func TestValidateReportsInvalidServerBaseURL(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "daemon.example"
	cfg.Server.BaseURL = &baseURL

	err = Validate(cfg)
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate() error = %T %v, want *ConfigValidationError", err, err)
	}
	assertValidationIssue(t, validationErr, "server.baseUrl", "must use the http or https scheme")
}

func TestValidateAcceptsCanonicalServerBaseURL(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "https://daemon.example/looper"
	cfg.Server.BaseURL = &baseURL

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want valid baseUrl accepted", err)
	}
}

func TestNormalizeStoresCanonicalServerBaseURL(t *testing.T) {
	t.Parallel()

	value := "HTTP://Daemon.Example:8080/base/"
	cfg, err := Normalize(t.TempDir(), PartialConfig{Server: &PartialServerConfig{BaseURL: &value}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Server.BaseURL == nil || *cfg.Server.BaseURL != "http://daemon.example:8080/base" {
		t.Fatalf("Normalize() Server.BaseURL = %v, want http://daemon.example:8080/base", cfg.Server.BaseURL)
	}

	invalid := "daemon.example"
	cfg, err = Normalize(t.TempDir(), PartialConfig{Server: &PartialServerConfig{BaseURL: &invalid}})
	if err != nil {
		t.Fatalf("Normalize() error = %v, want invalid baseUrl kept for Validate", err)
	}
	if cfg.Server.BaseURL == nil || *cfg.Server.BaseURL != "daemon.example" {
		t.Fatalf("Normalize() Server.BaseURL = %v, want verbatim daemon.example", cfg.Server.BaseURL)
	}
}
