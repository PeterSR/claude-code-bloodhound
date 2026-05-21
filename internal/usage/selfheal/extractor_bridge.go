package selfheal

import (
	"os"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// extractorSavedSince returns true if the on-disk extractor at
// usage.ExtractorPaths() has a mtime after `since` AND was generated
// by claude (not a stale bundled-default fallback). Used by Run() as a
// success heuristic — the orchestrator might exit cleanly without
// saving, and we want to distinguish that from a real save.
func extractorSavedSince(since time.Time) (bool, string) {
	path, _, err := usage.ExtractorPaths()
	if err != nil || path == "" {
		return false, ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, ""
	}
	if info.ModTime().Before(since) {
		return false, ""
	}
	// Peek at the file's generated_by to make sure this isn't an
	// unrelated touch.
	ext, _, err := usage.LoadExtractor()
	if err != nil {
		return false, ""
	}
	if ext.GeneratedBy != "claude" {
		return false, ""
	}
	return true, path
}
