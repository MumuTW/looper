package runtime

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/storage"
)

type runtimeEscalatorLinker struct {
	dashboardBase string
}

// NewEscalatorLinker builds the action links every Escalator projection points
// at. Exported because the operator web UI matches digest items back to the
// pull requests and loops it renders by link identity, and a second link
// builder would silently stop matching the moment either one changed.
func NewEscalatorLinker(cfg config.Config) escalator.Linker {
	return newRuntimeEscalatorLinker(cfg)
}

// NewEscalatorCollector builds the read-only digest collector from the same
// role configuration the scheduler uses. The collector never mutates work, so
// read surfaces may run it regardless of whether the Escalator role is enabled;
// what roles.escalator gates is the notification cadence, not the detection.
func NewEscalatorCollector(cfg config.Config, repos *storage.Repositories, now func() time.Time) *escalator.Collector {
	return escalator.NewCollector(repos, NewEscalatorLinker(cfg), escalator.CollectorOptions{
		Now:                   now,
		RetryAttemptThreshold: cfg.Roles.Escalator.RetryAttemptThreshold,
		UnroutedAfter:         time.Duration(cfg.Roles.Escalator.UnroutedAfterSeconds) * time.Second,
		StaleHeadAfter:        time.Duration(cfg.Roles.Escalator.StaleHeadAfterSeconds) * time.Second,
	})
}

func newRuntimeEscalatorLinker(cfg config.Config) runtimeEscalatorLinker {
	base := ""
	if cfg.Server.BaseURL != nil {
		base = strings.TrimRight(strings.TrimSpace(*cfg.Server.BaseURL), "/")
	}
	if base == "" {
		host := strings.TrimSpace(cfg.Server.Host)
		switch host {
		case "", "0.0.0.0", "::", "[::]":
			host = "127.0.0.1"
		}
		base = "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Server.Port))
	}
	return runtimeEscalatorLinker{dashboardBase: base + "/dashboard"}
}

func (l runtimeEscalatorLinker) Issue(_ string, repo string, number int64) string {
	return fmt.Sprintf("%s/issues/%d", repositoryBrowserBase(repo), number)
}

func (l runtimeEscalatorLinker) PullRequest(_ string, repo string, number int64) string {
	return fmt.Sprintf("%s/pull/%d", repositoryBrowserBase(repo), number)
}

func (l runtimeEscalatorLinker) Loop(_ string, seq int64) string {
	return fmt.Sprintf("%s/loops/%d", l.dashboardBase, seq)
}

func repositoryBrowserBase(repo string) string {
	repo = strings.TrimSpace(repo)
	if parsed, err := url.Parse(repo); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return strings.TrimRight(parsed.Scheme+"://"+parsed.Host+parsed.Path, "/")
	}
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return "https://" + strings.Join(parts, "/")
	}
	return "https://github.com/" + strings.Join(parts, "/")
}
