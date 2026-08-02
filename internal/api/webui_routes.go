package api

import (
	"net/http"
	"net/url"
	"strings"

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

	collector := looperdruntime.NewEscalatorCollector(cfg, repositories, h.now)
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
