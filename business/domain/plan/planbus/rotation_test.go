package planbus_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

const day = 24 * time.Hour

// bucket is a repository the server has told this machine about.
func bucket(ageDays int) planbus.Bucket {
	return planbus.Bucket{
		URL:       "s3:https://s3.example.invalid/example-node-bucket",
		CreatedAt: measuredAt().AddDate(0, 0, -ageDays),
		State:     planbus.StateActive,
	}
}

// sound and damaged are the two integrity results that mean something. A check
// that never ran and one that was skipped are deliberately not here: neither is
// evidence, and neither may raise the subject.
func sound() planbus.Integrity {
	return planbus.Integrity{CheckedAt: measuredAt().Add(-day), OK: true, Subset: "512M"}
}

func damaged() planbus.Integrity {
	return planbus.Integrity{
		CheckedAt: measuredAt().Add(-day),
		Subset:    "512M",
		Detail:    "pack 3f2a: ciphertext verification failed",
	}
}

// TestConsiderRaisesTheSubjectForTheRightReasons covers the three independent
// arguments and the cases each of them must NOT fire on.
func TestConsiderRaisesTheSubjectForTheRightReasons(t *testing.T) {
	cases := []struct {
		name string
		b    planbus.Bucket
		m    planbus.Measurement
		i    planbus.Integrity
		want planbus.Urgency
		why  string
	}{
		{
			name: "young and tidy",
			b:    bucket(30),
			m:    measurement(30, 340*gib, 300*gib),
			i:    sound(),
			want: planbus.RotationNone,
			why:  "nothing to say about a month-old bucket",
		},
		{
			name: "bloated, on the space argument alone",
			b:    bucket(200),
			m:    measurement(200, 340*gib, 150*gib),
			i:    sound(),
			want: planbus.RotationWorthRaising,
			why:  "56% of it is history and that is worth 190 GiB",
		},
		{
			name: "too young for the space argument",
			b:    bucket(30),
			m:    measurement(30, 340*gib, 150*gib),
			i:    sound(),
			want: planbus.RotationNone,
			why:  "a new repository always looks bloated for a while",
		},
		{
			name: "old but tidy — files barely change, history is nearly free",
			b:    bucket(300),
			m:    measurement(300, 160*gib, 150*gib),
			i:    sound(),
			want: planbus.RotationNone,
			why:  "6% reclaimable, and not yet a year old",
		},
		{
			name: "a large fraction of a tiny repository",
			b:    bucket(300),
			m:    measurement(300, 3*gib, 1*gib),
			i:    sound(),
			want: planbus.RotationNone,
			why:  "2 GiB is not worth days of somebody's uplink",
		},
		{
			name: "a year old and tidy",
			b:    bucket(400),
			m:    measurement(400, 160*gib, 150*gib),
			i:    sound(),
			want: planbus.RotationWorthRaising,
			why:  "the password and both key pairs are a year old, whatever the shape",
		},
		{
			name: "two years old",
			b:    bucket(800),
			m:    measurement(800, 160*gib, 150*gib),
			i:    sound(),
			want: planbus.RotationOverdue,
			why:  "past InsistAfter",
		},
		{
			name: "young, tidy, and the last check failed",
			b:    bucket(30),
			m:    measurement(30, 340*gib, 300*gib),
			i:    damaged(),
			want: planbus.RotationUrgent,
			why:  "a repository that cannot be read back outranks age and shape",
		},
		{
			name: "never checked",
			b:    bucket(30),
			m:    measurement(30, 340*gib, 300*gib),
			i:    planbus.Integrity{},
			want: planbus.RotationNone,
			why:  "a check that never ran is not evidence of damage",
		},
		{
			name: "checks skipped on a metered connection",
			b:    bucket(30),
			m:    measurement(30, 340*gib, 300*gib),
			i:    planbus.Integrity{CheckedAt: measuredAt().Add(-day), SkippedReason: "metered connection"},
			want: planbus.RotationNone,
			why:  "a laptop that has been travelling is not a damaged repository",
		},
		{
			name: "never measured, but two years old",
			b:    bucket(800),
			m:    planbus.Measurement{},
			i:    sound(),
			want: planbus.RotationOverdue,
			why:  "the age argument does not need a measurement",
		},
		{
			name: "the server has never answered",
			b:    planbus.Bucket{},
			m:    measurement(800, 160*gib, 150*gib),
			i:    sound(),
			want: planbus.RotationNone,
			why:  "no age to argue from, and the shape alone does not qualify",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planbus.Consider(measuredAt(), c.b, c.m, c.i)

			if got.Urgency != c.want {
				t.Errorf("Urgency = %v, want %v (%s)", got.Urgency, c.want, c.why)
			}

			if got.Show != (c.want > planbus.RotationNone) {
				t.Errorf("Show = %v at urgency %v", got.Show, got.Urgency)
			}

			if got.Show && len(got.Reasons) == 0 {
				t.Error("the subject was raised with nothing to say about why")
			}
		})
	}
}

