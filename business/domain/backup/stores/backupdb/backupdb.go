// Package backupdb stores the run history in SQLite.
package backupdb

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

//go:embed schema.sql
var schemaSQL string

// Migrate brings the schema up to date.
//
// schema.sql creates what is missing; the additions below carry a database
// that already has a run table forward, because CREATE TABLE IF NOT EXISTS
// does nothing to one that exists. Each is a column with a default, applied
// only when absent, which is all this schema has needed so far — the day one
// of them needs backfilling or a table rewrite is the day this earns a
// numbered migration table instead.
func Migrate(ctx context.Context, db *sqldb.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("backupdb: applying the schema: %w", err)
	}

	added := []struct{ column, ddl string }{
		{"run_uuid", `ALTER TABLE run ADD COLUMN run_uuid TEXT NOT NULL DEFAULT ''`},
		{"seeding", `ALTER TABLE run ADD COLUMN seeding INTEGER NOT NULL DEFAULT 0`},
	}

	for _, a := range added {
		has, err := hasColumn(ctx, db, "run", a.column)
		if err != nil {
			return err
		}

		if has {
			continue
		}

		if _, err := db.ExecContext(ctx, a.ddl); err != nil {
			return fmt.Errorf("backupdb: adding run.%s: %w", a.column, err)
		}
	}

	return nil
}

func hasColumn(ctx context.Context, db *sqldb.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM pragma_table_info(?) WHERE name = ?`,
		table, column)
	if err != nil {
		return false, fmt.Errorf("backupdb: looking for %s.%s: %w", table, column, err)
	}
	defer rows.Close()

	found := rows.Next()

	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("backupdb: looking for %s.%s: %w", table, column, err)
	}

	return found, nil
}

// Store is the SQLite implementation of backupbus.Storer.
type Store struct {
	db *sqldb.DB
}

// NewStore constructs one over an already-migrated database.
func NewStore(db *sqldb.DB) *Store {
	return &Store{db: db}
}

// Create opens a run row and returns its ID.
//
// The row is written before the backup starts, not after it finishes, and that
// is deliberate: a machine that is switched off mid-backup leaves a run with
// no finished_at, which the status page shows as "interrupted". Writing only
// on completion would leave no trace of it at all, and "nothing happened last
// night" and "it was killed halfway through" are very different problems.
func (s *Store) Create(ctx context.Context, r backupbus.Run) (int64, error) {
	const q = `
		INSERT INTO run (node_id, repository, run_uuid, seeding, started_at, outcome, message)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	res, err := s.db.ExecContext(ctx, q,
		r.NodeID, r.Repository, r.RunUUID, r.Seeding,
		r.StartedAt.Format(time.RFC3339), string(r.Outcome), r.Message)
	if err != nil {
		return 0, fmt.Errorf("backupdb: opening a run row: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("backupdb: reading the new run's ID: %w", err)
	}

	return id, nil
}

// Finish records the outcome.
func (s *Store) Finish(ctx context.Context, r backupbus.Run) error {
	files, err := json.Marshal(nonNil(r.UnreadableFiles))
	if err != nil {
		return fmt.Errorf("backupdb: encoding the unreadable files: %w", err)
	}

	const q = `
		UPDATE run SET
			finished_at = ?, outcome = ?, message = ?, snapshot_id = ?,
			files_new = ?, files_changed = ?, total_files_processed = ?,
			total_bytes_processed = ?, data_added = ?, unreadable_files = ?,
			verified = ?, vss_fell_back = ?
		WHERE id = ?`

	res, err := s.db.ExecContext(ctx, q,
		r.FinishedAt.Format(time.RFC3339), string(r.Outcome), r.Message, r.SnapshotID,
		r.FilesNew, r.FilesChanged, r.TotalFilesProcessed,
		r.TotalBytesProcessed, r.DataAdded, string(files),
		r.Verified, r.VSSFellBack,
		r.ID,
	)
	if err != nil {
		return fmt.Errorf("backupdb: closing run %d: %w", r.ID, err)
	}

	// Checked, because an UPDATE matching nothing is not an error to SQLite
	// and would silently discard the outcome of a completed backup.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("backupdb: run %d is not in the history", r.ID)
	}

	return nil
}

const columns = `
	id, node_id, repository, run_uuid, seeding, started_at, finished_at, outcome,
	message, snapshot_id, files_new, files_changed, total_files_processed,
	total_bytes_processed, data_added, unreadable_files, verified,
	vss_fell_back, reported_at`

