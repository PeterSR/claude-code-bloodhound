package budget

import (
	"testing"
	"time"
)

func b(spend, meter float64) Budget {
	return Budget{Cwd: "/home/dev/myapp", Bucket: BucketWeek, SpendPct: spend, MeterPct: meter}
}

func TestEvaluateSpendRule(t *testing.T) {
	tests := []struct {
		name      string
		allowed   float64
		used      float64
		wantState string
	}{
		{"well inside", 20, 5, StateClear},
		{"just under the tight line", 20, 14.9, StateClear},
		{"at the tight line", 20, 15, StateTight},
		{"nearly gone", 20, 19.9, StateTight},
		{"exactly spent", 20, 20, StateExceeded},
		{"overspent", 20, 31, StateExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Evaluate(b(tc.allowed, 0), Window{AttributedPct: tc.used, AttributedKnown: true})
			if p.State != tc.wantState {
				t.Errorf("state = %q, want %q", p.State, tc.wantState)
			}
			if tc.wantState != StateClear && p.Rule != "spend" {
				t.Errorf("rule = %q, want spend", p.Rule)
			}
		})
	}
}

func TestEvaluateMeterRuleIgnoresAttribution(t *testing.T) {
	// The whole point of the meter rule: it fires on the shared reading even
	// though this directory caused none of the movement.
	p := Evaluate(b(0, 70), Window{Pct: 80, PctKnown: true, AttributedPct: 0, AttributedKnown: true})
	if p.State != StateExceeded {
		t.Fatalf("state = %q, want %q", p.State, StateExceeded)
	}
	if p.Rule != "meter" {
		t.Errorf("rule = %q, want meter", p.Rule)
	}
}

func TestEvaluateMissingInputsYieldUnknownNotClear(t *testing.T) {
	// Saying "clear" off a measurement that does not exist invents headroom,
	// which is the precise failure a budget exists to prevent.
	t.Run("spend rule without attribution", func(t *testing.T) {
		p := Evaluate(b(20, 0), Window{Pct: 10, PctKnown: true})
		if p.State != "" {
			t.Errorf("state = %q, want empty (unknown)", p.State)
		}
	})
	t.Run("meter rule without a reading", func(t *testing.T) {
		p := Evaluate(b(0, 70), Window{AttributedPct: 1, AttributedKnown: true})
		if p.State != "" {
			t.Errorf("state = %q, want empty (unknown)", p.State)
		}
	})
}

func TestEvaluateSeverityWins(t *testing.T) {
	// Both rules set, and they disagree. The worse one decides and Rule says
	// which, so a reader is never told the milder half of the story.
	//
	// Spend is spent (21 of 20) while the shared meter is nowhere near the 90
	// this directory stops at, so spend must carry it.
	w := Window{Pct: 20, PctKnown: true, AttributedPct: 21, AttributedKnown: true}
	p := Evaluate(b(20, 90), w)
	if p.State != StateExceeded || p.Rule != "spend" {
		t.Errorf("state/rule = %q/%q, want exceeded/spend", p.State, p.Rule)
	}

	// And the milder rule does not downgrade the worse one: spend tight plus
	// meter clear still reports tight.
	mixed := Evaluate(b(20, 90), Window{Pct: 20, PctKnown: true, AttributedPct: 16, AttributedKnown: true})
	if mixed.State != StateTight || mixed.Rule != "spend" {
		t.Errorf("state/rule = %q/%q, want tight/spend", mixed.State, mixed.Rule)
	}

	w2 := Window{Pct: 95, PctKnown: true, AttributedPct: 1, AttributedKnown: true}
	p2 := Evaluate(b(50, 90), w2)
	if p2.State != StateExceeded || p2.Rule != "meter" {
		t.Errorf("state/rule = %q/%q, want exceeded/meter", p2.State, p2.Rule)
	}
}

func TestEvaluateReportsRemaining(t *testing.T) {
	p := Evaluate(b(20, 0), Window{AttributedPct: 8, AttributedKnown: true})
	if p.SpentPct != 8 || p.RemainingPct != 12 {
		t.Errorf("spent/remaining = %v/%v, want 8/12", p.SpentPct, p.RemainingPct)
	}
	// Overspend floors at zero rather than going negative, which reads wrong
	// in a gauge.
	over := Evaluate(b(20, 0), Window{AttributedPct: 25, AttributedKnown: true})
	if over.RemainingPct != 0 {
		t.Errorf("remaining = %v, want 0", over.RemainingPct)
	}
}

