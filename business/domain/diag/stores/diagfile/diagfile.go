// Package diagfile holds diagnostic reports as files until they are sent.
//
// A directory of JSON files rather than a table in the database, for one
// reason: the report worth having most is from an install that fell over
// before the database existed. A store that needs the thing that failed is not
// a store.
//
// Everything here is deliberately dull. It is written by a program that is
// about to exit, sometimes from a panic handler, so it does no locking beyond
// what the filesystem gives it, allocates almost nothing, and treats every
// failure as "give up quietly" — a diagnostic that interferes with the program
// it is diagnosing is worse than no diagnostic.
package diagfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
)

// Keep is how many reports the directory holds.
//
// Small on purpose. Twenty crashes say the same thing as two hundred, and this
// directory sits in a user profile that gets copied around.
const Keep = 20

// MaxAge is when an unsent report stops being worth sending.
//
// A month, which is long enough to survive a laptop being on holiday and short
// enough that a server which never grows the endpoint does not accumulate a
// permanent pile of files nobody will ever read.
const MaxAge = 30 * 24 * time.Hour

// Store is a directory of reports.
type Store struct {
	dir string
}

// NewStore constructs one over a directory, creating it if it is missing.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("diagfile: creating %s: %w", dir, err)
	}

	return &Store{dir: dir}, nil
}

// Put writes one report.
//
// The name carries the timestamp so that a directory listing is in the order
// things happened, and a UUID so that two reports in the same millisecond are
// two files. Written to a temporary name and renamed, so a crash in the middle
// of writing a crash report leaves nothing half-parsed behind.
func (s *Store) Put(r diagbus.Report) error {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("diagfile: encoding a report: %w", err)
	}

	name := fmt.Sprintf("%s-%s.json",
		r.OccurredAt.UTC().Format("20060102T150405.000"), uuid.NewString()[:8])

	tmp := filepath.Join(s.dir, "."+name)

	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return fmt.Errorf("diagfile: writing a report: %w", err)
	}

	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("diagfile: storing a report: %w", err)
	}

	s.trim()

	return nil
}

// List returns what is waiting, oldest first.
//
// A file that cannot be parsed is deleted rather than returned. It is a
// diagnostic about a diagnostic, and keeping it would block the queue behind
// it forever.
func (s *Store) List() ([]diagbus.Queued, error) {
	names, err := s.names()
	if err != nil {
		return nil, err
	}

	var out []diagbus.Queued

	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("diagfile: reading %s: %w", name, err)
		}

		var r diagbus.Report

		if err := json.Unmarshal(raw, &r); err != nil {
			_ = os.Remove(filepath.Join(s.dir, name))

			continue
		}

		if !r.OccurredAt.IsZero() && time.Since(r.OccurredAt) > MaxAge {
			_ = os.Remove(filepath.Join(s.dir, name))

			continue
		}

		out = append(out, diagbus.Queued{ID: name, Report: r})
	}

	return out, nil
}

// Remove deletes one report by the ID List gave out.
func (s *Store) Remove(id string) error {
	// Refused rather than joined: an ID is a file name from this directory,
	// and anything with a separator in it came from somewhere else.
	if id != filepath.Base(id) {
		return fmt.Errorf("diagfile: %q is not a report in this directory", id)
	}

	if err := os.Remove(filepath.Join(s.dir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("diagfile: removing %s: %w", id, err)
	}

	return nil
}

// names lists report files, oldest first, which is lexical order given how
// Put names them.
func (s *Store) names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("diagfile: reading %s: %w", s.dir, err)
	}

	var names []string

	for _, e := range entries {
		name := e.Name()

		// Skip the half-written ones and anything else that wandered in.
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}

		names = append(names, name)
	}

	sort.Strings(names)

	return names, nil
}

// trim keeps the newest Keep reports.
//
// Newest rather than oldest, which is the opposite of how the queue drains, and
// deliberately: if the machine is producing reports faster than it can send
// them, the ones describing what is wrong now are worth more than the ones
// describing what was wrong first.
func (s *Store) trim() {
	names, err := s.names()
	if err != nil || len(names) <= Keep {
		return
	}

	for _, name := range names[:len(names)-Keep] {
		_ = os.Remove(filepath.Join(s.dir, name))
	}
}

var _ diagbus.Queue = (*Store)(nil)
