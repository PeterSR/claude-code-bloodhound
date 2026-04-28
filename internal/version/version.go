// Package version exposes build metadata, populated via -ldflags at build time.
package version

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human-readable build identifier.
func String() string {
	return Version
}
