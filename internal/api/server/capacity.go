package server

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
)

const (
	// capDefaultWeeks / capMaxWeeks bound the retrospective window. Independent
	// of the History page's day selector: "weekly capacity" only means
	// something across several weekly windows.
	capDefaultWeeks = 8
	capMaxWeeks     = 26

	// capWorkSessionPct: a 5h window that moved the weekly meter less than
	// this is a brief check-in, not a work session. Only work sessions feed
	// the "typical session" median, so light-touch windows can't drag it down
	// and inflate the sessions-per-week figure.
	capWorkSessionPct = 3.0

	// capMinSessions: below this many complete work sessions the median is too
	// noisy to publish a sessions-per-week number.
	capMinSessions = 3
)

// Window-reconstruction thresholds (how far ahead a reset may plausibly sit,
// how far it must jump to count as a genuine rotation) are attribute's to
// own; using attribute.WeekAhead / SessAhead / WeekMinGap / SessMinGap here
// instead of a local copy is what keeps this reconstruction and attribute's
// from ever disagreeing about where a window begins.

// capObs is the slice of a /usage observation the capacity reconstruction
// needs. The reset timestamps are the *advertised* boundaries from the panel,
// which identify limit windows far more reliably than the reset-detected
// flags (those are tuned for breaking chart lines and over-fire on misparses).
type capObs struct {
	tsMS        int64
	weekPct     *int
	weekValid   bool
	weekSat     bool
	sessResetTS string // raw RFC3339 from session_reset_ts, or ""
	weekResetTS string // raw RFC3339 from week_reset_ts, or ""
}

