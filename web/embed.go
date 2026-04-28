// Package web embeds the built React bundle so the Go binary can serve it
// without needing the on-disk web/dist directory at runtime.
//
// The embed always succeeds because web/dist/.gitkeep is committed. Whether
// a real bundle is present is detected at runtime via Has().
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the dist subtree as an io/fs.FS, with the leading "dist/"
// stripped so callers can serve "/" → index.html naturally.
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// Has reports whether a real built bundle (index.html) is present in the
// embedded filesystem. Used by the serve command to decide between serving
// the real UI or a "build me" placeholder.
func Has() bool {
	sub, err := FS()
	if err != nil {
		return false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return false
	}
	return true
}
