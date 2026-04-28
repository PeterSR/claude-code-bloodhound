//go:build windows

package usage

import (
	"context"
	"errors"
)

// drive on Windows is not yet implemented. The Unix driver uses creack/pty;
// the Windows port needs a ConPTY-based equivalent. Tracked in NOTES.md.
func drive(ctx context.Context, opts Options) ([]byte, error) {
	return nil, errors.New("usage scraper not yet supported on Windows: ConPTY driver pending")
}
