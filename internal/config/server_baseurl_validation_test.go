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
		{value: "https://Daemon.Example:8443/", want: "https://daemon.example:8443"},
		{value: "http://[::1]:17310", want: "http://[::1]:17310"},
		{value: "http://192.168.1.5:17310", want: "http://192.168.1.5:17310"},
		// The IPv6 zone identifier keeps its case and its %25 escaping.
		{value: "http://[FE80::1%25ETH0]:17310", want: "http://[fe80::1%25ETH0]:17310"},
		// Ports canonicalize to the browser serialization: integer spelling,
		// scheme defaults omitted.
		{value: "https://daemon.example:0443", want: "https://daemon.example"},
		{value: "http://daemon.example:080", want: "http://daemon.example"},
		{value: "http://daemon.example:08080", want: "http://daemon.example:8080"},
		{value: "https://daemon.example:80", want: "https://daemon.example:80"},
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
		{value: "http://daemon.example//double", wantMessage: "must not include a path"},
		{value: "http://daemon.example/a/../b", wantMessage: "must not include a path"},
		{value: "http://daemon.example/./a", wantMessage: "must not include a path"},
		{value: "https://daemon.example/looper", wantMessage: "must not include a path"},
		{value: "http://0:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://00:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://0x0:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://0.0.0:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://0x7f.0x0.0x0.0x1:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://127.0.0.1:99999", wantMessage: "port between 1 and 65535"},
		{value: "http://127.0.0.1:0", wantMessage: "port between 1 and 65535"},
		{value: "https://daemon.example/%2e%2e/admin", wantMessage: "must not include a path"},
		{value: "https://daemon.example/a%2F%2Fb", wantMessage: "must not include a path"},
		{value: "https://daemon.example/a%20b", wantMessage: "must not include a path"},
		{value: "http://bücher.example", wantMessage: "IDNA/punycode"},
		{value: "http://0.0.0.0:17310", wantMessage: "unspecified (wildcard) host"},
		{value: "http://[::]:17310", wantMessage: "unspecified (wildcard) host"},
		// Colon-bearing hosts: url.Parse splits the authority on the final
		// colon, so the misparse is preserved unless the host is a bracketed
		// IPv6 literal.
		{value: "http://localhost:80:90", wantMessage: "colon-bearing host"},
		{value: "http://::1", wantMessage: "colon-bearing host"},
		// Trailing-dot numeric IPv4 spellings: browsers strip the terminal dot
		// while canonicalizing, but Go's net.ParseIP rejects the dotted form,
		// so the stored authority and dial target would diverge.
		{value: "http://0.0.0.0.:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://127.0.0.1.:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://2130706433.:17310", wantMessage: "canonical dotted-quad"},
		{value: "http://0x7f.0x0.0x0.0x1.:17310", wantMessage: "canonical dotted-quad"},
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
	baseURL := "https://daemon.example"
	token := "secret"
	cfg.Server.BaseURL = &baseURL
	cfg.Server.AuthMode = AuthModeLocalToken
	cfg.Server.LocalToken = &token

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want valid baseUrl accepted", err)
	}
}

func TestValidateRequiresTokenAuthForPublicBaseURL(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.AuthMode = AuthModeNone
	publicBase := "https://looper.example.com"
	cfg.Server.BaseURL = &publicBase

	err = Validate(cfg)
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate() error = %T %v, want *ConfigValidationError", err, err)
	}
	assertValidationIssue(t, validationErr, "server.authMode", "none is allowed only when server.baseUrl advertises a loopback authority; use local-token when a proxy, tunnel, or public hostname fronts the daemon")

	// Loopback advertised authority stays fine without a token, and a public
	// one is accepted once token auth is on.
	loopbackBase := "http://localhost:8080"
	cfg.Server.BaseURL = &loopbackBase
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want loopback baseUrl accepted with authMode none", err)
	}
	token := "secret"
	cfg.Server.BaseURL = &publicBase
	cfg.Server.AuthMode = AuthModeLocalToken
	cfg.Server.LocalToken = &token
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want public baseUrl accepted with local-token", err)
	}
}

func TestValidateRejectsNonCanonicalServerBaseURL(t *testing.T) {
	t.Parallel()

	// A config constructed directly (bypassing Normalize) must already hold
	// the canonical form: Validate only computes canonical without storing it,
	// so a raw spelling like "https://daemon.example:0443" would otherwise pass
	// while allowedAuthorities records port "0443" and the browser omits the
	// default :443.
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := "secret"
	cfg.Server.AuthMode = AuthModeLocalToken
	cfg.Server.LocalToken = &token

	nonCanonical := "https://daemon.example:0443"
	cfg.Server.BaseURL = &nonCanonical
	err = Validate(cfg)
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate() error = %T %v, want *ConfigValidationError", err, err)
	}
	found := false
	for _, issue := range validationErr.Issues {
		if issue.Path == "server.baseUrl" && strings.Contains(issue.Message, "must be in canonical form") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Validate() issues = %#v, want server.baseUrl canonical-form issue", validationErr.Issues)
	}

	// The canonical spelling of the same authority is accepted.
	canonical := "https://daemon.example"
	cfg.Server.BaseURL = &canonical
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want canonical baseUrl accepted", err)
	}
}

func TestNormalizeStoresCanonicalServerBaseURL(t *testing.T) {
	t.Parallel()

	value := "HTTP://Daemon.Example:8080/"
	cfg, err := Normalize(t.TempDir(), PartialConfig{Server: &PartialServerConfig{BaseURL: &value}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Server.BaseURL == nil || *cfg.Server.BaseURL != "http://daemon.example:8080" {
		t.Fatalf("Normalize() Server.BaseURL = %v, want http://daemon.example:8080", cfg.Server.BaseURL)
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
