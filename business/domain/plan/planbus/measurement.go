package planbus

import "time"

// Measurement is what the repository currently costs, and what a fresh one
// would cost.
//
// Kept per repository URL. When the URL changes the figures are meaningless
// and Since restarts — a new bucket has no history to reclaim.
type Measurement struct {
	// RepositoryURL is what these figures describe. A measurement for a
	// different repository than the plan names is stale and ignored.
	RepositoryURL string

	// Since is when this machine first backed up to this repository. It is the
	// history horizon the status page shows, and half of the rotation
	// question: reclaiming space means discarding everything since this date.
	Since time.Time

	// MeasuredAt is when the figures were taken. `restic stats` walks the
	// whole index, so it is not run on every backup.
	MeasuredAt time.Time

	// Now is everything in the repository, deduplicated. Roughly the bill.
	Now int64

	// Fresh is what the newest snapshot alone needs — the floor a new
	// repository would start at.
	Fresh int64

	Snapshots int
}

// Reclaimable is what rotating would recover, which is also exactly the
// history it would discard.
func (m Measurement) Reclaimable() int64 {
	if m.Now <= m.Fresh {
		return 0
	}

	return m.Now - m.Fresh
}

// Fraction is the reclaimable share of the repository, 0 to 1.
func (m Measurement) Fraction() float64 {
	if m.Now <= 0 {
		return 0
	}

	return float64(m.Reclaimable()) / float64(m.Now)
}

// Stale reports whether the figures need retaking.
func (m Measurement) Stale(now time.Time, every time.Duration) bool {
	return m.MeasuredAt.IsZero() || now.Sub(m.MeasuredAt) >= every
}
