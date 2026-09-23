package announce

import (
	"strings"
	"testing"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

const lowPriSession = "session-abc12345"

func saturationEvent() events.Event {
	return ev("saturation.saturated", "session", "", map[string]any{
		"pct":      float64(100),
		"reset_ts": iso(testNow.Add(70 * time.Minute)),
	})
}

func lowPriVerdict() store.QuotaVerdict {
	return store.QuotaVerdict{
		Refused:               true,
		RefusedAtMS:           testNow.Add(-10 * time.Minute).UnixMilli(),
		Bucket:                "session",
		ResetTSUnixMS:         testNow.Add(70 * time.Minute).UnixMilli(),
		OverageStatus:         "rejected",
		OverageDisabledReason: "out_of_credits",
		LowPriorityOffered:    true,
		LowPriorityActive:     true,
		LowPrioritySinceMS:    testNow.Add(-9 * time.Minute).UnixMilli(),
		LowPriorityUntilMS:    testNow.Add(70 * time.Minute).UnixMilli(),
		LowPrioritySessions:   []string{lowPriSession},
	}
}

func TestASessionOnLowPriorityIsToldItIsStillWorking(t *testing.T) {
	// The failure this exists to stop: a session reads "the meter has stopped
	// moving", takes it for a stop, writes up where it got to and waits out
	// the window, while every request it would have made was going through.
	got := describe(saturationEvent(), testNow, lowPriVerdict(), lowPriSession)
	if !strings.Contains(got, "still going through") {
		t.Fatalf("line does not say the session can keep working:\n%s", got)
	}
	if !strings.Contains(got, "weekly allowance") {
		t.Errorf("line does not say what is paying for it:\n%s", got)
	}
}

func TestASessionThatIsActuallyRefusedIsToldSoAndToldTheOffer(t *testing.T) {
	v := lowPriVerdict()
	v.LowPriorityActive = false
	v.LowPrioritySessions = nil

	got := describe(saturationEvent(), testNow, v, lowPriSession)
	if !strings.Contains(got, "refused") {
		t.Fatalf("line does not say requests are being refused:\n%s", got)
	}
	if !strings.Contains(got, "out of credits") {
		t.Errorf("line does not say why extra usage is not absorbing it:\n%s", got)
	}
	if !strings.Contains(got, "lower request priority") {
		t.Errorf("line does not mention the offer that is on the reader's screen:\n%s", got)
	}
}

func TestExtraUsageIsOnlyClaimedWhenItIsActuallyBilling(t *testing.T) {
	v := lowPriVerdict()
	v.LowPriorityActive = false
	v.LowPrioritySessions = nil
	v.UsingOverage = true

	got := describe(saturationEvent(), testNow, v, lowPriSession)
	if !strings.Contains(got, "extra usage") {
		t.Fatalf("line does not name the tier the spend is landing on:\n%s", got)
	}
	if strings.Contains(got, "Requests are being refused") {
		t.Errorf("line claims a refusal that did not happen:\n%s", got)
	}
}

func TestSaturationSaysNothingExtraWithoutEvidence(t *testing.T) {
	// The ordinary case. bloodhound has scraped a percentage and nothing
	// else, so the sentence has to stand exactly as it did before any of
	// this existed.
	got := describe(saturationEvent(), testNow, store.QuotaVerdict{}, lowPriSession)
	for _, forbidden := range []string{"refused", "extra usage", "lower request priority"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("line claims %q with no signal behind it:\n%s", forbidden, got)
		}
	}
}

func TestAWeekEventDoesNotInheritAFiveHourRefusal(t *testing.T) {
	e := ev("saturation.saturated", "week", "", map[string]any{"pct": float64(100)})
	got := describe(e, testNow, lowPriVerdict(), lowPriSession)
	if strings.Contains(got, "still going through") {
		t.Errorf("a five hour refusal was applied to the weekly line:\n%s", got)
	}
}

func TestNoWakeupIsPromisedToASessionThatIsNotStopping(t *testing.T) {
	// Both wakeup forms open with "work that stops here". Nothing stops for a
	// session running at the lower priority, so the nudge would be a false
	// warning and the promise would be a wakeup for a conversation that never
	// paused.
	at := testNow.Add(70 * time.Minute)

	line, plan := wakeupLine(wantsResume, ccsock.Session{SessionID: lowPriSession}, testNow, at, "session", "", lowPriVerdict())
	if line != "" || plan != nil {
		t.Errorf("promised a wakeup to a working session: %q / %+v", line, plan)
	}

	nudge, _ := wakeupLine(wantsNudge, ccsock.Session{SessionID: lowPriSession}, testNow, at, "session", "", lowPriVerdict())
	if nudge != "" {
		t.Errorf("warned a working session it was about to stop: %q", nudge)
	}

	// A different session in the same batch never accepted the offer, so it
	// still gets the far side of the warning.
	other, otherPlan := wakeupLine(wantsResume, ccsock.Session{SessionID: "session-def67890"}, testNow, at, "session", "", lowPriVerdict())
	if other == "" || otherPlan == nil {
		t.Errorf("a session that really is stopping lost its wakeup: %q / %+v", other, otherPlan)
	}
}
