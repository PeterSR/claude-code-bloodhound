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

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
	"github.com/PeterSR/claude-code-bloodhound/web"
)

const (
	wmClass        = "bloodhound"
	winTitle       = "Bloodhound"
	singleID       = "bloodhound-gui"
	apiPrefix      = "/api/"
	activationFlag = "--xdg-activation-token"
)

func main() {
	// Wayland's xdg-activation protocol passes XDG_ACTIVATION_TOKEN in
	// env to whatever process the compositor activates. On a second
	// `bloodhound-gui` launch from dash-to-dock, Wails' D-Bus IPC
	// captures the secondary's args + cwd but drops env, so the primary
	// never sees the token and the compositor refuses to grant focus.
	// Pre-pend the token to os.Args so it travels through Wails'
	// SecondInstanceData and OnSecondInstanceLaunch can replay it.
	if tok := os.Getenv("XDG_ACTIVATION_TOKEN"); tok != "" {
		os.Args = append(os.Args, activationFlag, tok)
	}

	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		socketFlag  = flag.String("daemon-socket", "", "override daemon unix-socket path (default: $XDG_RUNTIME_DIR/bloodhound/api.sock)")
		width       = flag.Int("width", 1280, "initial window width")
		height      = flag.Int("height", 800, "initial window height")
		_           = flag.String("xdg-activation-token", "", "internal: Wayland activation token threaded through to the primary instance")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	socketPath := *socketFlag
	if socketPath == "" {
		p, err := routes.SocketPath()
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
			OnSecondInstanceLaunch: func(data options.SecondInstanceData) {
				if appCtx == nil {
					return
				}
				// Re-set XDG_ACTIVATION_TOKEN from the secondary's args so
				// GTK/GDK can pass it to the compositor when we present
				// the window. On X11 this is unnecessary but harmless; on
				// Wayland it's the only thing the compositor accepts as
				// permission to grant focus.
				if tok := activationTokenFromArgs(data.Args); tok != "" {
					_ = os.Setenv("XDG_ACTIVATION_TOKEN", tok)
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

// activationTokenFromArgs finds the value we appended to os.Args at
// startup. data.Args is os.Args[1:] from the secondary instance, so we
// look for either "--xdg-activation-token <value>" (two-arg form, what
// we emit) or "--xdg-activation-token=<value>" (defensive).
func activationTokenFromArgs(args []string) string {
	for i, a := range args {
		if a == activationFlag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, activationFlag+"=") {
			return strings.TrimPrefix(a, activationFlag+"=")
		}
	}
	return ""
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
