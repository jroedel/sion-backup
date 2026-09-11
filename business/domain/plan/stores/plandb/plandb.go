// Package plandb stores the backup plan in SQLite.
package plandb

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

//go:embed schema.sql
var schemaSQL string

// Keys in plan_meta.
const (
	// seededKey marks config.toml as consumed.
	seededKey = "config_seeded"

	// measurementKey holds the repository size figures.
	//
	// A JSON blob in the key/value table rather than its own schema: there is
	// exactly one, it is read and written whole, and nothing ever queries
	// inside it. A table would be four columns and a migration for no gain.
	measurementKey = "repository_measurement"

	// integrityKey holds the last repository check. Same reasoning as the
	// measurement: one row, read and written whole, nothing queries inside it.
	integrityKey = "repository_integrity"
)

// Migrate brings the schema up to date.
func Migrate(ctx context.Context, db *sqldb.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("plandb: applying the schema: %w", err)
	}

	return addColumns(ctx, db)
}

// addColumns brings a plan table written by an older build up to this one.
//
// schema.sql is CREATE TABLE IF NOT EXISTS and therefore does nothing at all
// to a table that already exists, so every column added after the first
// release has to arrive here. SQLite has no ADD COLUMN IF NOT EXISTS, hence
// the pragma.
func addColumns(ctx context.Context, db *sqldb.DB) error {
	have, err := columns(ctx, db, "plan")
	if err != nil {
		return err
	}

	for _, c := range []struct {
		name string
		ddl  string
	}{
		{"style", `ALTER TABLE plan ADD COLUMN style TEXT NOT NULL DEFAULT ''`},
		{"confirmed_at", `ALTER TABLE plan ADD COLUMN confirmed_at TEXT NOT NULL DEFAULT ''`},
		{"skip_larger_than_gb", `ALTER TABLE plan ADD COLUMN skip_larger_than_gb INTEGER NOT NULL DEFAULT 0`},
		{"skip_on_metered", `ALTER TABLE plan ADD COLUMN skip_on_metered INTEGER NOT NULL DEFAULT 0`},
	} {
		if have[c.name] {
			continue
		}

		if _, err := db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("plandb: adding the %s column: %w", c.name, err)
		}

		// A plan that was already here was already running. The scheduler now
		// refuses to start a run against an unconfirmed plan, so leaving this
		// at the column default would stop every machine in the fleet backing
		// up at the moment it took this build — which is the single worst
		// thing an upgrade of this program could do.
		//
		// Confirmed as of when it was last edited, not as of now, because that
		// is the honest answer: somebody chose this plan, on that day.
		if c.name == "confirmed_at" {
			if _, err := db.ExecContext(ctx,
				`UPDATE plan SET confirmed_at = updated_at WHERE id = 1`); err != nil {
				return fmt.Errorf("plandb: confirming the plan this machine already had: %w", err)
			}
		}
	}

	return nil
}

// columns reports which columns a table has.
func columns(ctx context.Context, db *sqldb.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("plandb: reading the %s columns: %w", table, err)
	}
	defer rows.Close()

	out := map[string]bool{}

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("plandb: reading the %s columns: %w", table, err)
		}

		out[name] = true
	}

	return out, rows.Err()
}

// Store is the SQLite implementation of planbus.Storer.
type Store struct {
	db *sqldb.DB
}

// NewStore constructs one over an already-migrated database.
func NewStore(db *sqldb.DB) *Store {
	return &Store{db: db}
}