func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acct, ok := s.withAccount(w, r)
	if !ok {
		return
	}

	weeks := capDefaultWeeks
	if v := r.URL.Query().Get("weeks"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= capMaxWeeks {
			weeks = n
		}
	}
	cutoff := time.Now().Add(-time.Duration(weeks) * 7 * 24 * time.Hour).UnixMilli()

	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, week_pct, week_pct_valid, week_saturated,
		       session_reset_ts, week_reset_ts
		FROM usage_observations
		WHERE account_id = ? AND ts_unix_ms >= ?
		ORDER BY ts_unix_ms ASC
	`, acct, cutoff)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()

	var obs []capObs
	for rows.Next() {
		var (
			tsMS           int64
			wp             sql.NullInt64
			wvalid, wsat   int
			sReset, wReset sql.NullString
		)
		if err := rows.Scan(&tsMS, &wp, &wvalid, &wsat, &sReset, &wReset); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		o := capObs{
			tsMS:        tsMS,
			weekValid:   wvalid == 1,
			weekSat:     wsat == 1,
			sessResetTS: sReset.String,
			weekResetTS: wReset.String,
		}
		if wp.Valid {
			v := int(wp.Int64)
			o.weekPct = &v
		}
		obs = append(obs, o)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	out := weeklyCapacity(obs)
	out.WindowWeeks = weeks

	// Secondary "maxed ceiling" reference: a fully-used 5h session is worth
	// tokens_per_pct(session) tokens per point, the week tokens_per_pct(week);
	// their ratio is how many maxed sessions fill a week. Free here since the
	// calibration medians are already computed.
	if sc, _, sn, ok1, _ := s.Store.LatestCalibrationMedian(ctx, acct, "session", 10); ok1 && sc > 0 && sn > 0 {
		if wc, _, wn, ok2, _ := s.Store.LatestCalibrationMedian(ctx, acct, "week", 10); ok2 && wc > 0 && wn > 0 {
			out.MaxedSessionsPerWeek = round2(wc / sc)
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// weeklyCapacity reconstructs 5h work sessions from the /usage percentage
// series and measures how much of the weekly limit each consumed, so History
// can answer "how many typical sessions fit in a week."
//
// Windows are identified by the advertised reset timestamp: consecutive
// observations counting down to the same weekly reset are one week, and a new
// week begins only when that reset jumps forward far enough to be a genuine
// rotation rather than a misparse. The 5h session reset bounds a session the
// same way. A session's weekly cost is how much it lifted the running weekly
// peak. Real weekly usage only rises within a window, so the peak recovers the
// true monotonic envelope and unflagged misparse dips cannot inflate a
// session. The per-session lifts telescope to the week's total peak, so a
// week's bar never exceeds the cap.
//
// A week opened by a detected reset starts its peak at 0, the meter's true
// starting value, so whatever was spent between the reset and the window's
// first poll is counted instead of silently discarded. The leading partial
// week (collection joined mid-flight, no reset ever observed) has no such
// floor to reach for and baselines at its first reading instead, mirroring
// attribute.Build's window reconstruction.
//
// Pure function of the observation slice (ascending by ts) so it is unit
// tested directly, like burnSeries.
func weeklyCapacity(obs []capObs) routes.CapacityResponse {
	out := routes.CapacityResponse{OK: true, Weeks: []routes.WeekCapacity{}}
	if len(obs) == 0 {
		return out
	}

	var (
		weeks         []routes.WeekCapacity
		sessionTotals []float64 // one per complete work session, for the median

		curWeek       *routes.WeekCapacity
		weekReset     time.Time
		haveWeekReset bool

		sessReset     time.Time
		haveSessReset bool
		sessIdx       int // 0 is the leading partial session; excluded from median
		sessTotal     float64

		// Running weekly peak. Reset to unknown at each new week; the first
		// reading then seeds it at 0 for a reset-opened week, or at that
		// reading's own value for the leading partial week (see below).
		weekHavePeak bool
		weekPeak     int

		// Current slice: a run with constant (week, session). Its cost is how
		// much it lifted the weekly peak: weekPeak at flush minus the peak when
		// the slice opened.
		sliceOpen              bool
		sliceStartMS, sliceEnd int64
		sliceHaveEntry         bool
		sliceEntry             int
	)

	newWeek := func(startMS int64, partial bool) *routes.WeekCapacity {
		return &routes.WeekCapacity{StartUnixMS: startMS, Sessions: []routes.SessionSlice{}, Partial: partial}
	}

	flushSlice := func() {
		if sliceOpen && sliceHaveEntry && weekHavePeak {
			cost := float64(weekPeak - sliceEntry)
			if cost < 0 {
				cost = 0
			}
			if cost > 0 {
				curWeek.Sessions = append(curWeek.Sessions, routes.SessionSlice{
					StartUnixMS: sliceStartMS,
					EndUnixMS:   sliceEnd,
					WeekPct:     round2(cost),
				})
				curWeek.TotalWeekPct += cost
				sessTotal += cost
			}
		}
		sliceOpen = false
		sliceHaveEntry = false
	}

	openSlice := func(ms int64) {
		sliceOpen = true
		sliceStartMS, sliceEnd = ms, ms
		sliceHaveEntry = weekHavePeak
		if weekHavePeak {
			sliceEntry = weekPeak
		}
	}

	for i := range obs {
		o := obs[i]

		wt, wok := attribute.ParseReset(o.weekResetTS, o.tsMS, attribute.WeekAhead)
		st, sok := attribute.ParseReset(o.sessResetTS, o.tsMS, attribute.SessAhead)

		if curWeek == nil {
			// First observation: the oldest week is partial (we joined it
			// mid-window) and so is the leading session.
			curWeek = newWeek(o.tsMS, true)
			weekReset, haveWeekReset = wt, wok
			sessReset, haveSessReset = st, sok
			openSlice(o.tsMS)
		} else {
			weekChanged := false
			if wok {
				if !haveWeekReset {
					weekReset, haveWeekReset = wt, true // adopt after unknown start
				} else if wt.After(weekReset.Add(attribute.WeekMinGap)) {
					weekChanged = true
				}
			}
			sessChanged := false
			if sok {
				if !haveSessReset {
					sessReset, haveSessReset = st, true
				} else if st.After(sessReset.Add(attribute.SessMinGap)) {
					sessChanged = true
				}
			}

			if weekChanged || sessChanged {
				flushSlice()
			}
			if weekChanged {
				curWeek.ResetUnixMS = o.tsMS
				curWeek.TotalWeekPct = round2(curWeek.TotalWeekPct)
				weeks = append(weeks, *curWeek)
				curWeek = newWeek(o.tsMS, false)
				weekReset = wt
				weekHavePeak = false // new week starts fresh at its reset
			}
			if sessChanged {
				// The just-ended session feeds the median unless it was the
				// leading partial (idx 0).
				if sessIdx >= 1 {
					sessionTotals = append(sessionTotals, sessTotal)
				}
				sessIdx++
				sessTotal = 0
				sessReset = st
			}
			if weekChanged || sessChanged {
				openSlice(o.tsMS)
			}
		}

		if o.weekPct != nil && o.weekValid && !o.weekSat {
			v := *o.weekPct
			if v > 100 {
				v = 100
			}
			if !weekHavePeak {
				weekHavePeak = true
				// A week opened by a detected reset genuinely started the
				// meter at 0, so it baselines there rather than at this
				// first reading: whatever was spent between the reset and
				// this poll is already baked into v, and baselining at v
				// would silently discard it. The leading partial week is
				// different: collection joined it mid-flight, so there is
				// no earlier reading to reach back to, and v is the best
				// available floor.
				entry := v
				if !curWeek.Partial {
					entry = 0
				}
				weekPeak = entry
				if v > weekPeak {
					weekPeak = v
				}
				// Baseline the slice that opened before any reading (the first
				// slice of the week, or of a partial joined window) the same
				// way: 0 for a real reset, v for the leading partial.
				if !sliceHaveEntry {
					sliceHaveEntry = true
					sliceEntry = entry
				}
			} else if v > weekPeak {
				weekPeak = v
			}
		}
		if o.weekSat {
			curWeek.HitCap = true
		}
		sliceEnd = o.tsMS
	}

	// Close the trailing slice and week. The final session is still open, so
	// it is intentionally not added to sessionTotals.
	flushSlice()
	curWeek.TotalWeekPct = round2(curWeek.TotalWeekPct)
	curWeek.InProgress = true
	weeks = append(weeks, *curWeek)
	out.Weeks = weeks

	// Summary over complete work sessions only.
	var working []float64
	for _, t := range sessionTotals {
		if t >= capWorkSessionPct {
			working = append(working, t)
		}
	}
	out.SessionCount = len(working)
	if len(working) >= capMinSessions {
		sort.Float64s(working)
		med := percentile(working, 0.5)
		out.TypicalSessionWeekPct = round2(med)
		out.SessionWeekPctP25 = round2(percentile(working, 0.25))
		out.SessionWeekPctP75 = round2(percentile(working, 0.75))
		if med > 0 {
			out.SessionsPerWeek = round2(100 / med)
			out.DaysPerSession = round2(7 * med / 100)
		}
	}
	return out
}

// percentile does linear interpolation on an already-sorted slice. p in [0,1].
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := p * float64(n-1)
	lo := int(idx)
	if lo >= n-1 {
		return sorted[n-1]
	}
	frac := idx - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}

// observationsSince loads the slimmed /usage observation series since
// cutoffMS, ascending. Used by the History handler's charts, heatmaps, and
// burn rate.
func (s *Server) observationsSince(ctx context.Context, acct, cutoffMS int64) ([]routes.ObservationPoint, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, session_pct, week_pct,
		       session_saturated, week_saturated,
		       session_reset_detected, week_reset_detected,
		       session_pct_valid, week_pct_valid
		FROM usage_observations
		WHERE account_id = ? AND ts_unix_ms >= ?
		ORDER BY ts_unix_ms ASC
	`, acct, cutoffMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []routes.ObservationPoint
	for rows.Next() {
		var (
			tsMS                       int64
			ssat, wsat, sreset, wreset int
			svalid, wvalid             int
			sNull, wNull               interface{}
		)
		if err := rows.Scan(&tsMS, &sNull, &wNull, &ssat, &wsat, &sreset, &wreset, &svalid, &wvalid); err != nil {
			return nil, err
		}
		p := routes.ObservationPoint{
			TSUnixMS:         tsMS,
			SessionSaturated: ssat == 1,
			WeekSaturated:    wsat == 1,
			SessionReset:     sreset == 1,
			WeekReset:        wreset == 1,
			SessionValid:     svalid == 1,
			WeekValid:        wvalid == 1,
		}
		if v, ok := nullableInt(sNull); ok {
			p.SessionPct = &v
		}
		if v, ok := nullableInt(wNull); ok {
			p.WeekPct = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