// TestNothingHereEverActs is the promise the whole design rests on, and it is
// worth a test rather than a comment.
//
// An assessment is a sentence and a level. There is no field on it that
// anything could read as "and therefore do this", at any urgency — not even
// the urgent one, where the temptation is strongest. The machine raises the
// subject; a person clicks.
func TestNothingHereEverActs(t *testing.T) {
	got := planbus.Consider(measuredAt(), bucket(900), measurement(900, 340*gib, 10*gib), damaged())

	if got.Urgency != planbus.RotationUrgent {
		t.Fatalf("Urgency = %v, want urgent", got.Urgency)
	}

	// Every reason that applies is reported, not just the winning one. Somebody
	// deciding whether to spend days of their connection is owed all of it.
	if len(got.Reasons) != 3 {
		t.Errorf("Reasons = %d, want all three (space, age, integrity): %q",
			len(got.Reasons), got.Reasons)
	}
}

// TestAnAdoptedBucketIsOldByConstruction pins the behaviour that will surprise
// somebody the first day this ships.
//
// On an adopted bucket the server's created_at is where the SNAPSHOTS start,
// which is years before Eumaeus ever heard of the machine. Every laptop
// migrated off a legacy script therefore lands past InsistAfter immediately.
// That is right — such a bucket really has been accumulating for years and its
// password really did come from the old install — and it is precisely why
// nothing in this package files a request or starts an upload by itself.
func TestAnAdoptedBucketIsOldByConstruction(t *testing.T) {
	b := bucket(7 * 365)
	b.Adopted = true

	got := planbus.Consider(measuredAt(), b, measurement(30, 160*gib, 150*gib), sound())

	if got.Urgency != planbus.RotationOverdue {
		t.Errorf("Urgency = %v, want overdue for a seven-year-old adopted bucket", got.Urgency)
	}
}

// TestTheOfferCarriesBothSidesOfTheTrade. The page asks somebody for days of
// bandwidth and every snapshot they have. Whatever the arithmetic decides, the
// numbers for both halves have to be there to show them.
func TestTheOfferCarriesBothSidesOfTheTrade(t *testing.T) {
	got := planbus.Consider(measuredAt(), bucket(200), measurement(200, 340*gib, 150*gib), sound())

	if got.Reclaimable != 190*gib {
		t.Errorf("Reclaimable = %d, want %d", got.Reclaimable, 190*gib)
	}

	if got.Upload != 150*gib {
		t.Errorf("Upload = %d, want the fresh size", got.Upload)
	}

	if got.Discards != 200*day {
		t.Errorf("Discards = %v, want 200 days of history", got.Discards)
	}

	if got.Fraction < 0.55 || got.Fraction > 0.57 {
		t.Errorf("Fraction = %.3f, want ~0.559", got.Fraction)
	}
}

