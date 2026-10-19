package plugin

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// dealIndex is where a profile keeps which steps and sealed reports each of
// its deals holds, with each step's local event: the step source of
// capsule-cli#242 (design note §2.8). Every read and write of that
// bookkeeping goes through it, so that deals kept on evidence books can stand
// beside the deals of this SQLite index. The deal's log itself is read
// through the session's target (stepEntries, logEntries).
type dealIndex interface {
	// stepCount is how many steps the deal has indexed.
	stepCount(ctx context.Context, dealID string) (int, error)
	// steps are the indexed steps the filter selects (see stepFilter).
	steps(ctx context.Context, f stepFilter) ([]indexedStep, error)
	// dealIDs are every deal and sale with an indexed step, in id order.
	dealIDs(ctx context.Context) ([]string, error)
	// prepareStep indexes a step before it reaches the log, at sequence 0
	// whatever st.Sequence holds,
	// publishedStep records its log position, and withdrawStep removes a
	// step that never reached the log.
	prepareStep(ctx context.Context, st indexedStep) error
	publishedStep(ctx context.Context, dealID string, n int64, sequence uint64) error
	withdrawStep(ctx context.Context, dealID string, n int64) error
	// The same three for a sealed report, by its capsule id.
	prepareReport(ctx context.Context, dealID, capsuleID, audience string) error
	publishedReport(ctx context.Context, dealID, capsuleID string, sequence uint64) error
	withdrawReport(ctx context.Context, dealID, capsuleID string) error
	// reportIDs are the capsule ids of every report sealed for the deal.
	reportIDs(ctx context.Context, dealID string) (map[string]bool, error)
	// reportsThrough is how many of the deal's reports are in the first
	// leaves entries of its log.
	reportsThrough(ctx context.Context, dealID string, leaves uint64) (int, error)
	// latestReport is the newest report for audience in the first leaves
	// entries of the deal's log, with its position; ok is false when there
	// is none.
	latestReport(ctx context.Context, dealID, audience string, leaves uint64) (capsuleID string, sequence uint64, ok bool, err error)
	// reportAt is the report for audience at position sequence of the
	// deal's log; ok is false when there is none.
	reportAt(ctx context.Context, dealID, audience string, sequence uint64) (capsuleID string, ok bool, err error)
}

// The SQLite index's two tables, created with the rest of the store
// (dealIndexSchema).
const (
	dealStepsTable = `CREATE TABLE IF NOT EXISTS deal_steps (
	deal_id TEXT NOT NULL,
	n INTEGER NOT NULL,
	kind TEXT NOT NULL,
	capsule_id TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	record_digest TEXT NOT NULL,
	local TEXT NOT NULL,
	PRIMARY KEY (deal_id, n)
)`
	dealReportsTable = `CREATE TABLE IF NOT EXISTS deal_reports (
	deal_id TEXT NOT NULL,
	capsule_id TEXT NOT NULL,
	audience TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	PRIMARY KEY (deal_id, capsule_id)
)`
)

// indexedStep is one step as the index holds it. Sequence is 0 while the
// step is prepared but not yet in the log.
type indexedStep struct {
	DealID    string
	N         int64
	Kind      string
	CapsuleID string
	Sequence  uint64
	Digest    string
	Local     string
}

// stepFilter selects indexed steps. An empty field selects every value.
// Steps come in deal id then step order, or, with Indexed, in the order they
// were indexed across deals: the order their steps were prepared, which the
// rules history keeps among acts of the same second. The SQLite index has
// it as deal_steps' rowid. A store of books on one shelf has it as the order
// their leaves were appended across the file (the CLL entries' rowid), and
// must give the same order.
type stepFilter struct {
	DealID    string
	Kinds     []string
	N         int64
	CapsuleID string
	// ExceptDeal leaves one deal out.
	ExceptDeal string
	Indexed    bool
}

// sqliteDealIndex is the index in the profile's deal.db, as every released
// build has kept it: the deal_steps and deal_reports tables.
type sqliteDealIndex struct{ db *sql.DB }

