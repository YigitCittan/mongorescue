// Package web provides embedded static assets for the MongoRescue dashboard.
package web

import (
	"embed"
	"io/fs"
)

// StaticFS embeds all dashboard web assets into the compiled binary.
//
//go:embed static/*
var StaticFS embed.FS

// GetSubFS returns an isolated sub-filesystem where static/ is the root.
func GetSubFS() (fs.FS, error) {
	return fs.Sub(StaticFS, "static")
}
