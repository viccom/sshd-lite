//go:build webssh

package webssh

import (
	"embed"
	"net/http"
)

//go:embed assets
var assetsFS embed.FS

// serveUI writes the embedded index.html. http.FS files implement
// io.ReadSeeker, so the file can go straight to ServeContent.
func (h *Handler) serveUI(w http.ResponseWriter, r *http.Request) {
	fs := http.FS(assetsFS)
	f, err := fs.Open("assets/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	stat, _ := f.Stat()
	http.ServeContent(w, r, "index.html", stat.ModTime(), f)
}
