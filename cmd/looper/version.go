package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/MumuTW/looper/internal/version"
)

type daemonVersionResponse struct {
	Version string                `json:"version"`
	Build   version.BuildMetadata `json:"build"`
}

type versionCheckReport struct {
	SameBuild bool         `json:"sameBuild"`
	CLI       version.Info `json:"cli"`
	Daemon    version.Info `json:"daemon"`
}

func runVersion(ctx context.Context, global, operands []string, stdout io.Writer) error {
	jsonOutput := false
	checkDaemon := false
	for _, operand := range operands {
		switch operand {
		case "--json":
			if jsonOutput {
				return badUsage("version accepts --json at most once")
			}
			jsonOutput = true
		case "--check-daemon":
			if checkDaemon {
				return badUsage("version accepts --check-daemon at most once")
			}
			checkDaemon = true
		default:
			return badUsage("version does not accept %q", operand)
		}
	}

	local := version.Current()
	if !checkDaemon {
		if !jsonOutput {
			_, _ = fmt.Fprintln(stdout, local.Version)
			return nil
		}
		return writeVersionJSON(stdout, local)
	}

	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	remote, err := requestJSON[daemonVersionResponse](ctx, cfg, http.MethodGet, "/api/v1/version", nil)
	if err != nil {
		return err
	}
	daemon := version.Info{Version: remote.Version, Metadata: remote.Build}
	report := versionCheckReport{SameBuild: local.SameBuild(daemon), CLI: local, Daemon: daemon}
	if jsonOutput {
		if err := writeVersionJSON(stdout, report); err != nil {
			return err
		}
	} else {
		localJSON, err := json.Marshal(local)
		if err != nil {
			return fmt.Errorf("encode looper build identity: %w", err)
		}
		daemonJSON, err := json.Marshal(daemon)
		if err != nil {
			return fmt.Errorf("encode looperd build identity: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "looper:    %s\nlooperd:   %s\nsameBuild: %t\n", localJSON, daemonJSON, report.SameBuild)
	}
	if !report.SameBuild {
		return fmt.Errorf("looper and looperd build identities do not match")
	}
	return nil
}

func writeVersionJSON(w io.Writer, value any) error {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return fmt.Errorf("encode build identity: %w", err)
	}
	return nil
}
