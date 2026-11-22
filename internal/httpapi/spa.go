package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves the Angular build; unknown paths fall back to index.html.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(fsys, p); err != nil {
			index, err := fs.ReadFile(fsys, "index.html")
			if err != nil {
				http.Error(w, "interface não compilada: rode `make web`", 503)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(index)
			return
		}
		if p == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.Contains(p, "-") && (strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css")) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
