package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

// fiveHourMS is the fallback span for a low priority stretch whose refusal
// record carried no reset. Low priority lasts until the window it is
// standing in for reopens, and the five hour window is the only one Claude
// Code offers it against, so its own length is the honest guess when the
// exact moment is missing.
const fiveHourMS = int64(5 * 60 * 60 * 1000)

// QuotaVerdict is the current quota situation as the transcripts describe
// it, which is the half the /usage percentage cannot express.
//
// Zero value means the transcripts have nothing to say — no refusal on
// record inside a window that is still closed. That is the ordinary case
// and readers must render it as "no claim", not as "everything is fine and
// also nothing is billing": a percentage pinned at the cap with no signal
// behind it is a percentage bloodhound cannot interpret, and saying so is
// more useful than picking one of the three readings at random.
type QuotaVerdict struct {
	// Refused is whether a request is on record as having been turned away
	// for quota reasons inside a window that has not reopened yet.
	Refused       bool
	RefusedAtMS   int64
	Bucket        string
	ResetTSUnixMS int64

	// OverageStatus / OverageDisabledReason / UsingOverage are the
	// pay-per-use tier's verdict at the moment of the refusal. UsingOverage
	// is the one that settles whether "on extra usage" is a true sentence.
	OverageStatus         string
	OverageDisabledReason string
	UsingOverage          bool

	// LowPriorityOffered is whether the fallback was on the table.
	LowPriorityOffered bool

	// LowPriorityActive is whether at least one session took it and the
	// window it is standing in for has not reopened. Sessions lists them,
	// oldest acceptance first, so a caller can say which conversations are
	// in this state rather than only that some are.
	LowPriorityActive   bool
	LowPrioritySinceMS  int64
	LowPriorityUntilMS  int64
	LowPrioritySessions []string
}

// LowPriority reports whether sessionUUID is one of the sessions running at
// the lower priority right now. The account-wide flag is what the gauges
// want; this is what a message addressed to one conversation wants, because
// the sentence "you are still able to work" is only true for the session
// that accepted.
func (v QuotaVerdict) LowPriority(sessionUUID string) bool {
	for _, s := range v.LowPrioritySessions {
		if s == sessionUUID {
			return true
		}
	}
	return false
}

// QuotaNow reads the current verdict as of nowMS.
//
// Two questions, two queries, deliberately not one join. The refusal is an
// account fact and wants the single newest row; the priority is a per
// session fact and wants the newest row for each session separately, since
// one conversation switching back to waiting says nothing about the others.
//
// Both are about one account: a refusal on another account's quota says
// nothing about this one's.
func (s *Store) QuotaNow(ctx context.Context, accountID int64, nowMS int64) (QuotaVerdict, error) {
	var v QuotaVerdict
	accountID = accountOr1(accountID)

	// A refusal matters while the window it was about is still closed. Rows
	// that carried no reset fall back to their own age, because an hours-old
	// refusal with nothing to bound it is not evidence about now.
	var offer string
	err := s.DB.QueryRowContext(ctx, `
		SELECT ts_unix_ms, bucket, reset_ts_unix_ms,
		       overage_status, overage_disabled_reason, using_overage,
		       low_priority_offer
		  FROM quota_signals
		 WHERE kind = 'rate_limited'
		   AND account_id = ?
		   AND ts_unix_ms <= ?
		   AND (CASE WHEN reset_ts_unix_ms > 0
		             THEN reset_ts_unix_ms > ?
		             ELSE ts_unix_ms > ? - ? END)
		 ORDER BY ts_unix_ms DESC
		 LIMIT 1
	`, accountID, nowMS, nowMS, nowMS, fiveHourMS).Scan(
		&v.RefusedAtMS, &v.Bucket, &v.ResetTSUnixMS,
		&v.OverageStatus, &v.OverageDisabledReason, &v.UsingOverage,
		&offer,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No live refusal. Low priority can still be running underneath one
		// that has scrolled out of that window, so fall through rather than
		// returning here.
	case err != nil:
		return QuotaVerdict{}, err
	default:
		v.Refused = true
		// Any arm name at all means the fallback was offered; the name
		// itself stays in the column, for the day a second arm needs
		// telling apart from the first.
		v.LowPriorityOffered = offer != ""
	}

	sess, err := s.lowPrioritySessions(ctx, accountID, nowMS)
	if err != nil {
		return QuotaVerdict{}, err
	}
	for _, ls := range sess {
		v.LowPrioritySessions = append(v.LowPrioritySessions, ls.session)
		if v.LowPrioritySinceMS == 0 || ls.sinceMS < v.LowPrioritySinceMS {
			v.LowPrioritySinceMS = ls.sinceMS
		}
		if ls.untilMS > v.LowPriorityUntilMS {
			v.LowPriorityUntilMS = ls.untilMS
		}
	}
	v.LowPriorityActive = len(v.LowPrioritySessions) > 0

	return v, nil
}

