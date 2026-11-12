package server

import (
	"embed"
	"html/template"
)

//go:embed web/*.gohtml
var browserTemplates embed.FS

// Templates are parsed once during server construction; html/template keeps all
// identity and remote client metadata escaped at the presentation boundary.
func parseBrowserViews() (*template.Template, error) {
	return template.ParseFS(browserTemplates, "web/*.gohtml")
}
