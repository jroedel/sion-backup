package planbus_test

import (
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

const gib = 1 << 30

func measurement(ageDays int, now, fresh int64) planbus.Measurement {
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	return planbus.Measurement{
		RepositoryURL: "s3:https://s3.example.invalid/example-node-bucket",
		Since:         base.AddDate(0, 0, -ageDays),
		MeasuredAt:    base,
		Now:           now,
		Fresh:         fresh,
		Snapshots:     ageDays,
	}
}

// measuredAt is the fixed "now" these cases are reasoned from. Named to avoid
// colliding with schedule_test.go's at(), which parses a clock time.
func measuredAt() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }

func TestReclaimableIsNeverNegative(t *testing.T) {
	// Fresh can exceed Now briefly: the figures are taken by two separate
	// calls, and a backup can land between them.
	m := planbus.Measurement{Now: 100, Fresh: 140}

	if got := m.Reclaimable(); got != 0 {
		t.Errorf("Reclaimable = %d, want 0", got)
	}

	if got := m.Fraction(); got != 0 {
		t.Errorf("Fraction = %v, want 0", got)
	}
}

func TestStale(t *testing.T) {
	m := measurement(200, 10, 5)

	if m.Stale(measuredAt(), 7*24*time.Hour) {
		t.Error("a measurement taken now is stale")
	}

	if !m.Stale(measuredAt().AddDate(0, 0, 8), 7*24*time.Hour) {
		t.Error("a measurement from eight days ago is not stale")
	}

	if !(planbus.Measurement{}).Stale(measuredAt(), 7*24*time.Hour) {
		t.Error("a measurement that was never taken is not stale")
	}
}