// Get returns the plan, or planbus.ErrNoPlan on a machine not set up yet.
func (s *Store) Get(ctx context.Context) (planbus.Plan, error) {
	const q = `
		SELECT node_id, repository, targets, excludes, schedule_times,
		       jitter_minutes, min_interval_secs,
		       pack_size_mib, read_concurrency,
		       use_fs_snapshot, allow_vss_fallback, one_file_system, paused,
		       skip_on_metered, skip_larger_than_gb, style,
		       confirmed_at, updated_at
		FROM plan WHERE id = 1`

	var (
		p                        planbus.Plan
		targets, excludes, times string
		minInterval              int64
		confirmed, updated       string
	)

	err := s.db.QueryRowContext(ctx, q).Scan(
		&p.NodeID, &p.Repository, &targets, &excludes, &times,
		&p.Schedule.JitterMinutes, &minInterval,
		&p.PackSizeMiB, &p.ReadConcurrency,
		&p.UseFSSnapshot, &p.AllowVSSFallback, &p.OneFileSystem, &p.Paused,
		&p.SkipOnMetered, &p.SkipLargerThanGB, &p.Style,
		&confirmed, &updated,
	)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return planbus.Plan{}, planbus.ErrNoPlan
	case err != nil:
		return planbus.Plan{}, fmt.Errorf("plandb: reading the plan: %w", err)
	}

	// A slice and not a map keyed by the JSON: two of these are very often the
	// identical string "[]", and a map would silently decode one of them.
	for _, list := range []struct {
		name string
		raw  string
		dst  *[]string
	}{
		{"targets", targets, &p.Targets},
		{"excludes", excludes, &p.Excludes},
		{"schedule times", times, &p.Schedule.Times},
	} {
		if err := json.Unmarshal([]byte(list.raw), list.dst); err != nil {
			return planbus.Plan{}, fmt.Errorf("plandb: reading the stored %s: %w", list.name, err)
		}
	}

	p.Schedule.MinInterval = time.Duration(minInterval) * time.Second

	// RFC3339 rather than SQLite's own datetime, so the zone survives. A plan
	// edited on a laptop that then crosses a time zone should not appear to
	// have been edited in the future.
	p.UpdatedAt, err = time.Parse(time.RFC3339, updated)
	if err != nil {
		return planbus.Plan{}, fmt.Errorf("plandb: reading updated_at: %w", err)
	}

	// Empty is the whole point rather than a missing value: it is what the
	// column defaults to, and it means nobody at this machine has said yes to
	// the plan yet. See planbus.Plan.ConfirmedAt.
	if confirmed != "" {
		p.ConfirmedAt, err = time.Parse(time.RFC3339, confirmed)
		if err != nil {
			return planbus.Plan{}, fmt.Errorf("plandb: reading confirmed_at: %w", err)
		}
	}

	return p, nil
}

// Put writes the plan, replacing whatever was there.
func (s *Store) Put(ctx context.Context, p planbus.Plan) error {
	targets, err := json.Marshal(nonNil(p.Targets))
	if err != nil {
		return fmt.Errorf("plandb: encoding the targets: %w", err)
	}

	excludes, err := json.Marshal(nonNil(p.Excludes))
	if err != nil {
		return fmt.Errorf("plandb: encoding the excludes: %w", err)
	}

	times, err := json.Marshal(nonNil(p.Schedule.Times))
	if err != nil {
		return fmt.Errorf("plandb: encoding the schedule: %w", err)
	}

	const q = `
		INSERT INTO plan (
			id, node_id, repository, targets, excludes, schedule_times,
			jitter_minutes, min_interval_secs,
			pack_size_mib, read_concurrency,
			use_fs_snapshot, allow_vss_fallback, one_file_system, paused,
			skip_on_metered, skip_larger_than_gb, style,
			confirmed_at, updated_at
		) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			node_id            = excluded.node_id,
			repository         = excluded.repository,
			targets            = excluded.targets,
			excludes           = excluded.excludes,
			schedule_times     = excluded.schedule_times,
			jitter_minutes     = excluded.jitter_minutes,
			min_interval_secs  = excluded.min_interval_secs,
			pack_size_mib      = excluded.pack_size_mib,
			read_concurrency   = excluded.read_concurrency,
			use_fs_snapshot    = excluded.use_fs_snapshot,
			allow_vss_fallback = excluded.allow_vss_fallback,
			one_file_system    = excluded.one_file_system,
			paused             = excluded.paused,
			skip_on_metered    = excluded.skip_on_metered,
			skip_larger_than_gb = excluded.skip_larger_than_gb,
			style              = excluded.style,
			confirmed_at       = excluded.confirmed_at,
			updated_at         = excluded.updated_at`

	if _, err := s.db.ExecContext(ctx, q,
		p.NodeID, p.Repository, string(targets), string(excludes), string(times),
		p.Schedule.JitterMinutes, int64(p.Schedule.MinInterval/time.Second),
		p.PackSizeMiB, p.ReadConcurrency,
		p.UseFSSnapshot, p.AllowVSSFallback, p.OneFileSystem, p.Paused,
		p.SkipOnMetered, p.SkipLargerThanGB, p.Style,
		rfc3339OrEmpty(p.ConfirmedAt), p.UpdatedAt.Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("plandb: writing the plan: %w", err)
	}

	return nil
}