// TestTheHistoryHorizonPrefersTheServer. The local record starts when this
// program first wrote to the bucket; on an adopted one that is years after the
// snapshots it would be discarding, and telling an owner they would lose "three
// months" of history when it is really nine years is the worst kind of wrong.
func TestTheHistoryHorizonPrefersTheServer(t *testing.T) {
	got := planbus.Consider(measuredAt(), bucket(900), measurement(90, 340*gib, 150*gib), sound())

	if got.Discards != 900*day {
		t.Errorf("Discards = %v, want the server's 900 days rather than the local 90", got.Discards)
	}

	// And falls back to the local record when the server has said nothing,
	// rather than claiming there is no history at all.
	local := planbus.Consider(measuredAt(), planbus.Bucket{}, measurement(90, 340*gib, 150*gib), sound())

	if local.Discards != 90*day {
		t.Errorf("Discards = %v, want the local 90 days", local.Discards)
	}
}

// TestTheEstimateIsWhatTheDecisionTurnsOn. A byte count means nothing to the
// person being asked; "about four days" is the whole question, and it is the
// number the server cannot compute because it cannot see the link.
func TestTheEstimateIsWhatTheDecisionTurnsOn(t *testing.T) {
	got := planbus.Consider(measuredAt(), bucket(400), measurement(400, 340*gib, 150*gib), sound())

	// 150 GiB at 3 MiB/s, which is what was measured against a real bucket.
	estimate := got.Estimate(3 << 20)

	if estimate < 13*time.Hour || estimate > 16*time.Hour {
		t.Errorf("Estimate = %v, want about fourteen hours", estimate)
	}

	// Somebody on rural broadband gets a very different answer from the same
	// byte count, which is the entire reason this is decided here.
	if slow := got.Estimate(200 << 10); slow < 7*24*time.Hour {
		t.Errorf("Estimate at 200 KiB/s = %v, want more than a week", slow)
	}

	if none := got.Estimate(0); none != 0 {
		t.Errorf("Estimate with nothing measured = %v, want zero", none)
	}
}

func TestMonthlySaving(t *testing.T) {
	offer := planbus.Consider(measuredAt(), bucket(200), measurement(200, 340*gib, 150*gib), sound())

	// 190 GiB at $6.99/TiB/month is about $1.30.
	if got := offer.MonthlySaving(6.99); !strings.Contains(got, "$1") {
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

// TestAStateForAnotherBucketIsNotAnAnswer. After a cutover the stored poll
// describes the bucket this machine has left, and showing its age or its offer
// beside the new repository would be a fact about the wrong thing.
func TestAStateForAnotherBucketIsNotAnAnswer(t *testing.T) {
	state := planbus.MachineState{
		AskedAt: measuredAt(),
		Bucket:  bucket(900),
	}

	if !state.Describes(bucket(900).URL) {
		t.Error("a state does not describe the bucket it names")
	}

	if state.Describes("s3:https://s3.example.invalid/somewhere-else") {
		t.Error("a state describes a bucket it does not name")
	}

	if (planbus.MachineState{}).Describes(bucket(900).URL) {
		t.Error("a state nobody has ever fetched describes something")
	}
}

// TestTheCardSignalIsTheStateAndNotItsDate. issued_at is null alongside
// "superseded", so a client testing it for "has a card" reads the one state
// that means "destroy the old paper" as "there is no card".
func TestTheCardSignalIsTheStateAndNotItsDate(t *testing.T) {
	superseded := planbus.Card{State: planbus.CardSuperseded}

	if !superseded.Owed() {
		t.Error("a superseded card is not owed a reprint")
	}

	if !superseded.DestroyTheOld() {
		t.Error("a superseded card does not say to destroy the old one")
	}

	// A cutover reads "never", and it must not tell an owner to shred the card
	// that is currently the only way into the only bucket holding any history.
	never := planbus.Card{State: planbus.CardNever}

	if !never.Owed() {
		t.Error("a bucket with no card is not owed one")
	}

	if never.DestroyTheOld() {
		t.Error("a card that was never printed says to destroy something")
	}

	if (planbus.Card{State: planbus.CardIssued}).Owed() {
		t.Error("an issued card is owed a reprint")
	}
}
