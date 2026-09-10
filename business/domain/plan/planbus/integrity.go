package planbus

import (
	"fmt"
	"time"
)

// Integrity is the last time this machine read its repository back and what
// happened.
//
// # Why this exists at all
//
// Nothing in this program used to call `restic check`. foundation/restic
// defined it and no command invoked it, so no machine in the fleet had ever
// verified that what is in the bucket is still what was put there. Every other
// check in the program is about whether the backup HAPPENED; this is the only
// one about whether it is still WORTH anything.
//
// The failure it guards against is quiet by construction. A pack file that is
// present, correctly named, the right length and corrupt reads as a healthy
// repository to everything else — the snapshot list is fine, the index is
// fine, `stats` is fine — right up until somebody needs the file inside it,
// which is the worst possible moment to find out.
//
// # Why a slice rather than the whole thing
//
// Reading a repository back means downloading all of it. Measured against a
// real bucket: a full read moves 100% of the repository, which for a laptop
// with 80 GB of documents is hours of transfer and, on a metered connection,
// somebody's money. A slice of 1/52 each week covers everything in a year and
// moves about two percent at a time, which is affordable on any connection
// this fleet is likely to be on.
//
// The metadata half is nearly free either way — a plain check moved about one
// percent of a repository's size in the same measurement — so it is done every
// time and the slice is what varies.
type Integrity struct {
	// RepositoryURL is what was checked. A result for a different repository
	// than the plan names is stale: the machine has been rotated to a new
	// bucket and has never verified this one.
	RepositoryURL string

	// CheckedAt is when the last check finished, successfully or not.
	CheckedAt time.Time

	// Subset is the slice of pack data that was read back, as restic spells
	// it. Empty means metadata only.
	Subset string

	// OK is whether the repository was sound.
	OK bool

	// Detail is restic's own words when it was not. Empty when it was.
	Detail string

	// SkippedReason records a check that did not happen, and why. A machine
	// that has skipped for six weeks because it is always on a phone is a
	// different problem from one that has never tried, and the difference
	// should be visible without reading a log.
	SkippedReason string
}

// CheckEvery is how often the repository is read back.
//
// Weekly, which is a compromise between two real costs. More often and a
// laptop spends its evenings downloading its own backups; less often and a
// repository can rot for a month before anybody hears. It also matches
// [CheckSubset]: one slice a week is one repository a year, up to the point
// where a year's worth would cost more in one evening than it is worth.
const CheckEvery = 7 * 24 * time.Hour

// The size of one week's slice, and why it is a size rather than a fraction.
//
// restic will take "1/52", and a fraction is the obvious way to say "cover the
// repository in a year". It is the wrong knob for this fleet twice over.
//
// At the small end it selects nothing. Measured against a real bucket: a
// 257 MiB repository holds about sixteen pack files, and 1/52 of sixteen packs
// rounds to none — the check ran, reported success, and read no data at all.
// A verification that silently verifies nothing is worse than none, because
// somebody believes it.
//
// At the large end it is unbounded. A 500 GB repository would read back 10 GB
// a week, on a laptop, possibly over a phone. The point of checking is to find
// rot before somebody needs the file; it is not worth making the machine
// unusable one evening in seven.
//
// So: a fraction, clamped at both ends. A small repository gets read entirely
// and often; a large one gets a bounded sample that finds corruption
// eventually rather than covering everything on a schedule. Detection here is
// probabilistic and always was — a year of full coverage still misses a pack
// that rots in month eleven.
const (
	// checkSlicePerWeek is the share of a repository one weekly check aims at:
	// a fifty-second, so a year covers it.
	checkSlicePerWeek = 52

	// minCheckBytes stops the slice rounding down to no packs at all.
	minCheckBytes = 64 << 20

	// maxCheckBytes bounds what one evening can cost. At the 3 MiB/s measured
	// against a real bucket this is about six minutes; the two-hour budget
	// this was sized against would allow far more, and does not need to be
	// spent.
	maxCheckBytes = 1 << 30
)

// CheckSubset is the restic --read-data-subset spec for one week's slice of a
// repository of this size.
//
// A size rather than a fraction, because restic's size form reads at least
// that much and the fraction form can round to nothing. Zero or unknown size
// means the floor: a repository nobody has measured still gets checked.
func CheckSubset(repositoryBytes int64) string {
	want := repositoryBytes / checkSlicePerWeek

	switch {
	case want < minCheckBytes:
		want = minCheckBytes
	case want > maxCheckBytes:
		want = maxCheckBytes
	}

	// Whole mebibytes. restic parses the suffix, and a spec somebody may read
	// in a log should not be nine digits long.
	return fmt.Sprintf("%dM", want>>20)
}

// Due reports whether it is time to read the repository back.
//
// A result for a different repository is not a result: a machine moved to a
// new bucket has verified nothing about the one it is now writing to.
func (i Integrity) Due(repository string, now time.Time) bool {
	if i.RepositoryURL != repository {
		return true
	}

	return now.Sub(i.CheckedAt) >= CheckEvery
}

// Describe is one line for doctor and the status page.
func (i Integrity) Describe(now time.Time) string {
	if i.CheckedAt.IsZero() {
		return "never"
	}

	ago := now.Sub(i.CheckedAt).Round(time.Minute)

	switch {
	case i.SkippedReason != "":
		return fmt.Sprintf("skipped %s ago: %s", ago, i.SkippedReason)
	case !i.OK:
		return fmt.Sprintf("FAILED %s ago: %s", ago, i.Detail)
	case i.Subset == "":
		return fmt.Sprintf("metadata only, %s ago", ago)
	}

	return fmt.Sprintf("sound %s ago, having re-read %s of the data", ago, i.Subset)
}
