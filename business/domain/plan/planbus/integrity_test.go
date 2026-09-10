package planbus_test

import (
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// TestASmallRepositoryIsStillActuallyRead is the bug the size form exists to
// prevent, found by measuring rather than reasoning: restic's fraction form
// selects whole pack files, and 1/52 of a repository with sixteen of them is
// none. The check passed, reported success, and read nothing.
func TestASmallRepositoryIsStillActuallyRead(t *testing.T) {
	for _, size := range []int64{0, 1 << 20, 257 << 20, 1 << 30} {
		got := planbus.CheckSubset(size)

		if got != "64M" {
			t.Errorf("CheckSubset(%d) = %q, want the 64M floor", size, got)
		}
	}
}

// TestALargeRepositoryDoesNotCostAnEvening. A fiftieth of half a terabyte is
// ten gigabytes, and a laptop that spends one evening in seven downloading its
// own backups is a laptop somebody switches this off on.
func TestALargeRepositoryDoesNotCostAnEvening(t *testing.T) {
	if got := planbus.CheckSubset(500 << 30); got != "1024M" {
		t.Errorf("CheckSubset(500 GiB) = %q, want the 1024M ceiling", got)
	}
}

// TestAnOrdinaryRepositoryGetsAFiftySecond, which is the case the whole
// arrangement is designed around: cover it in a year.
func TestAnOrdinaryRepositoryGetsAFiftySecond(t *testing.T) {
	// 104 GiB / 52 = 2 GiB, which is over the ceiling, so pick something that
	// lands in the middle: 20 GiB / 52 is about 393 MiB.
	if got := planbus.CheckSubset(20 << 30); got != "393M" {
		t.Errorf("CheckSubset(20 GiB) = %q, want 393M", got)
	}
}

// TestACheckIsDueWhenTheRepositoryChanged. A machine rotated to a new bucket
// has verified nothing about the one it is now writing to, however recently it
// checked the old one.
func TestACheckIsDueWhenTheRepositoryChanged(t *testing.T) {
	now := time.Now()

	i := planbus.Integrity{
		RepositoryURL: "s3:https://example/old",
		CheckedAt:     now.Add(-time.Hour),
		OK:            true,
	}

	if !i.Due("s3:https://example/new", now) {
		t.Error("a check of a different repository counted as this one")
	}

	if i.Due("s3:https://example/old", now) {
		t.Error("a check an hour ago was treated as due")
	}

	if !i.Due("s3:https://example/old", now.Add(planbus.CheckEvery)) {
		t.Error("a check a week ago was not treated as due")
	}
}

// TestNeverCheckedIsDue, because the zero value must not read as "checked at
// the epoch and therefore never again".
func TestNeverCheckedIsDue(t *testing.T) {
	var i planbus.Integrity

	if !i.Due("s3:https://example/repo", time.Now()) {
		t.Error("a machine that has never checked was not due")
	}

	if got := i.Describe(time.Now()); got != "never" {
		t.Errorf("Describe = %q, want never", got)
	}
}

// TestSkippedAndFailedReadDifferently. A machine that has skipped for six
// weeks because it is always tethered is a different problem from one whose
// repository is damaged, and somebody reading one line should not confuse them.
func TestSkippedAndFailedReadDifferently(t *testing.T) {
	now := time.Now()

	skipped := planbus.Integrity{CheckedAt: now.Add(-time.Hour), SkippedReason: "metered connection"}
	failed := planbus.Integrity{CheckedAt: now.Add(-time.Hour), Detail: "pack 1a2b is damaged"}

	if got := skipped.Describe(now); got == failed.Describe(now) {
		t.Fatal("a skipped check and a failed one read the same")
	}

	if got := failed.Describe(now); got[:6] != "FAILED" {
		t.Errorf("a damaged repository does not announce itself: %q", got)
	}
}
