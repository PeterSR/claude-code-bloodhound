//go:build !(linux && gui)

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"bloodhound-gui was built without the 'gui' build tag (or for a non-Linux target). "+
			"Rebuild with `go build -tags gui ./cmd/bloodhound-gui` on a Linux host with "+
			"gtk+-3.0 and webkit2gtk-4.1 dev packages installed, or run `bloodhound daemon` "+
			"and open http://127.0.0.1:7777/ in a browser instead.")
	os.Exit(1)
}
