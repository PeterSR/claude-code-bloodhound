package statusline

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

func TestEtaFromPoints_NeedsTwo(t *testing.T) {
	if _, ok := etaFromPoints(nil, 50); ok {
		t.Error("nil should return ok=false")
	}
	if _, ok := etaFromPoints([]store.PctPoint{{TSUnixMS: 0, Pct: 10}}, 50); ok {
		t.Error("single point should return ok=false")
	}
}

func TestEtaFromPoints_LinearProjection(t *testing.T) {
	// Two points one hour apart, 0% -> 20%.
	pts := []store.PctPoint{
		{TSUnixMS: 0, Pct: 0},
		{TSUnixMS: int64(time.Hour / time.Millisecond), Pct: 20},
	}
	// Currently at 20%; 80% remaining at 20%/h => 4 hours.
	got, ok := etaFromPoints(pts, 20)
	if !ok {
		t.Fatal("ok=false")
	}
	want := 4 * time.Hour
	if abs := got - want; abs > 50*time.Millisecond || abs < -50*time.Millisecond {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEtaFromPoints_FlatSlopeRejected(t *testing.T) {
	pts := []store.PctPoint{
		{TSUnixMS: 0, Pct: 50},
		{TSUnixMS: int64(time.Hour / time.Millisecond), Pct: 50},
	}
	if _, ok := etaFromPoints(pts, 50); ok {
		t.Error("flat slope should return ok=false")
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
