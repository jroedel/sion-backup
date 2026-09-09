package planbus_test

import (
	"strings"
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

func TestConsiderRotation(t *testing.T) {
	cases := []struct {
		name string
		m    planbus.Measurement
		show bool
		why  string
	}{
		{
			name: "old, bloated and worth it",
			m:    measurement(200, 340*gib, 150*gib),
			show: true,
		},
		{
			name: "too young — a new repository always looks bad for a while",
			m:    measurement(30, 340*gib, 150*gib),
			why:  "under the age threshold",
		},
		{
			name: "old but tidy — files barely change, history is nearly free",
			m:    measurement(400, 160*gib, 150*gib),
			why:  "only 6% reclaimable",
		},
		{
			name: "large fraction of a tiny repository",
			m:    measurement(400, 3*gib, 1*gib),
			why:  "2 GiB is not worth two days of uploading",
		},
		{
			name: "never measured",
			m:    planbus.Measurement{Since: measuredAt().AddDate(0, 0, -400)},
			why:  "no figures yet",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.m.ConsiderRotation(measuredAt())

			if got.Show != c.show {
				t.Errorf("Show = %v, want %v (%s)", got.Show, c.show, c.why)
			}
		})
	}
}

// TestTheOfferCarriesBothSidesOfTheTrade. The card asks somebody for two days
// of bandwidth and every snapshot they have. Whatever the arithmetic decides,
// the numbers for both halves have to be there to show them.
func TestTheOfferCarriesBothSidesOfTheTrade(t *testing.T) {
	got := measurement(200, 340*gib, 150*gib).ConsiderRotation(measuredAt())

	if got.Reclaimable != 190*gib {
		t.Errorf("Reclaimable = %d, want %d", got.Reclaimable, 190*gib)
	}

	if got.Upload != 150*gib {
		t.Errorf("Upload = %d, want the fresh size", got.Upload)
	}

	if got.Discards != 200*24*time.Hour {
		t.Errorf("Discards = %v, want 200 days of history", got.Discards)
	}

	if got.Fraction < 0.55 || got.Fraction > 0.57 {
		t.Errorf("Fraction = %.3f, want ~0.559", got.Fraction)
	}
}

func TestMonthlySaving(t *testing.T) {
	offer := measurement(200, 340*gib, 150*gib).ConsiderRotation(measuredAt())

	// 190 GiB at $6.99/TiB/month is about $1.30.
	got := offer.MonthlySaving(6.99)
	if !strings.Contains(got, "$1") {
		t.Errorf("MonthlySaving = %q", got)
	}

	// No price configured means no money in the copy at all. A wrong figure is
	// worse than none — it is the sort of thing that gets quoted in a meeting.
	if got := offer.MonthlySaving(0); got != "" {
		t.Errorf("a saving was quoted with no price configured: %q", got)
	}

	big := planbus.Offer{Reclaimable: 4 << 40} // 4 TiB
	if got := big.MonthlySaving(6.99); !strings.Contains(got, "a year") {
		t.Errorf("a large saving should name the annual figure: %q", got)
	}
}

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
