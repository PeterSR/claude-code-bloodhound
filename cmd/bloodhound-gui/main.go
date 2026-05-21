// bloodhound-gui opens a native window with the dashboard, talking to a
// running daemon over a unix socket. The React bundle is embedded; an
// asset-server middleware reverse-proxies /api/* to the daemon socket so
// the React app stays single-origin and the daemon needs no CORS config.
//
// Single-instance behaviour is provided by Wails (D-Bus name reservation
// on Linux); a second launch sends its args to the primary and exits.
//
// All actual data work — collection, persistence, the API — lives in the
// daemon. This binary is a viewer.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/PeterSR/claude-code-bloodhound/internal/api"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
	"github.com/PeterSR/claude-code-bloodhound/web"
)

const (
	wmClass   = "bloodhound"
	winTitle  = "Bloodhound"
	singleID  = "bloodhound-gui"
	apiPrefix = "/api/"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		socketFlag  = flag.String("daemon-socket", "", "override daemon unix-socket path (default: $XDG_RUNTIME_DIR/bloodhound/api.sock)")
		width       = flag.Int("width", 1280, "initial window width")
		height      = flag.Int("height", 800, "initial window height")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	socketPath := *socketFlag
	if socketPath == "" {
		p, err := api.SocketPath()
		if err != nil {
			log.Fatalf("daemon socket: %v", err)
		}
		socketPath = p
	}

	assets, err := web.FS()
	if err != nil {
		log.Fatalf("embedded assets: %v", err)
	}

	var appCtx context.Context

	err = wails.Run(&options.App{
		Title:  winTitle,
		Width:  *width,
		Height: *height,
		AssetServer: &assetserver.Options{
			Assets:     assets,
			Middleware: apiProxyMiddleware(socketPath),
		},
		OnStartup: func(ctx context.Context) {
			appCtx = ctx
		},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: singleID,
			OnSecondInstanceLaunch: func(_ options.SecondInstanceData) {
				if appCtx == nil {
					return
				}
				wruntime.WindowUnminimise(appCtx)
				wruntime.WindowShow(appCtx)
			},
		},
		Linux: &linux.Options{
			ProgramName:      wmClass,
			WebviewGpuPolicy: linux.WebviewGpuPolicyAlways,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "wails: %v\n", err)
		os.Exit(1)
	}
}

// apiProxyMiddleware reverse-proxies /api/* requests to the daemon over
// a unix socket. Any non-/api request falls through to the next handler
// (Wails' static asset server, which serves the embedded React bundle).
//
// The proxy target URL's host is cosmetic — the request's HTTP layer
// still sets a Host header — but the Transport's DialContext ignores
// network/address and dials the socket directly.
func apiProxyMiddleware(socketPath string) assetserver.Middleware {
	target := &url.URL{Scheme: "http", Host: "bloodhound.local"}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, apiPrefix) {
				proxy.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
