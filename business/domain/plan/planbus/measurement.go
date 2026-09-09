package planbus

import (
	"fmt"
	"time"
)

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

// Rotation thresholds.
//
// Three conditions, all of which must hold, because each one on its own
// produces a bad suggestion:
//
//   - Age alone would nag the owner of a machine whose files never change,
//     where a year of history costs almost nothing.
//   - Fraction alone would fire in week two of a new repository, when a few
//     large deletions briefly make the ratio look terrible.
//   - An absolute floor keeps it quiet about savings too small to be worth a
//     conversation, let alone two days of somebody's uplink.
const (
	// MinAgeForRotation is how long a repository must have been accumulating.
	MinAgeForRotation = 90 * 24 * time.Hour

	// MinFractionForRotation is how much of it must be recoverable.
	MinFractionForRotation = 0.30

	// MinBytesForRotation is the floor below which it is not worth mentioning.
	MinBytesForRotation = 5 << 30 // 5 GiB
)

// Offer is the suggestion shown on the status page, or the absence of one.
//
// The wording matters more than the arithmetic here. Rotation costs the owner
// two days of upload and every snapshot older than today, and it saves the
// organisation money. Somebody being asked to trade the first for the second
// is entitled to see both numbers in the same sentence, and to say no.
type Offer struct {
	// Show is whether to put this in front of the person at all.
	Show bool

	// Reclaimable is the space a fresh repository would not need.
	Reclaimable int64

	// Fraction of the repository that is history rather than current data.
	Fraction float64

	// Discards is how much history rotating would throw away.
	Discards time.Duration

	// Upload is what the fresh backup would have to send.
	Upload int64
}

// ConsiderRotation decides whether to raise the subject.
//
// A suggestion and never an action. Nothing here rotates anything: the machine
// cannot create a bucket, and the point of surfacing it locally is that the
// person who pays the bandwidth gets to be the one who asks.
func (m Measurement) ConsiderRotation(now time.Time) Offer {
	offer := Offer{
		Reclaimable: m.Reclaimable(),
		Fraction:    m.Fraction(),
		Upload:      m.Fresh,
	}

	if !m.Since.IsZero() {
		offer.Discards = now.Sub(m.Since)
	}

	switch {
	case m.MeasuredAt.IsZero(), m.Now <= 0:
		return offer
	case offer.Discards < MinAgeForRotation:
		return offer
	case offer.Fraction < MinFractionForRotation:
		return offer
	case offer.Reclaimable < MinBytesForRotation:
		return offer
	}

	offer.Show = true

	return offer
}

// MonthlySaving converts reclaimable bytes into money, or "" when no price is
// configured.
//
// Deliberately conservative and deliberately vague in its wording: storage is
// billed in ways this program does not model — per-account minimums, minimum
// retention periods — so a figure presented as exact would eventually be wrong
// in a way that costs the next number its credibility.
func (o Offer) MonthlySaving(pricePerTiBMonth float64) string {
	if pricePerTiBMonth <= 0 || o.Reclaimable <= 0 {
		return ""
	}

	perMonth := float64(o.Reclaimable) / float64(1<<40) * pricePerTiBMonth

	if perMonth < 0.5 {
		return "under $1 a month"
	}

	return fmt.Sprintf("about $%.0f a month, $%.0f a year", perMonth, perMonth*12)
}
