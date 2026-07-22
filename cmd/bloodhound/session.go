package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// claudeCodeSessionEnvVar is the environment variable Claude Code injects
// into every process it spawns: a bash tool call, a hook, an MCP server,
// its own statusline command. Verified live to hold the exact session UUID,
// needing no /proc walk and working the same on every platform.
//
// The asymmetry that matters here: Claude Code's OWN process never carries
// this (injected into children only, confirmed by reading its environment
// directly), so this only ever answers "which session spawned ME", never
// "which session is my parent". That's exactly the question a bloodhound
// command run from inside a session needs answered, and no other.
//
// Deliberately no process-tree fallback. It was designed and rejected: an
// idle session left open while another is actively used gets misidentified
// (its process started first, but its last record is oldest, and nearest-
// match assigns it to whichever session is actually being typed in), and a
// dev machine with several concurrent `claude` processes makes that
// ambiguity the normal case, not an edge case. Reporting "unknown" is
// strictly better than guessing wrong with confidence.
const claudeCodeSessionEnvVar = "CLAUDE_CODE_SESSION_ID"

var sessionJSON bool

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Print the current Claude Code session's UUID",
	Long: `Reads ` + claudeCodeSessionEnvVar + ` from THIS process's own environment: the
exact session UUID Claude Code injects into every process it spawns. Only
works when bloodhound itself is running as one of those children (a bash
tool call, a hook, an MCP server) - a plain shell, or a script launched some
other way, has no such variable and this command reports that plainly
rather than guessing.

Prints the UUID alone on stdout, so it pipes cleanly, e.g.:

  bloodhound attribution session "$(bloodhound session)"

though "bloodhound attribution session" with no argument now does this
itself; see its own help.

--json adds the session's project and, if aggregate has materialized this
session, its cwd and limit-meter attribution totals. Kept small on purpose:
this command is meant to be cheap enough to shell out to as a default, not
a second /api/sessions/{uuid}.`,
	Args: cobra.NoArgs,
	RunE: runSession,
}

func init() {
	sessionCmd.Flags().BoolVar(&sessionJSON, "json", false,
		"also emit project, cwd and attribution totals (small; not a full session detail dump)")
	rootCmd.AddCommand(sessionCmd)
}

// currentSessionUUID reads claudeCodeSessionEnvVar from the calling
// process's own environment. Never reads /proc/<pid>/environ, here or
// anywhere else in shipped code: it can contain secrets, and the doc
// comment above already covers why walking to a parent wouldn't help even
// if it were safe to read.
func currentSessionUUID() (string, bool) {
	v := os.Getenv(claudeCodeSessionEnvVar)
	return v, v != ""
}

func runSession(cmd *cobra.Command, args []string) error {
	uuid, ok := currentSessionUUID()
	if !ok {
		return fmt.Errorf(
			"not running inside a Claude Code session: %s is not set. "+
				"This only works from a process Claude Code itself spawned "+
				"(a bash tool call, a hook, an MCP server) - not from a plain shell",
			claudeCodeSessionEnvVar)
	}

	if !sessionJSON {
		fmt.Fprintln(cmd.OutOrStdout(), uuid)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	out, err := buildSessionJSON(ctx, s, uuid)
	if err != nil {
		return err
	}
	return writeJSONOut(cmd.OutOrStdout(), out)
}

// sessionJSONOut is the --json shape: small by design (see the command's
// help). Known separates "this session has no row yet" (still being
// written, or aggregate hasn't run since) from an actually-empty value, so
// a consumer doesn't have to guess which "" means.
type sessionJSONOut struct {
	SessionUUID string                     `json:"session_uuid"`
	Known       bool                       `json:"known"`
	Project     string                     `json:"project,omitempty"`
	Cwd         string                     `json:"cwd,omitempty"`
	Attribution *routes.SessionAttribution `json:"attribution,omitempty"`
}

// buildSessionJSON reads the materialized sessions row for uuid (written by
// `bloodhound aggregate`, not derived here by re-sanitizing a live path:
// project's sanitization is already documented elsewhere in this codebase
// as irreversible, and hand-rolling a second implementation of it risks
// silently disagreeing with the one Claude Code itself used on disk).
// Attribution is looked up the same way the CLI's `attribution session`
// command already does, so the two commands can never disagree about what
// one session's totals are.
func buildSessionJSON(ctx context.Context, s *store.Store, uuid string) (sessionJSONOut, error) {
	out := sessionJSONOut{SessionUUID: uuid}

	err := s.DB.QueryRowContext(ctx,
		`SELECT project, cwd FROM sessions WHERE session_uuid = ?`, uuid,
	).Scan(&out.Project, &out.Cwd)
	switch {
	case err == nil:
		out.Known = true
	case errors.Is(err, sql.ErrNoRows):
		// Not yet ingested and aggregated (a session still being written, or
		// one `bloodhound aggregate` hasn't reached yet): report unknown
		// rather than guess at a project or cwd we don't actually have.
		return out, nil
	default:
		return out, err
	}

	totals, err := s.SessionPctTotalsAll(ctx)
	if err != nil {
		return out, err
	}
	if t, ok := totals[uuid]; ok && t != nil {
		attr := sessionAttributionFromStore(t)
		out.Attribution = &attr
	}
	return out, nil
}
