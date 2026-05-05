package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/api"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var (
	serveHost string
	servePort int
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the web UI and JSON API server (deprecated — use 'bloodhound daemon')",
	Long: `Reads from the local SQLite database and serves the web UI plus its
backing API on the configured host:port (default 127.0.0.1:7777).

Deprecated: 'bloodhound daemon' now runs the same HTTP API alongside its
collection loop, so a separate 'serve' process is no longer needed. This
subcommand still works (useful for "API without polling" setups) but will
likely be removed in a future release.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fmt.Fprintln(cmd.ErrOrStderr(),
			"[serve] note: 'bloodhound daemon' now serves the API too; "+
				"prefer that unless you specifically want API-without-polling.")

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		host := serveHost
		if host == "" {
			host = cfg.Host
		}
		port := servePort
		if port == 0 {
			port = cfg.Port
		}

		s, err := store.Open(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintln(os.Stderr, "[serve] shutting down")
			cancel()
		}()

		srv := &api.Server{Store: s}
		return api.Run(ctx, host, port, srv)
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveHost, "host", "", "override config host (default 127.0.0.1)")
	serveCmd.Flags().IntVar(&servePort, "port", 0, "override config port (default 7777)")
	rootCmd.AddCommand(serveCmd)
}
