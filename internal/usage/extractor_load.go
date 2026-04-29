package usage

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
)

//go:embed extractor_default.json
var defaultExtractorJSON []byte

const (
	extractorFileName = "extractors.json"
	snapshotFileName  = "extractor_snapshot.txt"
)

// ExtractorOrigin describes where the active extractor came from.
type ExtractorOrigin string

const (
	OriginUser    ExtractorOrigin = "user"
	OriginDefault ExtractorOrigin = "default"
)

// LoadExtractor returns the active extractor and where it came from.
//
// Strategy: prefer the user state file at $XDG_STATE_HOME/bloodhound/extractors.json;
// fall back to the bundled default if the state file is missing. If the
// state file exists but is *invalid*, return an error — that's a loud
// failure case (the user previously bootstrapped, but the file is corrupt
// or schema-incompatible). Callers can choose to re-bootstrap or fall
// back; we don't silently swallow.
func LoadExtractor() (*Extractor, ExtractorOrigin, error) {
	dir, dirErr := config.StateDir()
	if dirErr == nil {
		userPath := filepath.Join(dir, extractorFileName)
		data, err := os.ReadFile(userPath)
		if err == nil {
			ex, parseErr := ParseExtractor(data)
			if parseErr == nil {
				return ex, OriginUser, nil
			}
			if errors.Is(parseErr, ErrExtractorVersionMismatch) {
				// Soft fail: a previous version's extractor on disk shouldn't
				// brick the tool. Fall back to default and log once; the
				// user can run --rebootstrap to regenerate.
				ex, err := ParseExtractor(defaultExtractorJSON)
				if err != nil {
					return nil, "", fmt.Errorf("parse bundled default extractor: %w", err)
				}
				fmt.Fprintf(os.Stderr,
					"[usage] %s is a previous version (%v); using bundled default. Run `bloodhound poll --rebootstrap` to regenerate.\n",
					userPath, parseErr)
				return ex, OriginDefault, nil
			}
			return nil, "", fmt.Errorf("user extractor at %s is invalid: %w", userPath, parseErr)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("read user extractor: %w", err)
		}
	}
	ex, err := ParseExtractor(defaultExtractorJSON)
	if err != nil {
		return nil, "", fmt.Errorf("parse bundled default extractor: %w", err)
	}
	return ex, OriginDefault, nil
}

// ExtractorPaths returns the resolved on-disk paths for the user extractor
// and its snapshot file. Useful for `doctor` and the Debug page.
func ExtractorPaths() (extractorPath, snapshotPath string, err error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, extractorFileName),
		filepath.Join(dir, snapshotFileName),
		nil
}

// SaveExtractor persists e to the state dir atomically (write-temp + rename),
// and writes the snapshot text alongside if non-empty. Always overwrites.
func SaveExtractor(e *Extractor, snapshot string) error {
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("encode extractor: %w", err)
	}
	dest := filepath.Join(dir, extractorFileName)
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp extractor: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename extractor: %w", err)
	}
	if snapshot != "" {
		_ = os.WriteFile(filepath.Join(dir, snapshotFileName), []byte(snapshot), 0o644)
	}
	return nil
}
