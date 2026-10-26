// Package web exposes the embedded template and static assets.
// Handlers import these vars; the embed must live alongside the
// templates/ and static/ directories at the package root.
package web

import "embed"

// templates keeps `all:` because it needs it: `_pagination.html.tmpl` starts
// with an underscore, and a plain embed skips "." and "_" prefixed files.
//
//go:embed all:templates
var TemplatesFS embed.FS

// static does NOT, and that is the point. It is served wholesale by a
// FileServer, so with `all:` a stray .env, a .DS_Store or a scratch _notes.md
// dropped in there would be compiled into the binary and published at
// /static/. Nothing legitimate here starts with "." or "_".
//
//go:embed static
var StaticFS embed.FS
