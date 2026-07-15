package usage

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// synthPanel builds a raw byte stream that mimics claude printing a /usage
// panel: the session/week gauges at the top, then a "what's contributing"
// section tall enough to push the gauges past fillerLines. Lines are CRLF
// separated so each advances a row (scrolling the buffer once full).
func synthPanel(fillerLines int) []byte {
	lines := []string{
		"  Current session",
		"  █████████▌                     42% used",
		"  Resets 9:00am (UTC)",
		"",
		"  Current week (all models)",
		"  ██████████████                 58% used",
		"  Resets Jul 3, 1am (UTC)",
		"",
		"  What's contributing to your limits",
	}
	for i := 0; i < fillerLines; i++ {
		lines = append(lines, "  some contributing detail line describing usage")
	}
	return []byte(strings.Join(lines, "\r\n"))
}

func hasGauges(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "current session") &&
		strings.Contains(l, "42% used") &&
		strings.Contains(l, "resets 9:00am (utc)") &&
		strings.Contains(l, "current week") &&
		strings.Contains(l, "58% used")
}

// visibleOnly renders just the visible grid, reproducing RenderVT's behaviour
// before scrollback was read. Used to prove the tall-panel gauges genuinely
// scroll off the viewport (so the fix, not the fixture, is what recovers them).
func visibleOnly(raw []byte) string {
	e := vt.NewEmulator(VTCols, VTRows)
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, e); close(done) }()
	_, _ = e.Write(raw)
	_ = e.Close()
	<-done

	var b strings.Builder
	for y := 0; y < VTRows; y++ {
		var row strings.Builder
		for x := 0; x < VTCols; x++ {
			c := e.CellAt(x, y)
			if c == nil || c.String() == "" {
				row.WriteByte(' ')
				continue
			}
			row.WriteString(c.String())
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// A panel that overflows the 60-row viewport must still yield its gauges,
// because RenderVT reads scrollback. This is the regression guard for the
// "extraction failed while self-heal loops forever" capture-truncation bug.
func TestRenderVT_TallPanelRecoveredFromScrollback(t *testing.T) {
	raw := synthPanel(80) // 9 header lines + 80 => well past VTRows (60)

	// Precondition: the gauges really did scroll off the visible grid.
	if hasGauges(visibleOnly(raw)) {
		t.Fatal("test fixture too short: gauges still visible, not exercising scrollback")
	}
	// The fix: RenderVT recovers them from scrollback.
	got := RenderVT(raw)
	if !hasGauges(got) {
		t.Fatalf("RenderVT dropped the gauges that scrolled off the viewport:\n%s", got)
	}
}

// A short panel that fits in the viewport keeps rendering its gauges, and the
// scrollback path adds no duplication that breaks extraction.
func TestRenderVT_ShortPanelStillRendersGauges(t *testing.T) {
	raw := synthPanel(2) // fits comfortably in 60 rows
	got := RenderVT(raw)
	if !hasGauges(got) {
		t.Fatalf("RenderVT lost gauges on a short panel:\n%s", got)
	}
}

// End-to-end: the bundled default extractor must pull every required field
// from a tall panel once RenderVT reads scrollback. This is the actual
// pipeline (RenderVT -> Extractor.Apply) that the daemon poll runs.
func TestRenderVT_TallPanelExtractsAllFields(t *testing.T) {
	ex := loadDefault(t) // from usage_test.go
	out := ex.Apply(RenderVT(synthPanel(80)))
	if len(out.Missing) != 0 {
		t.Fatalf("required fields missing after tall-panel render: %v", out.Missing)
	}
	if got := out.Values["session_pct"]; got != 42 {
		t.Errorf("session_pct: want 42, got %v (%T)", got, got)
	}
	if got := out.Values["week_pct"]; got != 58 {
		t.Errorf("week_pct: want 58, got %v (%T)", got, got)
	}
}
