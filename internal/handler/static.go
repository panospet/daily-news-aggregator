package handler

import (
	"io/fs"
	"log"
	"net/http"
)

// StaticHandler returns an http.Handler that serves static files from the embedded FS.
func StaticHandler(embeddedFS fs.FS) http.Handler {
	staticFS, err := fs.Sub(embeddedFS, "static")
	if err != nil {
		log.Fatalf("static sub fs: %v", err)
	}
	return http.FileServer(http.FS(staticFS))
}