type lowPriSession struct {
	session string
	sinceMS int64
	untilMS int64
}

// lowPrioritySessions finds the sessions whose most recent priority signal
// is an acceptance that has not yet expired.
//
// "Not yet expired" is bounded by the reset of the refusal the acceptance
// answered — the newest refusal at or before it — because that is what the
// mode is standing in for and what Claude Code itself says it lasts until.
// Without such a refusal on record the acceptance gets five hours from its
// own timestamp, which is the window's own length.
func (s *Store) lowPrioritySessions(ctx context.Context, accountID, nowMS int64) ([]lowPriSession, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT q.session_uuid, q.kind, q.ts_unix_ms
		  FROM quota_signals q
		  JOIN (
		    SELECT session_uuid, MAX(ts_unix_ms) AS ts
		      FROM quota_signals
		     WHERE kind IN ('low_priority_on', 'low_priority_off')
		       AND ts_unix_ms <= ?
		     GROUP BY session_uuid
		  ) newest
		    ON newest.session_uuid = q.session_uuid
		   AND newest.ts = q.ts_unix_ms
		 WHERE q.kind IN ('low_priority_on', 'low_priority_off')
		   AND q.account_id = ?
	`, nowMS, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []lowPriSession
	for rows.Next() {
		var sess, kind string
		var tsMS int64
		if err := rows.Scan(&sess, &kind, &tsMS); err != nil {
			return nil, err
		}
		if kind != "low_priority_on" {
			continue
		}
		out = append(out, lowPriSession{session: sess, sinceMS: tsMS})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	live := out[:0]
	for _, ls := range out {
		until, err := s.resetForAcceptance(ctx, accountID, ls.sinceMS)
		if err != nil {
			return nil, err
		}
		if until == 0 {
			until = ls.sinceMS + fiveHourMS
		}
		if until <= nowMS {
			continue
		}
		ls.untilMS = until
		live = append(live, ls)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].sinceMS < live[j].sinceMS })
	return live, nil
}

// resetForAcceptance returns the reset of the refusal an acceptance at
// acceptedMS was answering, or 0 when no refusal precedes it. Bounded to
// the five hours before the acceptance so that a stale refusal from an
// earlier window cannot lend its reset to a later one.
func (s *Store) resetForAcceptance(ctx context.Context, accountID, acceptedMS int64) (int64, error) {
	var reset int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT reset_ts_unix_ms
		  FROM quota_signals
		 WHERE kind = 'rate_limited'
		   AND account_id = ?
		   AND reset_ts_unix_ms > 0
		   AND ts_unix_ms <= ?
		   AND ts_unix_ms > ? - ?
		 ORDER BY ts_unix_ms DESC
		 LIMIT 1
	`, accountID, acceptedMS, acceptedMS, fiveHourMS).Scan(&reset)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return reset, nil
}
