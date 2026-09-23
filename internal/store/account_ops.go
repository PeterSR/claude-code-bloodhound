package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
)

// PlaceholderAccountID is the row 0016_accounts.sql created to own every row
// collected before accounts existed. The default config dir claims it on
// first observation.
const PlaceholderAccountID int64 = 1

// AccountIdentity is what Claude Code's state file says about a login.
type AccountIdentity struct {
	AccountUUID   string
	OrgUUID       string
	Email         string
	OrgName       string
	RateLimitTier string
}

// Account mirrors the `accounts` table.
type Account struct {
	ID            int64  `json:"id"`
	AccountUUID   string `json:"account_uuid"`
	OrgUUID       string `json:"org_uuid"`
	Email         string `json:"email"`
	OrgName       string `json:"org_name"`
	RateLimitTier string `json:"rate_limit_tier"`
	Label         string `json:"label"`
	FirstSeenMS   int64  `json:"first_seen_ms"`
	LastSeenMS    int64  `json:"last_seen_ms"`
}

// DisplayName is the label if set, else the email, else the org, else a
// numbered fallback. Never empty.
func (a Account) DisplayName() string {
	switch {
	case a.Label != "":
		return a.Label
	case a.Email != "" && a.OrgName != "":
		return a.Email + " (" + a.OrgName + ")"
	case a.Email != "":
		return a.Email
	case a.OrgName != "":
		return a.OrgName
	}
	return "account " + strconv.FormatInt(a.ID, 10)
}

// Login is one row of a dir's login history: from FromMS on, until the next
// row, the dir was logged in to AccountID.
type Login struct {
	ConfigDir string `json:"config_dir"`
	FromMS    int64  `json:"from_ms"`
	AccountID int64  `json:"account_id"`
}

// ObserveLogin records that dir is currently logged in to ident, and returns
// that account's id.
//
// The account row is created on first sight. The dir's login history gains a
// row only when the account differs from the dir's newest one: at 0 the first
// time the dir is seen (so its existing transcripts go to this account), at
// nowMS on a switch.
//
// isDefaultDir lets the default dir claim the placeholder row that owns every
// row collected before 0016. If this identity already has a row of its own
// (another dir saw it first), the placeholder's rows merge into it instead.
func (s *Store) ObserveLogin(ctx context.Context, dir string, isDefaultDir bool, ident AccountIdentity, nowMS int64) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE account_uuid = ? AND org_uuid = ?`,
		ident.AccountUUID, ident.OrgUUID,
	).Scan(&id)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	placeholderOpen := false
	if isDefaultDir {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM accounts WHERE id = ? AND account_uuid = ''`,
			PlaceholderAccountID,
		).Scan(&n); err != nil {
			return 0, err
		}
		placeholderOpen = n > 0
	}

	switch {
	case placeholderOpen && !found:
		id = PlaceholderAccountID
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET account_uuid = ?, org_uuid = ? WHERE id = ?`,
			ident.AccountUUID, ident.OrgUUID, id,
		); err != nil {
			return 0, err
		}
	case placeholderOpen && found:
		if err := mergeAccountTx(ctx, tx, PlaceholderAccountID, id); err != nil {
			return 0, err
		}
	case !found:
		r, err := tx.ExecContext(ctx,
			`INSERT INTO accounts (account_uuid, org_uuid, first_seen_ms) VALUES (?, ?, ?)`,
			ident.AccountUUID, ident.OrgUUID, nowMS,
		)
		if err != nil {
			return 0, err
		}
		id, _ = r.LastInsertId()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE accounts SET
			email           = ?,
			org_name        = ?,
			rate_limit_tier = ?,
			first_seen_ms   = CASE WHEN first_seen_ms = 0 THEN ? ELSE first_seen_ms END,
			last_seen_ms    = ?
		WHERE id = ?`,
		ident.Email, ident.OrgName, ident.RateLimitTier, nowMS, nowMS, id,
	); err != nil {
		return 0, err
	}

	var cur int64
	err = tx.QueryRowContext(ctx,
		`SELECT account_id FROM account_logins WHERE config_dir = ? ORDER BY from_ms DESC LIMIT 1`,
		dir,
	).Scan(&cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account_logins (config_dir, from_ms, account_id) VALUES (?, 0, ?)`,
			dir, id,
		); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	case cur != id:
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO account_logins (config_dir, from_ms, account_id) VALUES (?, ?, ?)`,
			dir, nowMS, id,
		); err != nil {
			return 0, err
		}
	}

	return id, tx.Commit()
}

// accountScopedTables are the tables whose rows carry an account_id.
var accountScopedTables = []string{
	"turns", "sessions", "compactions", "user_prompts",
	"quota_signals", "usage_observations",
}

