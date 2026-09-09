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
)

// Migrate brings the schema up to date.
func Migrate(ctx context.Context, db *sqldb.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("plandb: applying the schema: %w", err)
	}

	return nil
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
		       updated_at
		FROM plan WHERE id = 1`

	var (
		p                        planbus.Plan
		targets, excludes, times string
		minInterval              int64
		updated                  string
	)

	err := s.db.QueryRowContext(ctx, q).Scan(
		&p.NodeID, &p.Repository, &targets, &excludes, &times,
		&p.Schedule.JitterMinutes, &minInterval,
		&p.PackSizeMiB, &p.ReadConcurrency,
		&p.UseFSSnapshot, &p.AllowVSSFallback, &p.OneFileSystem, &p.Paused,
		&updated,
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
			updated_at
		) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			updated_at         = excluded.updated_at`

	if _, err := s.db.ExecContext(ctx, q,
		p.NodeID, p.Repository, string(targets), string(excludes), string(times),
		p.Schedule.JitterMinutes, int64(p.Schedule.MinInterval/time.Second),
		p.PackSizeMiB, p.ReadConcurrency,
		p.UseFSSnapshot, p.AllowVSSFallback, p.OneFileSystem, p.Paused,
		p.UpdatedAt.Format(time.RFC3339),
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
