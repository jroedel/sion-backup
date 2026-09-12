package disclosurebus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
)

// chain builds a log of n entries whose hashes link up, so a test only has to
// describe the damage it wants to do to it.
//
// The hashes are not real SHA-256 values and deliberately do not need to be:
// nothing in this package recomputes them. It checks that the chain it was
// handed links up and that the heads it wrote down are still in it, which are
// the two things a client can check without a second implementation of the
// server's hash format — see the package comment.
func chain(n int) disclosurebus.Log {
	log := disclosurebus.Log{Count: int64(n)}

	at := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

	// Built oldest first, then reversed: the wire order is newest first, and
	// building it that way round is where an off-by-one in prev_hash hides.
	var asc []disclosurebus.Entry

	for i := 1; i <= n; i++ {
		e := disclosurebus.Entry{
			Seq:                int64(i),
			At:                 at.Add(time.Duration(i) * 24 * time.Hour),
			Kind:               disclosurebus.KindFetched,
			Actor:              "sion-backup/0.5.3",
			FromIP:             "203.0.113.9",
			CredentialsVersion: 1,
			ByMachine:          true,
			Describe:           "This computer collected the password to run a backup.",
			Hash:               hashOf(i),
		}

		if i > 1 {
			e.PrevHash = hashOf(i - 1)
		}

		asc = append(asc, e)
	}

	for i := len(asc) - 1; i >= 0; i-- {
		log.Entries = append(log.Entries, asc[i])
	}

	if n > 0 {
		log.Head = hashOf(n)
	}

	return log
}

func hashOf(seq int) string {
	return string(rune('a'+seq%26)) + "0000000" + string(rune('a'+seq%26))
}

type source struct {
	log disclosurebus.Log
	err error
}

func (s *source) List(context.Context, int) (disclosurebus.Log, error) {
	return s.log, s.err
}

type store struct {
	kept []disclosurebus.Keep
}

func (s *store) Remember(_ context.Context, w disclosurebus.Witness, seen time.Time) error {
	for _, k := range s.kept {
		if k.Count == w.Count {
			return nil
		}
	}

	s.kept = append(s.kept, disclosurebus.Keep{Witness: w, Seen: seen})

	return nil
}

func (s *store) Kept(context.Context) ([]disclosurebus.Keep, error) { return s.kept, nil }

func read(t *testing.T, log disclosurebus.Log, kept ...disclosurebus.Keep) disclosurebus.Report {
	t.Helper()

	b := disclosurebus.NewBusiness(&source{log: log}, &store{kept: kept})

	report, err := b.Read(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}

	return report
}

func keep(count int, seen time.Time) disclosurebus.Keep {
	return disclosurebus.Keep{
		Witness: disclosurebus.Witness{Count: int64(count), Hash: hashOf(count)},
		Seen:    seen,
	}
}

var lastWeek = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// TestTheFirstLookPromisesNothing.
//
// The verdict a machine gives before it has anything written down is the one
// most likely to be quietly wrong, because it is the state every machine in
// the fleet is in on the day this ships. It must not say the record is intact:
// it has no way to know that, and a reassuring sentence here would be the
// exact overstatement the whole feature is built to avoid.
func TestTheFirstLookPromisesNothing(t *testing.T) {
	report := read(t, chain(3))

	if report.Verdict != disclosurebus.VerdictFirstLook {
		t.Errorf("the first look gives %q, want %q",
			report.Verdict, disclosurebus.VerdictFirstLook)
	}

	if report.Verdict.Wrong() {
		t.Error("the first look reports something wrong, but nothing is wrong")
	}
}

// TestAKeptHeadIsWhatTheCheckRestsOn.
func TestAKeptHeadIsWhatTheCheckRestsOn(t *testing.T) {
	report := read(t, chain(5), keep(3, lastWeek))

	if report.Verdict != disclosurebus.VerdictAgrees {
		t.Fatalf("got %q, want %q", report.Verdict, disclosurebus.VerdictAgrees)
	}

	if !report.Against.Seen.Equal(lastWeek) {
		t.Errorf("the check vouches from %v, want %v", report.Against.Seen, lastWeek)
	}
}

