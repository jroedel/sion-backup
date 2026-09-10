// Package planbus is what this machine backs up, and when.
//
// One plan per machine. Not a list of plans, deliberately: a fleet of work
// computers each running one backup to one repository is the whole problem,
// and a machine with two competing plans is a machine where "is my backup
// working" has two answers. If a second repository is ever needed, it will be
// a second plan row and every caller will have to say which one it means —
// which is the right cost to pay at that point, and the wrong one to pay now.
//
// # Configuration has two sources and only one of them is authoritative
//
// The database is the plan. config.toml is a SEED: it populates a machine that
// has no plan yet, at install time, and is ignored from then on.
//
// This is the shape it has to be, because both ends are real. The IT
// administrator installs the machine and wants to hand it a file; the person
// using the machine changes their excludes on the status page and expects it
// to stick. Re-reading the file every start would silently undo their change
// at the next reboot, and that is a genuinely maddening bug to be on the
// receiving end of. So the file seeds, once, and [Business.Seeded] reports
// whether it has been consumed.
package planbus

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNoPlan reports a machine that has not been set up yet.
var ErrNoPlan = errors.New("planbus: this machine has no backup plan yet")

// Plan is everything a run needs to know.
type Plan struct {
	// NodeID names this machine in the fleet dashboard. It is chosen by the
	// administrator at install rather than derived from the hostname, because
	// hostnames on a corporate network are frequently neither stable nor
	// legible — "DESKTOP-4KJ2P1" tells nobody whose laptop stopped backing up.
	NodeID string

	// Repository is the restic repository URL.
	Repository string

	// Targets are the directories to back up.
	Targets []string

	// Excludes are restic exclude patterns.
	Excludes []string

	// Schedule says when to run.
	Schedule Schedule

	// PackSizeMiB and ReadConcurrency are the connection- and disk-dependent
	// tuning the legacy scripts set per machine by hand.
	PackSizeMiB     int
	ReadConcurrency int

	// UseFSSnapshot asks for VSS on Windows. It is on by default there and
	// meaningless elsewhere.
	UseFSSnapshot bool

	// AllowVSSFallback backs up the live tree when VSS is refused, and marks
	// the run degraded. See restic.BackupOptions.
	AllowVSSFallback bool

	// OneFileSystem keeps restic out of mounted network shares and external
	// drives.
	OneFileSystem bool

	// Paused stops the scheduler without losing the plan. Somebody travelling
	// on a metered connection needs this, and the alternative they will
	// otherwise reach for is uninstalling the program.
	Paused bool

	// UpdatedAt is when the plan last changed.
	UpdatedAt time.Time
}

// Validate reports a plan that would not produce a usable backup.
//
// Called before a plan is stored, not before it is used, so a bad plan cannot
// reach the disk and be found at 1am.
func (p Plan) Validate() error {
	switch {
	case p.NodeID == "":
		return errors.New("planbus: the plan needs a node ID, so the fleet dashboard can name this machine")
	case p.Repository == "":
		return errors.New("planbus: the plan needs a repository URL")
	case len(p.Targets) == 0:
		return errors.New("planbus: the plan has no targets; a backup of nothing would report success")
	}

	return p.Schedule.Validate()
}

// Storer is the persistence this domain needs.
type Storer interface {
	Get(ctx context.Context) (Plan, error)
	Put(ctx context.Context, p Plan) error
	Seeded(ctx context.Context) (bool, error)
	MarkSeeded(ctx context.Context) error
	GetMeasurement(ctx context.Context) (Measurement, error)
	PutMeasurement(ctx context.Context, m Measurement) error

	GetIntegrity(ctx context.Context) (Integrity, error)
	PutIntegrity(ctx context.Context, i Integrity) error
}

// Business is the plan domain.
type Business struct {
	store Storer
}

// NewBusiness constructs it.
func NewBusiness(store Storer) *Business {
	return &Business{store: store}
}

// Get returns the plan, or ErrNoPlan.
func (b *Business) Get(ctx context.Context) (Plan, error) {
	return b.store.Get(ctx)
}

// Put replaces the plan.
func (b *Business) Put(ctx context.Context, p Plan, now time.Time) error {
	if err := p.Validate(); err != nil {
		return err
	}

	p.UpdatedAt = now

	return b.store.Put(ctx, p)
}

// Measurement returns what is known about the repository's size.
//
// A measurement taken against a different repository is discarded rather than
// returned: after a rotation the old figures describe a bucket that no longer
// exists, and showing them would suggest reclaiming space that is already
// gone. Since restarts at the caller's clock so the new repository begins
// accumulating history from today.
func (b *Business) Measurement(ctx context.Context, repositoryURL string, now time.Time) (Measurement, error) {
	m, err := b.store.GetMeasurement(ctx)
	if err != nil {
		return Measurement{}, err
	}

	if m.RepositoryURL != repositoryURL {
		return Measurement{RepositoryURL: repositoryURL, Since: now}, nil
	}

	return m, nil
}

// RecordMeasurement stores fresh figures, preserving Since.
func (b *Business) RecordMeasurement(ctx context.Context, repositoryURL string,
	now, since time.Time, size Size) error {

	if since.IsZero() {
		since = now
	}

	return b.store.PutMeasurement(ctx, Measurement{
		RepositoryURL: repositoryURL,
		Since:         since,
		MeasuredAt:    now,
		Now:           size.Now,
		Fresh:         size.Fresh,
		Snapshots:     size.Snapshots,
	})
}

// Size is the pair of figures a measurement is built from.
//
// Declared here rather than taking foundation/restic's type, so that this
// domain does not depend on how the numbers were obtained — the composition
// root converts.
type Size struct {
	Now       int64
	Fresh     int64
	Snapshots int
}

// Seed installs a plan on a machine that has none.
//
// It reports whether it did anything. A second call is a no-op and not an
// error: the installer re-running is a normal thing, and the plan the person
// has since edited on the status page is the one that must survive.
func (b *Business) Seed(ctx context.Context, p Plan, now time.Time) (bool, error) {
	done, err := b.store.Seeded(ctx)
	if err != nil {
		return false, err
	}

	if done {
		return false, nil
	}

	if err := p.Validate(); err != nil {
		return false, fmt.Errorf("the seed configuration is not usable: %w", err)
	}

	p.UpdatedAt = now

	if err := b.store.Put(ctx, p); err != nil {
		return false, err
	}

	if err := b.store.MarkSeeded(ctx); err != nil {
		return false, err
	}

	return true, nil
}

// Integrity returns the last repository check.
//
// A result for a different repository is discarded rather than returned: a
// machine moved to a new bucket has verified nothing about the one it is now
// writing to, and a stale "sound, three days ago" on the status page would be
// a lie about the wrong thing.
func (b *Business) Integrity(ctx context.Context, repositoryURL string) (Integrity, error) {
	i, err := b.store.GetIntegrity(ctx)
	if err != nil {
		return Integrity{}, err
	}

	if i.RepositoryURL != repositoryURL {
		return Integrity{}, nil
	}

	return i, nil
}

// RecordIntegrity stores the result of a check, or of a check not taken.
func (b *Business) RecordIntegrity(ctx context.Context, i Integrity) error {
	return b.store.PutIntegrity(ctx, i)
}