// derivedAccountTables are rebuilt per account by the aggregator; a merged
// account's rows there are dropped rather than moved, and the next aggregate
// rebuilds the survivor's.
var derivedAccountTables = []string{
	"session_attribution", "limit_windows", "calibration_points", "buckets",
}

// mergeAccountTx moves everything owned by from onto to and deletes from.
func mergeAccountTx(ctx context.Context, tx *sql.Tx, from, to int64) error {
	for _, t := range derivedAccountTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE account_id = ?`, from); err != nil {
			return err
		}
	}
	for _, t := range append(accountScopedTables, "account_logins") {
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+t+` SET account_id = ? WHERE account_id = ?`, to, from,
		); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, from)
	return err
}

// LoginsFor returns dir's login history, oldest first.
func (s *Store) LoginsFor(ctx context.Context, dir string) ([]Login, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT config_dir, from_ms, account_id FROM account_logins
		  WHERE config_dir = ? ORDER BY from_ms ASC`, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Login
	for rows.Next() {
		var l Login
		if err := rows.Scan(&l.ConfigDir, &l.FromMS, &l.AccountID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CurrentLogins returns the newest login of every dir ever observed.
func (s *Store) CurrentLogins(ctx context.Context) ([]Login, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT l.config_dir, l.from_ms, l.account_id FROM account_logins l
		 WHERE l.from_ms = (SELECT MAX(from_ms) FROM account_logins WHERE config_dir = l.config_dir)
		 ORDER BY l.config_dir`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Login
	for rows.Next() {
		var l Login
		if err := rows.Scan(&l.ConfigDir, &l.FromMS, &l.AccountID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ResolveAccount picks the account a dir was logged in to at tsMS, given the
// dir's history from LoginsFor. Before the first row (which starts at 0, so
// only for negative timestamps) and for an empty history it falls back to the
// placeholder.
func ResolveAccount(logins []Login, tsMS int64) int64 {
	i := sort.Search(len(logins), func(i int) bool { return logins[i].FromMS > tsMS })
	if i == 0 {
		if len(logins) > 0 {
			return logins[0].AccountID
		}
		return PlaceholderAccountID
	}
	return logins[i-1].AccountID
}

// AccountIDs returns every account id, ascending.
func (s *Store) AccountIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MeteredAccountIDs returns the accounts that have a /usage meter, meaning at
// least one reading on record, with the primary first.
func (s *Store) MeteredAccountIDs(ctx context.Context) ([]int64, error) {
	primary, err := s.PrimaryAccountID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT DISTINCT account_id FROM usage_observations ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{primary}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != primary {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// ListAccounts returns every account, oldest first.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, account_uuid, org_uuid, email, org_name, rate_limit_tier,
		       label, first_seen_ms, last_seen_ms
		  FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.AccountUUID, &a.OrgUUID, &a.Email, &a.OrgName,
			&a.RateLimitTier, &a.Label, &a.FirstSeenMS, &a.LastSeenMS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAccountLabel sets the user-chosen display name. "" clears it.
func (s *Store) SetAccountLabel(ctx context.Context, id int64, label string) error {
	r, err := s.DB.ExecContext(ctx, `UPDATE accounts SET label = ? WHERE id = ?`, label, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

const primaryAccountKey = "primary_account_id"

// SetPrimaryAccount records the account that readers with no session or
// directory to go by should show: whoever the first watched config dir is
// logged in to. Written by ingest each run, so a /login there moves it.
func (s *Store) SetPrimaryAccount(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		primaryAccountKey, strconv.FormatInt(id, 10))
	return err
}

// PrimaryAccountID is the account a meter-wide view shows by default. Falls
// back to the placeholder before any ingest has recorded one, which on an
// install that predates accounts is the same account anyway.
func (s *Store) PrimaryAccountID(ctx context.Context) (int64, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, primaryAccountKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaceholderAccountID, nil
	}
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return PlaceholderAccountID, nil
	}
	return id, nil
}

// AccountForSession is the account of the session's newest turn, so a view
// about one conversation reads that conversation's meter. A session with no
// ingested turns yet (it just started) falls back to the primary account.
func (s *Store) AccountForSession(ctx context.Context, sessionUUID string) (int64, error) {
	return s.accountForNewestTurn(ctx, `session_uuid = ?`, sessionUUID)
}

// AccountForCwd is the account of the newest turn in a working directory,
// for directory-scoped views such as budgets. Falls back to the primary.
func (s *Store) AccountForCwd(ctx context.Context, cwd string) (int64, error) {
	return s.accountForNewestTurn(ctx, `cwd = ?`, cwd)
}

func (s *Store) accountForNewestTurn(ctx context.Context, where string, arg any) (int64, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT account_id FROM turns WHERE `+where+` ORDER BY ts_unix_ms DESC LIMIT 1`, arg,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.PrimaryAccountID(ctx)
	}
	return id, err
}