// TestTheCheckVouchesFromTheOldestHeadItCouldTest, not the newest.
//
// The sentence on the page is a date, and the date is the value of the whole
// feature: "unaltered since March" and "unaltered since Tuesday" are very
// different claims and the second one is nearly worthless. Taking the newest
// head would be the easy mistake and would silently make the promise smaller
// every time somebody opened the page.
func TestTheCheckVouchesFromTheOldestHeadItCouldTest(t *testing.T) {
	march := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)

	report := read(t, chain(5), keep(2, march), keep(4, lastWeek))

	if !report.Against.Seen.Equal(march) {
		t.Errorf("the check vouches from %v, want the oldest tested head %v",
			report.Against.Seen, march)
	}
}

// TestAShorterChainThanWeHaveSeenIsTampering.
//
// Entries are only ever appended. A count that has fallen is, on its own,
// evidence that somebody removed one — and it is detectable even when the
// removed entry is one this machine never held a head for.
func TestAShorterChainThanWeHaveSeenIsTampering(t *testing.T) {
	report := read(t, chain(3), keep(5, lastWeek))

	if report.Verdict != disclosurebus.VerdictShrunk {
		t.Errorf("got %q, want %q", report.Verdict, disclosurebus.VerdictShrunk)
	}

	if !report.Verdict.Wrong() {
		t.Error("a shortened record is not reported as something wrong")
	}
}

// TestAnEditedEntryIsCaughtByTheHeadWeKept.
func TestAnEditedEntryIsCaughtByTheHeadWeKept(t *testing.T) {
	kept := keep(3, lastWeek)
	kept.Hash = "something else entirely"

	report := read(t, chain(5), kept)

	if report.Verdict != disclosurebus.VerdictRewritten {
		t.Errorf("got %q, want %q", report.Verdict, disclosurebus.VerdictRewritten)
	}
}

// TestAChainThatDoesNotLinkUpIsCaughtWithNothingKept.
//
// The one check that works on a machine's very first look, which is why it
// runs before anything is compared against the store.
func TestAChainThatDoesNotLinkUpIsCaughtWithNothingKept(t *testing.T) {
	log := chain(4)
	log.Entries[1].Hash = "not what the entry after it points at"

	if got := read(t, log).Verdict; got != disclosurebus.VerdictBroken {
		t.Errorf("got %q, want %q", got, disclosurebus.VerdictBroken)
	}
}

// TestAHeadThatDoesNotMatchTheNewestEntryIsCaught.
func TestAHeadThatDoesNotMatchTheNewestEntryIsCaught(t *testing.T) {
	log := chain(4)
	log.Head = "a head belonging to some other chain"

	if got := read(t, log).Verdict; got != disclosurebus.VerdictBroken {
		t.Errorf("got %q, want %q", got, disclosurebus.VerdictBroken)
	}
}

// TestACappedListDoesNotPretendToCheckWhatItCannotSee.
//
// The server caps the list; a head older than the cap reaches names an entry
// that is simply not in front of the client. Calling that agreement would be
// the worst bug in this package — a green sentence resting on nothing.
func TestACappedListDoesNotPretendToCheckWhatItCannotSee(t *testing.T) {
	log := chain(10)
	log.Entries = log.Entries[:3] // the newest three of ten

	report := read(t, log, keep(2, lastWeek))

	if report.Verdict != disclosurebus.VerdictUnproven {
		t.Errorf("got %q, want %q", report.Verdict, disclosurebus.VerdictUnproven)
	}

	if report.Verdict.Wrong() {
		t.Error("an unproven record is reported as tampering, which it is not")
	}
}

