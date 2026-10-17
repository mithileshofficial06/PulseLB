// Package web embeds the dashboard and arena pages.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the embedded static files.
func Handler() http.Handler {
	sub, _ := fs.Sub(files, "static")
	return http.FileServer(http.FS(sub))
}
