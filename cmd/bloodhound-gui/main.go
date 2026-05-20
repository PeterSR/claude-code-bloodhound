// bloodhound-gui opens a native window with the dashboard, talking to a
// running daemon over HTTP. The React bundle is embedded; an asset-server
// middleware proxies /api/* to the daemon so the React app stays
// single-origin and the daemon needs no CORS configuration.
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

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
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
		urlOverride = flag.String("daemon-url", "", "override daemon URL (default: http://<cfg.Host>:<cfg.Port>)")
		width       = flag.Int("width", 1280, "initial window width")
		height      = flag.Int("height", 800, "initial window height")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	daemonURL := *urlOverride
	if daemonURL == "" {
		daemonURL = fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	}
	target, err := url.Parse(daemonURL)
	if err != nil {
		log.Fatalf("daemon-url: %v", err)
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
			Middleware: apiProxyMiddleware(target),
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

// apiProxyMiddleware reverse-proxies /api/* requests to the daemon. Any
// non-/api request falls through to the next handler (Wails' static asset
// server, which serves the embedded React bundle).
func apiProxyMiddleware(target *url.URL) assetserver.Middleware {
	proxy := httputil.NewSingleHostReverseProxy(target)
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
