package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/escalator"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/webui"
)

// The hypermedia operator UI is served from inside the API handler rather than
// beside it in NewRootHandler, so it inherits one authorization path with the
// JSON API: the same Host/Origin browser guard, the same auth-mode handling
// (loopback-only under authMode none, Bearer under local-token), and the same
// failure shapes. The React dashboard under /dashboard/ needs none of that
// because it ships no state — these pages render durable state directly.

func isWebUIPath(path string) bool { return webui.Owns(path) }

// handleWebUIRoute renders the /ui/ pages. The handler is assembled per request
// because both halves of what it reads — the live configuration and the
// runtime's repositories — can change after the API handler was built.
func (h *Handler) handleWebUIRoute(w http.ResponseWriter, r *http.Request) {
	cfg := h.effectiveConfig()
	var repositories *storage.Repositories
	if h.context.Runtime != nil {
		repositories = h.context.Runtime.Services().Repositories
	}

	collector := h.webUIEscalatorCollector(cfg, repositories)
	links := looperdruntime.NewEscalatorLinker(cfg)
	pathPrefix := ""
	if cfg.Server.BaseURL != nil {
		if parsed, err := url.Parse(strings.TrimSpace(*cfg.Server.BaseURL)); err == nil {
			pathPrefix = parsed.Path
		}
	}
	webui.Handler(webui.Options{
		Load:       h.webUICache.Wrap(webui.NewRepositoryLoader(repositories, collector, links, h.now)),
		Now:        h.now,
		PathPrefix: pathPrefix,
	}).ServeHTTP(w, r)
}

func (h *Handler) webUIEscalatorCollector(cfg config.Config, repositories *storage.Repositories) *escalator.Collector {
	key := fmt.Sprintf("%d/%d/%d/%d|%s|%s|%d", cfg.Roles.Escalator.RetryAttemptThreshold, cfg.Roles.Escalator.UnroutedAfterSeconds, cfg.Roles.Escalator.StaleHeadAfterSeconds, cfg.Roles.Escalator.MaxItems, derefString(cfg.Server.BaseURL), cfg.Server.Host, cfg.Server.Port)
	if h.webUICollectorState == nil {
		return looperdruntime.NewEscalatorCollector(cfg, repositories, h.now)
	}
	state := h.webUICollectorState
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.collector == nil || state.repositories != repositories || state.configKey != key {
		state.collector = looperdruntime.NewEscalatorCollector(cfg, repositories, h.now)
		state.repositories = repositories
		state.configKey = key
	}
	return state.collector
}