// TestAGapInACappedListIsNotItselfTampering.
//
// Two entries that are not adjacent say nothing about each other, because the
// server may cap a list anywhere. Treating a gap as a broken chain would put a
// red banner in front of everybody whose log is longer than the page asks for.
func TestAGapInACappedListIsNotItselfTampering(t *testing.T) {
	log := chain(10)
	log.Entries = append(log.Entries[:2], log.Entries[5:]...)

	if got := read(t, log, keep(10, lastWeek)).Verdict; got.Wrong() {
		t.Errorf("a gap in a capped list gives %q, want no complaint", got)
	}
}

// TestTheHeadIsKeptEvenWhenTheVerdictIsBad.
//
// A machine that stopped writing things down the moment something looked wrong
// would destroy the evidence of what it was shown, and the next look would
// have nothing to compare against.
func TestTheHeadIsKeptEvenWhenTheVerdictIsBad(t *testing.T) {
	s := &store{kept: []disclosurebus.Keep{keep(9, lastWeek)}}
	b := disclosurebus.NewBusiness(&source{log: chain(3)}, s)

	if _, err := b.Read(context.Background(), 200); err != nil {
		t.Fatal(err)
	}

	for _, k := range s.kept {
		if k.Count == 3 {
			return
		}
	}

	t.Error("the head this machine was shown was not written down")
}

// TestThePeopleAreSeparatedFromTheMachine.
//
// The one distinction the page is built on, and it is taken from the server's
// by_machine field rather than from the kind, so that a kind invented after
// this build still lands on the correct side of it.
func TestThePeopleAreSeparatedFromTheMachine(t *testing.T) {
	log := chain(3)
	log.Entries[0].ByMachine = false
	log.Entries[0].Kind = "some-kind-from-the-future"

	report := read(t, log)

	if report.PeopleCount != 1 || report.MachineCount != 2 {
		t.Fatalf("got %d people and %d machine entries, want 1 and 2",
			report.PeopleCount, report.MachineCount)
	}

	if report.People[0].Kind.Known() {
		t.Error("a kind from the future is reported as one this build knows")
	}
}

// TestAnEmptyRecordIsARealAnswer.
//
// A provisioned machine that has not enrolled has a password nobody has ever
// been given. That must read as an empty record and not as a broken one.
func TestAnEmptyRecordIsARealAnswer(t *testing.T) {
	report := read(t, disclosurebus.Log{})

	if report.Verdict.Wrong() {
		t.Errorf("an empty record gives %q, want no complaint", report.Verdict)
	}
}

// TestAMachineWithNoTokenSaysSo.
func TestAMachineWithNoTokenSaysSo(t *testing.T) {
	b := disclosurebus.NewBusiness(nil, nil)

	if b.Available() {
		t.Error("a business with no source reports itself available")
	}

	if _, err := b.Read(context.Background(), 200); !errors.Is(err, disclosurebus.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

// TestTheGlanceCostsNoNetworkCall.
//
// The status page refreshes itself every five seconds while a backup runs, and
// the line it shows about this record must come from what is already written
// down here. A Glance that called the server would put a round trip in the
// path of the page whose job is to say whether the computer is backing up.
func TestTheGlanceCostsNoNetworkCall(t *testing.T) {
	s := &store{kept: []disclosurebus.Keep{keep(3, lastWeek), keep(7, lastWeek)}}
	b := disclosurebus.NewBusiness(&source{err: errors.New("the network is not to be touched")}, s)

	glance, err := b.Glance(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !glance.Known || glance.Count != 7 {
		t.Errorf("got %+v, want the newest kept head, 7", glance)
	}
}

// TestTheGlanceSaysNothingBeforeTheFirstLook.
func TestTheGlanceSaysNothingBeforeTheFirstLook(t *testing.T) {
	b := disclosurebus.NewBusiness(&source{}, &store{})

	glance, err := b.Glance(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if glance.Known {
		t.Errorf("got %+v, want nothing known yet", glance)
	}
}