func TestValidate(t *testing.T) {
	live := []Lease{{Kind: LeaseManual}}
	tests := []struct {
		name    string
		b       Budget
		wantErr bool
	}{
		{"spend only", Budget{Bucket: BucketWeek, SpendPct: 10, Leases: live}, false},
		{"meter only", Budget{Bucket: BucketSession, MeterPct: 70, Leases: live}, false},
		{"neither rule", Budget{Bucket: BucketWeek, Leases: live}, true},
		{"bad bucket", Budget{Bucket: "5h", SpendPct: 10, Leases: live}, true},
		{"out of range", Budget{Bucket: BucketWeek, SpendPct: 101, Leases: live}, true},
		{"no lease", Budget{Bucket: BucketWeek, SpendPct: 10}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.b); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateRejectsAttributeSpelling(t *testing.T) {
	// The two layers disagree on the name of this bucket and the budget layer
	// uses "session". Accepting "5h" here would store a bucket the sensor
	// never queries, and the budget would silently measure nothing.
	if err := Validate(Budget{Bucket: "5h", SpendPct: 10, Leases: []Lease{{Kind: LeaseManual}}}); err == nil {
		t.Fatal("Validate accepted the attribution spelling of the bucket")
	}
	if got := AttributeBucket(BucketSession); got != "5h" {
		t.Errorf("AttributeBucket(session) = %q, want 5h", got)
	}
	if got := AttributeBucket(BucketWeek); got != "week" {
		t.Errorf("AttributeBucket(week) = %q, want week", got)
	}
}

func TestSettleLeasesKeepsBudgetAliveWhileAnyHolds(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour).UnixMilli()

	bud := Budget{Leases: []Lease{
		{ID: 1, Kind: LeaseDeadline, AtMS: &past},
		{ID: 2, Kind: LeaseManual},
	}}
	if live := SettleLeases(&bud, now, nil); !live {
		t.Fatal("budget died while a manual lease still held")
	}
	if bud.Leases[0].ExpiredMS == nil {
		t.Error("the passed deadline was not expired")
	}
	if bud.Leases[1].ExpiredMS != nil {
		t.Error("the manual lease was expired, but it never runs out")
	}
}

func TestSettleLeasesRetiresWhenTheLastRunsOut(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute).UnixMilli()
	bud := Budget{Leases: []Lease{{ID: 1, Kind: LeaseDeadline, AtMS: &past}}}
	if live := SettleLeases(&bud, now, nil); live {
		t.Fatal("budget stayed live with every lease run out")
	}
	if bud.Live() {
		t.Error("Live() disagrees with SettleLeases")
	}
}

func TestWindowResetLeaseWithNoWindowOpenHolds(t *testing.T) {
	// The governor lost a release to this. A lease bound when no window was
	// open must not expire on the first window that opens, because the first
	// window to open is the one the user is about to work in, which is exactly
	// what they set the budget for.
	now := time.Now()
	bound := now.UnixMilli()
	bud := Budget{Leases: []Lease{
		{ID: 1, Kind: LeaseWindowReset, Bucket: BucketSession, BoundAfterMS: &bound},
	}}

	// A window that ended before the lease was bound must not end it.
	stale := map[string]int64{BucketSession: bound - 1000}
	if live := SettleLeases(&bud, now, stale); !live {
		t.Fatal("a window that closed before binding expired the lease")
	}

	// One that opens after binding and has not yet ended: still held.
	future := map[string]int64{BucketSession: now.Add(time.Hour).UnixMilli()}
	if live := SettleLeases(&bud, now, future); !live {
		t.Fatal("an open window expired the lease before it turned over")
	}

	// And once that window's end has passed, it releases.
	later := now.Add(2 * time.Hour)
	if live := SettleLeases(&bud, later, future); live {
		t.Fatal("the lease survived the window it was bound to")
	}
}

func TestWindowResetLeaseBoundWithAWindowOpenIsPureClock(t *testing.T) {
	now := time.Now()
	end := now.Add(time.Hour).UnixMilli()
	bud := Budget{Leases: []Lease{{ID: 1, Kind: LeaseWindowReset, Bucket: BucketSession, WindowEndMS: &end}}}

	// No window map at all: a lease that already knows its end needs no meter.
	if live := SettleLeases(&bud, now, nil); !live {
		t.Fatal("lease expired before its known window end")
	}
	if live := SettleLeases(&bud, now.Add(2*time.Hour), nil); live {
		t.Fatal("lease survived its known window end")
	}
}

func TestNormalizeCwd(t *testing.T) {
	got, err := NormalizeCwd("/home/dev/myapp/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/dev/myapp" {
		t.Errorf("got %q, want the trailing separator gone", got)
	}
	if _, err := NormalizeCwd("  "); err == nil {
		t.Error("empty directory accepted")
	}
}

func TestLabelUsesTheLastElement(t *testing.T) {
	if got := Label("/home/dev/myapp"); got != "myapp" {
		t.Errorf("Label = %q, want myapp", got)
	}
	if got := Label(""); got != "this directory" {
		t.Errorf("Label(empty) = %q", got)
	}
}
