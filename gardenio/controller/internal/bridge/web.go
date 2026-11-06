package bridge

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/*
var webFS embed.FS

// WebHandler returns an http.Handler that serves the embedded web UI.
// The UI is available at the given prefix (e.g., "/ui/").
func WebHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	return http.FileServer(http.FS(sub))
}
