package serve

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPresentationOfflineAssetAllowlist(t *testing.T) {
	s := &Server{}
	for _, path := range []string{"/preview/assets/highlighting/highlighter.js", "/preview/assets/highlighting/worker.js", "/preview/assets/vendor/preview.js"} {
		w := httptest.NewRecorder()
		s.handlePreviewAsset(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || w.Body.Len() == 0 {
			t.Fatalf("offline asset unavailable: %s status=%d", path, w.Code)
		}
	}
	for _, path := range []string{"/preview/assets/preview.html", "/preview/assets/../preview.html", "/preview/assets/highlighting/not-allowed.js"} {
		w := httptest.NewRecorder()
		s.handlePreviewAsset(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("asset path not allowlisted: %s", path)
		}
	}
}
