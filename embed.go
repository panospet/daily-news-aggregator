package htmxdailynews

import "embed"

// EmbeddedFS contains the embedded templates and static files.
// This must live at the project root because go:embed paths are
// relative to the source file's directory.
//
//go:embed templates static
var EmbeddedFS embed.FS