// Recent returns the newest runs first.
func (s *Store) Recent(ctx context.Context, limit int) ([]backupbus.Run, error) {
	if limit <= 0 {
		limit = 30
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+columns+` FROM run ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("backupdb: reading the run history: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

// Last returns the newest run, or backupbus.ErrNoRuns.
func (s *Store) Last(ctx context.Context) (backupbus.Run, error) {
	runs, err := s.Recent(ctx, 1)
	if err != nil {
		return backupbus.Run{}, err
	}

	if len(runs) == 0 {
		return backupbus.Run{}, backupbus.ErrNoRuns
	}

	return runs[0], nil
}

// Unreported returns finished runs the fleet dashboard has not been told
// about, oldest first so the dashboard receives them in the order they
// happened.
//
// Only finished runs: a run still in progress has no outcome to report, and
// reporting one would put "failed" on the dashboard for a backup that is
// running perfectly well.
func (s *Store) Unreported(ctx context.Context, limit int) ([]backupbus.Run, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+columns+` FROM run
		 WHERE reported_at IS NULL AND finished_at IS NOT NULL
		 ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("backupdb: reading unreported runs: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

// SeenRepository reports whether a snapshot has ever been written to this
// repository from this machine.
//
// It is how a run learns whether it is the seeding one. Asked of the local
// history rather than of the repository itself because the answer is needed
// before the run starts, and asking restic would mean a round trip to the
// bucket to answer a question that is only ever used for reporting.
//
// A snapshot ID rather than a successful outcome, deliberately: a run that
// wrote a snapshot and failed verification still filled the repository, and
// the next one is not seeding it.
func (s *Store) SeenRepository(ctx context.Context, repository string) (bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT 1 FROM run WHERE repository = ? AND snapshot_id <> '' LIMIT 1`, repository)
	if err != nil {
		return false, fmt.Errorf("backupdb: looking for earlier runs against %s: %w", repository, err)
	}
	defer rows.Close()

	seen := rows.Next()

	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("backupdb: looking for earlier runs against %s: %w", repository, err)
	}

	return seen, nil
}

// MarkReported records that the fleet dashboard has been told.
func (s *Store) MarkReported(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run SET reported_at = ? WHERE id = ?`, at.Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("backupdb: marking run %d reported: %w", id, err)
	}

	return nil
}

// Prune keeps the history to a bounded number of rows.
//
// The history is for a person looking at a status page, not an audit log. A
// machine running twice a day for five years is 3,650 rows, which is not large
// — but the database sits in a user profile that gets copied around, and there
// is no reason for it to grow forever.
func (s *Store) Prune(ctx context.Context, keep int) error {
	if keep <= 0 {
		return errors.New("backupdb: refusing to prune the entire history")
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM run WHERE id NOT IN (SELECT id FROM run ORDER BY id DESC LIMIT ?)`,
		keep); err != nil {
		return fmt.Errorf("backupdb: pruning the history: %w", err)
	}

	return nil
}

func scanRuns(rows *sql.Rows) ([]backupbus.Run, error) {
	var out []backupbus.Run

	for rows.Next() {
		var (
			r          backupbus.Run
			started    string
			finished   sql.NullString
			reported   sql.NullString
			unreadable string
			outcome    string
		)

		if err := rows.Scan(
			&r.ID, &r.NodeID, &r.Repository, &r.RunUUID, &r.Seeding,
			&started, &finished, &outcome, &r.Message,
			&r.SnapshotID, &r.FilesNew, &r.FilesChanged, &r.TotalFilesProcessed,
			&r.TotalBytesProcessed, &r.DataAdded, &unreadable, &r.Verified,
			&r.VSSFellBack, &reported,
		); err != nil {
			return nil, fmt.Errorf("backupdb: reading a run: %w", err)
		}

		r.Outcome = backupbus.Outcome(outcome)

		var err error
		if r.StartedAt, err = time.Parse(time.RFC3339, started); err != nil {
			return nil, fmt.Errorf("backupdb: reading run %d's start time: %w", r.ID, err)
		}

		if finished.Valid {
			if r.FinishedAt, err = time.Parse(time.RFC3339, finished.String); err != nil {
				return nil, fmt.Errorf("backupdb: reading run %d's end time: %w", r.ID, err)
			}
		}

		if reported.Valid {
			t, err := time.Parse(time.RFC3339, reported.String)
			if err != nil {
				return nil, fmt.Errorf("backupdb: reading run %d's report time: %w", r.ID, err)
			}

			r.ReportedAt = &t
		}

		if err := json.Unmarshal([]byte(unreadable), &r.UnreadableFiles); err != nil {
			return nil, fmt.Errorf("backupdb: reading run %d's unreadable files: %w", r.ID, err)
		}

		out = append(out, r)
	}

	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}

var _ backupbus.Storer = (*Store)(nil)
