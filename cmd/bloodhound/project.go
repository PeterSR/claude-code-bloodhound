package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
)

var projectInitForce bool

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Read and write the .bloodhound file a project keeps beside its code",
	Long: `A project's own .bloodhound/config.json says how bloodhound should behave
toward sessions working in that directory: whether to speak up before a
compaction, whether a warning may suggest arming a wakeup.

It is not an override of the global config and shares no keys with it. The
global config is how the daemon operates: where the database lives, which
claude binary to drive. A working directory has no say over any of that.
This file only makes bloodhound quieter or chattier in one project, which is
what makes it safe to check in.

Every key defaults to off. A directory with no file gets the defaults, which
is not an error and is the normal case.`,
}

var projectInitCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Write a starter .bloodhound/config.json",
	Long: `Writes every key out at its default value, which is the closest thing JSON
allows to a commented example. Defaults to the current directory.

Refuses to overwrite an existing file unless --force.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := targetDir(args)
		if err != nil {
			return err
		}

		path := filepath.Join(dir, projectconfig.Dir, projectconfig.File)
		if _, err := os.Stat(path); err == nil && !projectInitForce {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(projectconfig.Default(), "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
		fmt.Fprintln(cmd.OutOrStdout(), "every key is at its default; turn on what this project wants.")
		return nil
	},
}

var projectShowCmd = &cobra.Command{
	Use:   "show [dir]",
	Short: "Show the project config governing a directory, and where it came from",
	Long: `Resolves the same way a warning about to be delivered does: walk up from the
directory to the nearest .bloodhound/config.json, stopping at home. Defaults to
the current directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := targetDir(args)
		if err != nil {
			return err
		}

		cfg, found, err := projectconfig.Load(dir)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "directory:     %s\n", dir)
		if found.Path == "" {
			fmt.Fprintln(out, "config:        none found (defaults apply)")
		} else {
			fmt.Fprintf(out, "config:        %s\n", found.Path)
		}
		fmt.Fprintf(out, "writeup nudge: %v\n", cfg.WriteupNudge)
		fmt.Fprintf(out, "wakeup nudge:  %v\n", cfg.WakeupNudge)
		if len(found.UnknownKeys) > 0 {
			fmt.Fprintf(out, "unknown keys:  %s (ignored; the global config's keys do not work here)\n",
				strings.Join(found.UnknownKeys, ", "))
		}
		return nil
	},
}

// targetDir resolves the optional directory argument, defaulting to the
// working directory. Absolute, so what gets printed is what was searched.
func targetDir(args []string) (string, error) {
	if len(args) == 1 {
		return filepath.Abs(args[0])
	}
	return os.Getwd()
}

func init() {
	projectInitCmd.Flags().BoolVar(&projectInitForce, "force", false, "overwrite an existing file")
	projectCmd.AddCommand(projectInitCmd, projectShowCmd)
	rootCmd.AddCommand(projectCmd)
}
