package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/ingest"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var (
	ingestForce       bool
	ingestJSON        bool
	ingestProjectsDir string
)

var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Walk Claude Code's session JSONL files and update the local database",
	Long: `Scans <dir>/projects/*/*.jsonl for every dir in claude_dirs (default
~/.claude), parses each assistant turn and
compaction event, and upserts them into the local store. Files whose mtime
hasn't changed since the last ingest are skipped (use --force to re-process).

Mostly useful when you're not running ` + "`bloodhound daemon`" + ` — the daemon
performs ingest on its own cadence (default every 5 minutes). Running this
alongside the daemon is harmless (the store serializes writes) but redundant.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		s, err := store.Open(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		dirs, err := config.ClaudeConfigDirs(cfg)
		if err != nil {
			return err
		}
		opts := ingest.Options{
			ConfigDirs:  dirs,
			ProjectsDir: ingestProjectsDir,
			Force:       ingestForce,
		}
		stats, err := ingest.Run(ctx, s, opts)
		w := cmd.OutOrStdout()
		if ingestJSON {
			b, _ := json.MarshalIndent(stats, "", "  ")
			fmt.Fprintln(w, string(b))
			return err
		}
		fmt.Fprintf(w, "ingest: %d scanned, %d parsed (%d skipped mtime, %d skipped tiny)\n",
			stats.FilesScanned, stats.FilesParsed, stats.FilesSkippedMtime, stats.FilesSkippedSmall)
		fmt.Fprintf(w, "  +%d turns, +%d compactions in %.1fs\n",
			stats.TurnsAdded, stats.CompactionsAdded, stats.ElapsedS)
		for _, e := range stats.Errors {
			fmt.Fprintf(w, "  ERR: %s\n", e)
		}
		return err
	},
}

func init() {
	ingestCmd.Flags().BoolVar(&ingestForce, "force", false, "re-ingest even if mtime is unchanged")
	ingestCmd.Flags().BoolVar(&ingestJSON, "json", false, "emit JSON stats instead of human text")
	ingestCmd.Flags().StringVar(&ingestProjectsDir, "projects-dir", "",
		"override Claude Code projects dir (default ~/.claude/projects)")
	rootCmd.AddCommand(ingestCmd)
}
