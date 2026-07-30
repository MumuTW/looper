package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/MumuTW/looper/internal/config"
)

const dashboardBootstrapCodePath = "/api/v1/dashboard/bootstrap/code"

type dashboardBootstrapCodeResponse struct {
	Code string `json:"code"`
}

// runDashboard prints one URL an operator can paste into an unauthenticated
// browser. In local-token mode, the daemon remains the authority that mints the
// one-shot capability; the CLI never puts the long-lived token in the URL.
func runDashboard(ctx context.Context, global, operands []string, stdout io.Writer) error {
	if len(operands) != 0 {
		return badUsage("dashboard takes no arguments")
	}
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}

	endpoint := daemonBaseURL(cfg)
	if cfg.Server.AuthMode == config.AuthModeNone {
		_, err = fmt.Fprintln(stdout, dashboardURL(endpoint, ""))
		return err
	}

	minted, err := requestJSON[dashboardBootstrapCodeResponse](ctx, cfg, http.MethodPost, dashboardBootstrapCodePath, nil)
	if err != nil {
		return fmt.Errorf("mint dashboard login URL: %w", err)
	}
	if strings.TrimSpace(minted.Code) == "" {
		return fmt.Errorf("mint dashboard login URL: daemon returned an empty bootstrap code")
	}
	_, err = fmt.Fprintln(stdout, dashboardURL(endpoint, minted.Code))
	return err
}

func dashboardURL(endpoint, code string) string {
	value := strings.TrimRight(endpoint, "/") + "/dashboard/"
	if code == "" {
		return value
	}
	return value + "?code=" + url.QueryEscape(code)
}
