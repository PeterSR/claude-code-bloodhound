package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Print build info, paths, config status, and DB schema version",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := cmd.OutOrStdout()

		fmt.Fprintf(w, "bloodhound %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		fmt.Fprintf(w, "go runtime: %s/%s (%s)\n", runtime.GOOS, runtime.GOARCH, runtime.Version())

		fmt.Fprintln(w, "\nPaths:")
		dataDir, err := config.DataDir()
		fmt.Fprintf(w, "  data dir:    %s%s\n", dataDir, errSuffix(err))
		stateDir, err := config.StateDir()
		fmt.Fprintf(w, "  state dir:   %s%s\n", stateDir, errSuffix(err))
		cfgDir, err := config.ConfigDir()
		fmt.Fprintf(w, "  config dir:  %s%s\n", cfgDir, errSuffix(err))
		cfgPath, err := config.Path()
		fmt.Fprintf(w, "  config file: %s%s\n", cfgPath, errSuffix(err))
		projects, err := config.ClaudeProjectsDir()
		exists := dirExists(projects)
		state := "missing"
		if exists {
			state = "found"
		}
		fmt.Fprintf(w, "  claude projects: %s (%s)%s\n", projects, state, errSuffix(err))

		fmt.Fprintln(w, "\nConfig:")
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(w, "  load: ERROR — %v\n", err)
		} else {
			fmt.Fprintf(w, "  host:port:        %s:%d\n", cfg.Host, cfg.Port)
			fmt.Fprintf(w, "  poll interval:    %ds\n", cfg.PollIntervalS)
			fmt.Fprintf(w, "  ingest interval:  %ds\n", cfg.IngestIntervalS)
			fmt.Fprintf(w, "  aggregate intvl:  %ds\n", cfg.AggregateIntervalS)
			fmt.Fprintf(w, "  plan tier:        %s (cosmetic)\n", cfg.PlanTier)
			fmt.Fprintf(w, "  join the pack:    %v\n", cfg.JoinThePack)
			binPath, binState := claudeBinaryStatus(cfg.ClaudeBinary)
			fmt.Fprintf(w, "  claude binary:    %s (%s)\n", binPath, binState)
		}

		fmt.Fprintln(w, "\nDatabase:")
		ctx := context.Background()
		s, err := store.Open(ctx)
		if err != nil {
			fmt.Fprintf(w, "  open: ERROR — %v\n", err)
			return err
		}
		defer s.Close()
		fmt.Fprintf(w, "  path: %s\n", s.Path)
		v, err := s.SchemaVersion(ctx)
		if err != nil {
			fmt.Fprintf(w, "  schema version: ERROR — %v\n", err)
		} else {
			fmt.Fprintf(w, "  schema version: %d\n", v)
		}
		id, err := s.DeviceID(ctx)
		if err != nil {
			fmt.Fprintf(w, "  device id: ERROR — %v\n", err)
		} else {
			fmt.Fprintf(w, "  device id: %s\n", id)
		}

		fmt.Fprintln(w, "\nExtractor:")
		ext, origin, exErr := usage.LoadExtractor()
		if exErr != nil {
			fmt.Fprintf(w, "  load: ERROR — %v\n", exErr)
		} else {
			extractorPath, snapshotPath, _ := usage.ExtractorPaths()
			fmt.Fprintf(w, "  origin:    %s\n", origin)
			fmt.Fprintf(w, "  version:   %d\n", ext.Version)
			if ext.GeneratedAt != "" {
				fmt.Fprintf(w, "  generated: %s by %s\n", ext.GeneratedAt, ext.GeneratedBy)
			}
			fmt.Fprintf(w, "  fields:    %d\n", len(ext.Fields))
			fmt.Fprintf(w, "  state:     %s\n", extractorPath)
			fmt.Fprintf(w, "  snapshot:  %s\n", snapshotPath)
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf(" (ERROR: %v)", err)
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func claudeBinaryStatus(override string) (string, string) {
	target := override
	if target == "" {
		target = "claude"
	}
	resolved, err := exec.LookPath(target)
	if err != nil {
		return target, "not found on PATH"
	}
	return resolved, "ok"
}
