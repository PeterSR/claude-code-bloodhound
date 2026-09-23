package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/nowstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var nowJSON bool

var nowCmd = &cobra.Command{
	Use:   "now",
	Short: "Show pool state: percentage, reset, burn rate, and poll freshness",
	Long: `Reads the same values GET /api/now serves straight out of SQLite (no
daemon socket needed, so this works with the daemon stopped): current
percentage per limit bucket, its window reset and time to that reset, burn
rate, saturation, and how old the last /usage poll is.

internal/nowstate computes the payload; this command and the HTTP handler
both call it, so the two surfaces cannot drift on a number a downstream
consumer might be comparing across them. In particular, the burn rate is
omitted rather than reported as 0 whenever there's no usable slope (too
few points, or a baseline too short to out-signal /usage's integer
rounding) — a literal 0 would read as "measured calm" instead of "couldn't
measure." --json below carries that same omission.

--json emits the routes.NowResponse struct GET /api/now returns, so a
consumer can switch transports without reshaping its parser. It carries
the pool-state fields (ok, session, week, last_poll, server_now_ms) plus
poll_interval_s and stale_after_s: nowstate.Compute loads config and sets
those two itself, for both surfaces, so they're never missing here the way
they used to be. The active-session-threshold and recent-session-window
config hints, the in-window chart history, and the recent-sessions panel
are still Now-page-only additions the HTTP handler layers on afterward, so
those remain absent here rather than zeroed.`,
	Args: cobra.NoArgs,
	RunE: runNow,
}

func init() {
	nowCmd.Flags().BoolVar(&nowJSON, "json", false, "emit JSON matching GET /api/now's pool-state fields")
	rootCmd.AddCommand(nowCmd)
}

func runNow(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	acct, err := cliAccount(ctx, s)
	if err != nil {
		return err
	}
	out, err := nowstate.Compute(ctx, s, acct, time.Now())
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	if nowJSON {
		return writeJSONOut(w, out)
	}
	return writeNowHuman(w, out)
}

func writeNowHuman(w io.Writer, out *routes.NowResponse) error {
	if out.LastPoll == nil {
		fmt.Fprintln(w, "no /usage observation captured yet. Run `bloodhound poll` (or start the daemon) to capture one.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BUCKET\tUSED\tLEFT\tRESETS\tBURN")
	if out.Session != nil {
		writeNowBucketRow(tw, "5 hour", out.Session)
	}
	if out.Week != nil {
		writeNowBucketRow(tw, "weekly", out.Week)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "last poll %s ago%s\n", fmtAgo(out.LastPoll.AgeS), pollNote(out.LastPoll))
	return nil
}

// capStateLabel puts the cap state in the table's own register: short,
// lowercase, and readable in a parenthetical next to a percentage.
func capStateLabel(state string) string {
	switch state {
	case routes.CapLowPriority:
		return "low priority"
	case routes.CapExtraUsage:
		return "extra usage"
	case routes.CapRefused:
		return "refused"
	}
	return state
}

func writeNowBucketRow(tw *tabwriter.Writer, label string, ws *routes.NowWindow) {
	// "saturated" says the number stopped moving; the cap state says what
	// that turned out to mean, and it is the more useful of the two whenever
	// bloodhound knows it. Only one of them goes in the column: printing
	// both would repeat the same fact in two vocabularies.
	used := fmtPct(float64(ws.Pct))
	switch {
	case ws.CapState != "":
		used += " (" + capStateLabel(ws.CapState) + ")"
	case ws.Saturated:
		used += " (saturated)"
	}
	left := fmtPct(float64(100 - ws.Pct))

	resets := "-"
	if ws.ResetTSISO != "" {
		if t, err := time.Parse(time.RFC3339, ws.ResetTSISO); err == nil {
			resets = fmt.Sprintf("%s (%s)", t.Local().Format("Mon 02 Jan 15:04"), fmtHoursOut(ws.TimeToResetMS))
		}
	}

	// BurnOK false means no usable slope was available (fewer than two
	// points since the last reset, or a baseline too short to out-signal
	// rounding) — "n/a", never "0%/hour". See internal/nowstate/burn.go.
	burn := "n/a"
	if ws.BurnOK {
		burn = fmt.Sprintf("%.2f%%/hour", ws.BurnPctPerHour)
		if ws.LimitOK {
			burn += fmt.Sprintf(", 100%% in %s", fmtHoursOut(ws.LimitETAMS))
		}
	}

	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", label, used, left, resets, burn)
}

// fmtHoursOut renders a millisecond duration the way a reader sizing up a
// reset wants it: one decimal place of hours, matching how long-lived a
// limit window actually is (5h or 7 days), not seconds or minutes.
func fmtHoursOut(ms int64) string {
	if ms <= 0 {
		return "now"
	}
	return fmt.Sprintf("%.1fh", float64(ms)/3_600_000)
}

// fmtAgo renders the last-poll age in the same coarse-to-fine style as
// fmtHoursOut: seconds under a minute, otherwise minutes.
func fmtAgo(ageS int64) string {
	if ageS < 0 {
		ageS = 0
	}
	if ageS < 60 {
		return fmt.Sprintf("%ds", ageS)
	}
	return fmt.Sprintf("%dm", ageS/60)
}

// pollNote flags a failed extraction inline, since a stale-looking bucket
// table might otherwise just look like a slow burn rate rather than a
// poll that didn't parse.
func pollNote(p *routes.NowPoll) string {
	if !p.ParseOK {
		return " (last extraction failed)"
	}
	return ""
}
