// Package webui serves the embedded admin single-page UI.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var staticFiles embed.FS

func IndexHTML() []byte {
	b, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		return []byte("<h1>webui missing</h1>")
	}
	return b
}

func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.StripPrefix("/static/", files).ServeHTTP(w, r)
	})
}
