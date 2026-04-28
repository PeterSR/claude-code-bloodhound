package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the OS service appropriate to this platform",
	Long:  `Installs systemd user units (Linux), launchd agent (macOS), or Scheduled Tasks (Windows).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("install: not yet implemented")
	},
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the OS service installed by `bloodhound install`",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("uninstall: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
}
