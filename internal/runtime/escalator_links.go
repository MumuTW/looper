package runtime

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/MumuTW/looper/internal/config"
)

type runtimeEscalatorLinker struct {
	dashboardBase string
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
