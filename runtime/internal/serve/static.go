// Static asset handler for the offline yawr preview UI.

package serve

import (
	"embed"
	"net/http"
)

//go:embed static
var staticFS embed.FS

func (s *Server) handlePreviewAsset(w http.ResponseWriter, r *http.Request) {
	assets := map[string]string{
		"/preview/assets/highlighting/highlighter.js": "static/highlighting/highlighter.js",
		"/preview/assets/highlighting/worker.js":      "static/highlighting/worker.js",
		"/preview/assets/vendor/preview.js":           "static/vendor/preview.js",
	}
	path, ok := assets[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// handlePreviewIndex serves the embedded preview.html for GET /preview/.
func (s *Server) handlePreviewIndex(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		http.Error(w, "preview UI not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
