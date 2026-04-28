package api

import (
	"io"
	"os"
)

// stderr is split out for testability and so callers can redirect.
var stderr = func() io.Writer { return os.Stderr }