// Seeded reports whether config.toml has already been consumed.
func (s *Store) Seeded(ctx context.Context) (bool, error) {
	var v string

	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM plan_meta WHERE key = ?`, seededKey).Scan(&v)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("plandb: reading the seed marker: %w", err)
	}

	return v == "1", nil
}

// MarkSeeded records that config.toml has been consumed.
func (s *Store) MarkSeeded(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO plan_meta (key, value) VALUES (?, '1')
		 ON CONFLICT (key) DO UPDATE SET value = '1'`, seededKey); err != nil {
		return fmt.Errorf("plandb: writing the seed marker: %w", err)
	}

	return nil
}

// GetMeasurement returns the stored figures, or a zero Measurement.
func (s *Store) GetMeasurement(ctx context.Context) (planbus.Measurement, error) {
	var raw string

	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM plan_meta WHERE key = ?`, measurementKey).Scan(&raw)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return planbus.Measurement{}, nil
	case err != nil:
		return planbus.Measurement{}, fmt.Errorf("plandb: reading the measurement: %w", err)
	}

	var m planbus.Measurement
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		// Not fatal. A measurement is a convenience, and a machine whose
		// figures cannot be decoded should still back up tonight.
		return planbus.Measurement{}, nil
	}

	return m, nil
}

// PutMeasurement replaces the stored figures.
func (s *Store) PutMeasurement(ctx context.Context, m planbus.Measurement) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("plandb: encoding the measurement: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO plan_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		measurementKey, string(raw)); err != nil {
		return fmt.Errorf("plandb: writing the measurement: %w", err)
	}

	return nil
}

// GetIntegrity returns the last repository check, or a zero Integrity.
func (s *Store) GetIntegrity(ctx context.Context) (planbus.Integrity, error) {
	var raw string

	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM plan_meta WHERE key = ?`, integrityKey).Scan(&raw)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return planbus.Integrity{}, nil
	case err != nil:
		return planbus.Integrity{}, fmt.Errorf("plandb: reading the integrity check: %w", err)
	}

	var i planbus.Integrity

	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		// A machine whose stored result cannot be decoded has, as far as
		// anybody can tell, never checked. Which is the safe reading: it makes
		// the next check due rather than skipping one forever.
		return planbus.Integrity{}, nil
	}

	return i, nil
}

// PutIntegrity replaces the stored result.
func (s *Store) PutIntegrity(ctx context.Context, i planbus.Integrity) error {
	raw, err := json.Marshal(i)
	if err != nil {
		return fmt.Errorf("plandb: encoding the integrity check: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO plan_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		integrityKey, string(raw)); err != nil {
		return fmt.Errorf("plandb: writing the integrity check: %w", err)
	}

	return nil
}

// nonNil turns a nil slice into an empty one, so the stored JSON is "[]"
// rather than "null" — which decodes back as nil and reads as "not set" rather
// than "set to nothing".
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}

// interface check, so a signature drift is a compile error here rather than a
// wiring error in main.
var _ planbus.Storer = (*Store)(nil)

// rfc3339OrEmpty renders a time, or the empty string for the zero one.
//
// The zero time has a perfectly good RFC 3339 rendering, and storing it would
// be the bug: "0001-01-01T00:00:00Z" parses back to a real instant in the
// distant past, and every "has this been confirmed" test in the program would
// answer yes.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.Format(time.RFC3339)
}