func (x sqliteDealIndex) stepCount(ctx context.Context, dealID string) (int, error) {
	var n int
	err := x.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_steps WHERE deal_id=?`, dealID).Scan(&n)
	return n, err
}

func (x sqliteDealIndex) steps(ctx context.Context, f stepFilter) (_ []indexedStep, err error) {
	var where []string
	var args []any
	if f.DealID != "" {
		where, args = append(where, "deal_id=?"), append(args, f.DealID)
	}
	if f.ExceptDeal != "" {
		where, args = append(where, "deal_id<>?"), append(args, f.ExceptDeal)
	}
	if f.N != 0 {
		where, args = append(where, "n=?"), append(args, f.N)
	}
	if f.CapsuleID != "" {
		where, args = append(where, "capsule_id=?"), append(args, f.CapsuleID)
	}
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN (?"+strings.Repeat(",?", len(f.Kinds)-1)+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	query := `SELECT deal_id, n, kind, capsule_id, cll_sequence, record_digest, local FROM deal_steps`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Indexed {
		query += " ORDER BY rowid"
	} else {
		query += " ORDER BY deal_id, n"
	}
	rows, err := x.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []indexedStep
	for rows.Next() {
		var st indexedStep
		if err = rows.Scan(&st.DealID, &st.N, &st.Kind, &st.CapsuleID, &st.Sequence, &st.Digest, &st.Local); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (x sqliteDealIndex) dealIDs(ctx context.Context) (_ []string, err error) {
	rows, err := x.db.QueryContext(ctx, `SELECT DISTINCT deal_id FROM deal_steps ORDER BY deal_id`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (x sqliteDealIndex) prepareStep(ctx context.Context, st indexedStep) error {
	_, err := x.db.ExecContext(ctx, `INSERT INTO deal_steps (deal_id, n, kind, capsule_id, cll_sequence, record_digest, local) VALUES (?,?,?,?,0,?,?)`,
		st.DealID, st.N, st.Kind, st.CapsuleID, st.Digest, st.Local)
	return err
}

func (x sqliteDealIndex) publishedStep(ctx context.Context, dealID string, n int64, sequence uint64) error {
	_, err := x.db.ExecContext(ctx, `UPDATE deal_steps SET cll_sequence=? WHERE deal_id=? AND n=?`, sequence, dealID, n)
	return err
}

func (x sqliteDealIndex) withdrawStep(ctx context.Context, dealID string, n int64) error {
	_, err := x.db.ExecContext(ctx, `DELETE FROM deal_steps WHERE deal_id=? AND n=?`, dealID, n)
	return err
}

func (x sqliteDealIndex) prepareReport(ctx context.Context, dealID, capsuleID, audience string) error {
	_, err := x.db.ExecContext(ctx, `INSERT INTO deal_reports (deal_id, capsule_id, audience, cll_sequence) VALUES (?,?,?,0)`, dealID, capsuleID, audience)
	return err
}

func (x sqliteDealIndex) publishedReport(ctx context.Context, dealID, capsuleID string, sequence uint64) error {
	_, err := x.db.ExecContext(ctx, `UPDATE deal_reports SET cll_sequence=? WHERE deal_id=? AND capsule_id=?`, sequence, dealID, capsuleID)
	return err
}

func (x sqliteDealIndex) withdrawReport(ctx context.Context, dealID, capsuleID string) error {
	_, err := x.db.ExecContext(ctx, `DELETE FROM deal_reports WHERE deal_id=? AND capsule_id=?`, dealID, capsuleID)
	return err
}

func (x sqliteDealIndex) reportIDs(ctx context.Context, dealID string) (_ map[string]bool, err error) {
	rows, err := x.db.QueryContext(ctx, `SELECT capsule_id FROM deal_reports WHERE deal_id=?`, dealID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func (x sqliteDealIndex) reportsThrough(ctx context.Context, dealID string, leaves uint64) (int, error) {
	var n int
	err := x.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_reports WHERE deal_id=? AND cll_sequence>0 AND cll_sequence<=?`, dealID, leaves).Scan(&n)
	return n, err
}

func (x sqliteDealIndex) latestReport(ctx context.Context, dealID, audience string, leaves uint64) (string, uint64, bool, error) {
	var id string
	var seq uint64
	err := x.db.QueryRowContext(ctx, `SELECT capsule_id, cll_sequence FROM deal_reports WHERE deal_id=? AND cll_sequence>0 AND cll_sequence<=? AND audience=? ORDER BY cll_sequence DESC LIMIT 1`,
		dealID, leaves, audience).Scan(&id, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return id, seq, err == nil, err
}

func (x sqliteDealIndex) reportAt(ctx context.Context, dealID, audience string, sequence uint64) (string, bool, error) {
	var id string
	err := x.db.QueryRowContext(ctx, `SELECT capsule_id FROM deal_reports WHERE deal_id=? AND audience=? AND cll_sequence=?`, dealID, audience, sequence).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
