//go:build linux && gui

// bloodhound-gui opens a native window pointing at the running daemon's
// HTTP API. Singleton: launching it twice raises the existing window
// (using the XDG activation token Gnome passes via env, when available).
//
// All actual data work — collection, persistence, the API itself —
// lives in the daemon. This binary is a viewer.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/guiapp"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

const (
	wmClass  = "bloodhound"
	iconName = "bloodhound"
	winTitle = "Bloodhound"
)

func init() {
	// GTK requires its calls on the main OS thread.
	runtime.LockOSThread()
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		urlOverride = flag.String("url", "", "override URL (default: http://<cfg.Host>:<cfg.Port>/)")
		width       = flag.Int("width", 1280, "initial window width")
		height      = flag.Int("height", 800, "initial window height")
		debug       = flag.Bool("debug", false, "verbose stderr logging")
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

	target := *urlOverride
	if target == "" {
		target = fmt.Sprintf("http://%s:%d/", cfg.Host, cfg.Port)
	}

	// Singleton bootstrap. A second launch sends a focus message to the
	// running primary then exits; the primary raises its window on the
	// GUI thread.
	isPrimary, cleanup, err := guiapp.Bootstrap()
	if err != nil {
		log.Fatalf("singleton: %v", err)
	}
	defer cleanup()

	if !isPrimary {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := guiapp.SendFocusToPrimary(ctx); err != nil && *debug {
			fmt.Fprintf(os.Stderr, "send focus: %v\n", err)
		}
		return
	}

	// Set program name BEFORE any GTK window is created so the resulting
	// window's WMClass matches the .desktop file's StartupWMClass and
	// the dash-to-dock groups it under our icon.
	setPrgName(wmClass)
	gtkInit()
	createWindow(winTitle, *width, *height, iconName)
	navigate(target)

	if err := guiapp.ListenForFocus(func(msg guiapp.FocusMessage) {
		gtkRequestRaise(msg.ActivationToken)
	}); err != nil && *debug {
		fmt.Fprintf(os.Stderr, "focus listener: %v\n", err)
	}

	// Translate signals into a graceful gtk_main_quit so cleanup runs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		gtkRequestQuit()
	}()

	gtkRun()
}
