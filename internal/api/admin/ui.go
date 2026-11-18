package admin

import (
	"bytes"
	"embed"
	"net/http"
	"path"
	"strings"

	"llmgw/internal/version"
)

//go:embed ui
var uiFS embed.FS

// uiCSP allows only same-origin resources: no inline scripts, no CDNs.
const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func (a *API) registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.HandleFunc("GET /admin/{$}", a.serveIndex)
	mux.HandleFunc("GET /admin/assets/{file}", a.serveAsset)
}

func uiHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

func (a *API) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b = bytes.ReplaceAll(b, []byte("{{VERSION}}"), []byte(version.Version))
	uiHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (a *API) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if strings.Contains(name, "/") || strings.HasPrefix(name, ".") || name == "index.html" {
		http.NotFound(w, r)
		return
	}
	b, err := uiFS.ReadFile("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", http.DetectContentType(b))
	}
	uiHeaders(w)
	// Asset URLs carry ?v=<version>, so they can be cached; dev builds revalidate.
	if version.Version == "dev" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	_, _ = w.Write(b)
}
