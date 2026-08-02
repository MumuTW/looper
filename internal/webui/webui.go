package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// BasePath is where this UI is mounted. It coexists with the React dashboard
// under /dashboard/ and shares its authorization: the caller mounts both behind
// the same auth-mode handling.
const BasePath = "/ui"

// TriagePath is the only page in this slice, and where /ui/ redirects.
const TriagePath = BasePath + "/triage"

// boardPath serves the swappable region on its own so the htmx poll re-renders
// the board without re-sending the chrome around it.
const boardPath = TriagePath + "/board"

const staticPrefix = BasePath + "/static/"

// cspHeader forbids inline script outright. htmx is the page's only script and
// it is served from this origin; the one thing htmx would otherwise inject
// inline is its indicator stylesheet, which the htmx-config meta turns off so
// style-src can stay 'self' as well.
const cspHeader = "default-src 'self'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var templates = template.Must(template.New("webui").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html"))

// NormalizePath is the shared routing rule for the API dispatcher and this
// handler. Cleaning before ownership checks keeps repeated slashes and dot
// segments from making the two layers disagree about which surface owns a URL.
func NormalizePath(requestPath string) string {
	return path.Clean("/" + strings.TrimPrefix(requestPath, "/"))
}

// Options configure the /ui/ handler.
type Options struct {
	// Load produces one render's input. Required.
	Load Loader
	// Now stamps the "last updated" line. Defaults to time.Now.
	Now func() time.Time
}

// Handler serves the hypermedia UI. It answers GET and HEAD only: this surface
// reads state and never asks the daemon to do anything.
func Handler(options Options) http.Handler {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	load := options.Load
	if load == nil {
		load = func(context.Context) Input { return Input{Now: now().UTC()} }
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Clean folds "/ui/" onto "/ui" so the bare mount point redirects to the
		// page whether or not the caller typed the trailing slash.
		requestPath := NormalizePath(r.URL.Path)
		switch {
		case requestPath == BasePath:
			http.Redirect(w, r, TriagePath, http.StatusFound)
		case requestPath == TriagePath:
			renderPage(w, r, "page.html", load, now)
		case requestPath == boardPath:
			renderPage(w, r, "board.html", load, now)
		case strings.HasPrefix(requestPath, staticPrefix):
			serveStatic(w, r, strings.TrimPrefix(requestPath, staticPrefix))
		default:
			http.NotFound(w, r)
		}
	})
}

// view is what the templates render. Board carries the rows; the rest is the
// chrome that does not change between polls.
type view struct {
	Board          Board
	Updated        string
	RefreshSeconds int
	RefreshEnabled bool
	BoardPath      string
	TriagePath     string
	StaticPrefix   string
	DashboardPath  string
}

func renderPage(w http.ResponseWriter, r *http.Request, name string, load Loader, now func() time.Time) {
	input := load(r.Context())
	if input.Now.IsZero() {
		input.Now = now().UTC()
	}
	board := Classify(input)

	data := view{
		Board:          board,
		Updated:        board.GeneratedAt.Local().Format("15:04:05"),
		RefreshSeconds: int(RefreshInterval / time.Second),
		RefreshEnabled: !strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("refresh")), "off"),
		BoardPath:      boardPath,
		TriagePath:     TriagePath,
		StaticPrefix:   staticPrefix,
		DashboardPath:  "/dashboard/",
	}

	// Render to a buffer so a template failure cannot leave a half-written 200
	// on the wire.
	var buffer bytes.Buffer
	if err := templates.ExecuteTemplate(&buffer, name, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buffer.Bytes())
}

func serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	file, err := staticFS.Open("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", staticContentType(name))
	// The asset names carry no content hash, so a max-age would keep serving the
	// previous binary's stylesheet after an upgrade. Revalidating against a
	// content ETag costs one conditional request and can never go stale.
	w.Header().Set("Cache-Control", "no-cache")
	if tag, ok := staticETags[name]; ok {
		w.Header().Set("ETag", tag)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), seeker)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// staticETags digests the embedded assets once, at start. The set is two files
// and never changes while the process runs.
var staticETags = func() map[string]string {
	tags := map[string]string{}
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		return tags
	}
	for _, entry := range entries {
		content, err := staticFS.ReadFile("static/" + entry.Name())
		if err != nil {
			continue
		}
		digest := sha256.Sum256(content)
		tags[entry.Name()] = `"` + hex.EncodeToString(digest[:12]) + `"`
	}
	return tags
}()

func staticContentType(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", cspHeader)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

// Owns reports whether a request path belongs to this UI.
func Owns(requestPath string) bool {
	requestPath = NormalizePath(requestPath)
	return requestPath == BasePath || strings.HasPrefix(requestPath, BasePath+"/")
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"age":     relativeAge,
		"compact": compactCount,
		"stamp": func(value time.Time) string {
			if value.IsZero() {
				return ""
			}
			return value.Local().Format("2006-01-02 15:04:05")
		},
	}
}
