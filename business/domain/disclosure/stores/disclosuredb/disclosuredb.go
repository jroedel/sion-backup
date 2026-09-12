// Package disclosuredb keeps, in SQLite on the owner's own machine, the heads
// of the disclosure chain the server has shown it.
//
// One table, append-only in practice, and never pruned. A head arrives about
// once a day — the chain only grows when the password is disclosed — so a
// decade of them is a few thousand rows of forty bytes, and the oldest row is
// the most valuable thing in the table: it is how far back this machine can
// say the trail has not been rewritten.
package disclosuredb

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

//go:embed schema.sql
var schemaSQL string

// Migrate brings the schema up to date.
func Migrate(ctx context.Context, db *sqldb.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("disclosuredb: applying the schema: %w", err)
	}

	return nil
}

// Store is the witness table.
type Store struct {
	db *sqldb.DB
}

// NewStore constructs one over an already-migrated database.
func NewStore(db *sqldb.DB) *Store { return &Store{db: db} }

// Remember writes a head down, leaving an existing row alone.
//
// DO NOTHING rather than an update, and the reason is the whole point of the
// table: re-observing a head must not move seen_at forward, or a machine that
// checks its trail every day would never be able to claim more than a day of
// history.
func (s *Store) Remember(ctx context.Context, w disclosurebus.Witness, seen time.Time) error {
	const q = `
		INSERT INTO disclosure_witness (count, hash, at, seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (count) DO NOTHING`

	at := ""
	if !w.At.IsZero() {
		at = w.At.Format(time.RFC3339)
	}

	_, err := s.db.ExecContext(ctx, q, w.Count, w.Hash, at, seen.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("disclosuredb: remembering head at %d: %w", w.Count, err)
	}

	return nil
}

// Kept returns every head written down, oldest first.
func (s *Store) Kept(ctx context.Context) ([]disclosurebus.Keep, error) {
	const q = `
		SELECT count, hash, at, seen_at
		FROM disclosure_witness
		ORDER BY count ASC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("disclosuredb: reading the kept heads: %w", err)
	}
	defer rows.Close()

	var kept []disclosurebus.Keep

	for rows.Next() {
		var (
			k          disclosurebus.Keep
			at, seenAt string
		)

		if err := rows.Scan(&k.Count, &k.Hash, &at, &seenAt); err != nil {
			return nil, fmt.Errorf("disclosuredb: reading a kept head: %w", err)
		}

		k.At = parse(at)
		k.Seen = parse(seenAt)

		kept = append(kept, k)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("disclosuredb: reading the kept heads: %w", err)
	}

	return kept, nil
}

// parse reads a stored timestamp, treating an unreadable one as absent.
//
// A row written by a future build, or by a hand-edited database, must not stop
// a machine reporting on its own trail: the timestamp is what the page prints
// beside the check, and the check itself rests on the hash.
func parse(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}

	return t
}

var _ disclosurebus.Store = (*Store)(nil)
