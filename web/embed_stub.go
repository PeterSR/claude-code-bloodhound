//go:build !prod

// Package web — non-prod build. Returns an empty filesystem so callers can
// detect the missing bundle and render a "run make build" placeholder.
//
// Two reasons this exists:
//  1. //go:embed requires its target directory to exist with at least one
//     file at compile time. We don't track web/dist, so a plain `go build`
//     from a fresh checkout would otherwise fail.
//  2. During day-to-day Go-only iteration we don't need the React bundle
//     bundled in; Vite's dev server proxies /api on its own.
package web

import (
	"errors"
	"io/fs"
)

// FS returns nil; Has() will report false.
func FS() (fs.FS, error) {
	return nil, errors.New("web bundle not embedded; rebuild with `make build`")
}

// Has reports false in non-prod builds. The serve command renders a
// placeholder page instructing the user to run `make build`.
func Has() bool {
	return false
}
