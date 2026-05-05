package statusline

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

func TestEtaToLimit_NeedsTwo(t *testing.T) {
	if _, ok := etaToLimit(nil, 50); ok {
		t.Error("nil should return ok=false")
	}
	if _, ok := etaToLimit([]store.PctPoint{{TSUnixMS: 0, Pct: 10}}, 50); ok {
		t.Error("single point should return ok=false")
	}
}

func TestEtaToLimit_LinearProjection(t *testing.T) {
	pts := []store.PctPoint{
		{TSUnixMS: 0, Pct: 0},
		{TSUnixMS: int64(time.Hour / time.Millisecond), Pct: 20},
	}
	got, ok := etaToLimit(pts, 20)
	if !ok {
		t.Fatal("ok=false")
	}
	want := 4 * time.Hour
	if abs := got - want; abs > 50*time.Millisecond || abs < -50*time.Millisecond {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEtaToLimit_FlatSlopeRejected(t *testing.T) {
	pts := []store.PctPoint{
		{TSUnixMS: 0, Pct: 50},
		{TSUnixMS: int64(time.Hour / time.Millisecond), Pct: 50},
	}
	if _, ok := etaToLimit(pts, 50); ok {
		t.Error("flat slope should return ok=false")
	}
}

func TestFormatBucket_PctOnly(t *testing.T) {
	got := formatBucket(72, "5h", "", nil, time.Now())
	if got != "72%/5h" {
		t.Errorf("want %q, got %q", "72%/5h", got)
	}
}

func TestFormatBucket_WithReset(t *testing.T) {
	now := time.Date(2026, 4, 29, 7, 38, 0, 0, time.UTC)
	resetIn := now.Add(3*time.Hour + 12*time.Minute).Format(time.RFC3339)
	got := formatBucket(72, "5h", resetIn, nil, now)
	if got != "72%/5h (3h12m)" {
		t.Errorf("want %q, got %q", "72%/5h (3h12m)", got)
	}
}

func TestFormatBucket_WarningWhenLimitBeforeReset(t *testing.T) {
	now := time.Date(2026, 4, 29, 7, 38, 0, 0, time.UTC)
	// Reset 3h away.
	resetISO := now.Add(3 * time.Hour).Format(time.RFC3339)
	// Two points, slope = 60%/h => from 72% to 100% takes ~28m.
	pts := []store.PctPoint{
		{TSUnixMS: now.Add(-1 * time.Hour).UnixMilli(), Pct: 12},
		{TSUnixMS: now.UnixMilli(), Pct: 72},
	}
	got := formatBucket(72, "5h", resetISO, pts, now)
	if got == "72%/5h (3h)" {
		t.Fatal("expected ⚠ warning, got plain reset string")
	}
	if want := "72%/5h ⚠100% in 28m (3h)"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatBucket_NoWarningWhenLimitAfterReset(t *testing.T) {
	now := time.Date(2026, 4, 29, 7, 38, 0, 0, time.UTC)
	// Reset 30 min away.
	resetISO := now.Add(30 * time.Minute).Format(time.RFC3339)
	// Slope = 5%/h => from 50% to 100% takes 10h, well after reset.
	pts := []store.PctPoint{
		{TSUnixMS: now.Add(-1 * time.Hour).UnixMilli(), Pct: 45},
		{TSUnixMS: now.UnixMilli(), Pct: 50},
	}
	got := formatBucket(50, "5h", resetISO, pts, now)
	if want := "50%/5h (30m)"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestTimeUntil_Past(t *testing.T) {
	now := time.Date(2026, 4, 29, 7, 38, 0, 0, time.UTC)
	past := now.Add(-1 * time.Hour).Format(time.RFC3339)
	if _, ok := timeUntil(past, now); ok {
		t.Error("past should return ok=false")
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{2 * time.Minute, "2m"},
		{59 * time.Minute, "59m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 12*time.Minute, "2h12m"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},
	}
	for _, c := range cases {
		if got := fmtDur(c.in); got != c.want {
			t.Errorf("fmtDur(%s): want %q, got %q", c.in, c.want, got)
		}
	}
}

func TestFmtDur_NegativeClamps(t *testing.T) {
	if got := fmtDur(-5 * time.Hour); !strings.HasPrefix(got, "0") {
		t.Errorf("negative duration should clamp to 0, got %q", got)
	}
}

func TestFormatActiveSegment_Nil(t *testing.T) {
	if got := formatActiveSegment(nil); got != "" {
		t.Errorf("nil insight should render empty, got %q", got)
	}
}

func TestFormatActiveSegment_Empty(t *testing.T) {
	if got := formatActiveSegment(&sessioninsight.Insight{}); got != "" {
		t.Errorf("empty insight should render empty, got %q", got)
	}
}

func TestFormatActiveSegment_ColdCacheWithPct(t *testing.T) {
	ins := &sessioninsight.Insight{
		ColdResumeCostCWTokens: 12345,
		ColdResumeCostPct:      4.7,
	}
	if got, want := formatActiveSegment(ins), "❄ cold +5%"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_ColdCacheSubPct(t *testing.T) {
	ins := &sessioninsight.Insight{
		ColdResumeCostCWTokens: 100,
		ColdResumeCostPct:      0.4,
	}
	if got, want := formatActiveSegment(ins), "❄ cold +<1%"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_ColdCacheNoCalibration(t *testing.T) {
	ins := &sessioninsight.Insight{
		ColdResumeCostCWTokens: 12345,
		// No ColdResumeCostPct (no calibration available).
	}
	if got, want := formatActiveSegment(ins), "❄ cold"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_CacheExpiring(t *testing.T) {
	cases := []struct {
		expiresIn int64
		want      string
	}{
		{47, "⏱ 47s"},
		{2 * 60, "⏱ 2m"},
		{72 * 60, "⏱ 1h12m"},
	}
	for _, c := range cases {
		ins := &sessioninsight.Insight{CacheExpiresInS: c.expiresIn}
		if got := formatActiveSegment(ins); got != c.want {
			t.Errorf("CacheExpiresInS=%d: want %q, got %q", c.expiresIn, c.want, got)
		}
	}
}

func TestFormatActiveSegment_CompactRecommendedWithPct(t *testing.T) {
	ins := &sessioninsight.Insight{
		Recommendation:     "compact",
		CompactCostPct:     12.0,
		Recent3AvgCWTokens: 30000,
		SessionAvgCWTokens: 10000,
	}
	if got, want := formatActiveSegment(ins), "⚠ /compact +12%"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_CompactRecommendedNoCalibration(t *testing.T) {
	ins := &sessioninsight.Insight{
		Recommendation:     "compact",
		Recent3AvgCWTokens: 30000,
		SessionAvgCWTokens: 10000,
	}
	if got, want := formatActiveSegment(ins), "⚠ /compact"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_Watch(t *testing.T) {
	ins := &sessioninsight.Insight{
		Recommendation:     "watch",
		Recent3AvgCWTokens: 17000,
		SessionAvgCWTokens: 10000,
	}
	if got, want := formatActiveSegment(ins), "↗ 1.7×"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_OkRecommendationSilent(t *testing.T) {
	ins := &sessioninsight.Insight{
		Recommendation:     "ok",
		Recent3AvgCWTokens: 10000,
		SessionAvgCWTokens: 10000,
	}
	if got := formatActiveSegment(ins); got != "" {
		t.Errorf("ok recommendation should render empty, got %q", got)
	}
}

func TestFormatActiveSegment_ColdOutranksCompact(t *testing.T) {
	// Both cold cache and a compact recommendation — cold wins because
	// the next turn pays it immediately.
	ins := &sessioninsight.Insight{
		ColdResumeCostCWTokens: 12345,
		ColdResumeCostPct:      5,
		Recommendation:         "compact",
		CompactCostPct:         12,
	}
	if got, want := formatActiveSegment(ins), "❄ cold +5%"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatActiveSegment_ExpiringOutranksCompact(t *testing.T) {
	ins := &sessioninsight.Insight{
		CacheExpiresInS: 30,
		Recommendation:  "compact",
		CompactCostPct:  12,
	}
	if got, want := formatActiveSegment(ins), "⏱ 30s"; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}
