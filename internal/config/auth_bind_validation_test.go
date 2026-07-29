package config

import (
	"errors"
	"testing"
)

func TestValidateTokenlessServerRequiresLoopbackBind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host    string
		wantErr bool
	}{
		{host: "127.0.0.1"},
		{host: "::1"},
		{host: "[::1]"},
		{host: "localhost"},
		{host: "LOCALHOST"},
		{host: "0.0.0.0", wantErr: true},
		{host: "::", wantErr: true},
		{host: "192.168.1.5", wantErr: true},
		{host: "looper.internal", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Server.Host = tt.host
			cfg.Server.AuthMode = AuthModeNone
			err = Validate(cfg)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			var validationErr *ConfigValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate() error = %T %v, want *ConfigValidationError", err, err)
			}
			assertValidationIssue(t, validationErr, "server.authMode", "none is allowed only when server.host is localhost or a loopback IP; use local-token for wildcard, LAN, public, proxy, or custom-hostname binds")
		})
	}
}

func TestValidatePublicBindAllowsLocalToken(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := "secret"
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.AuthMode = AuthModeLocalToken
	cfg.Server.LocalToken = &token
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want public bind with local-token accepted", err)
	}
}
