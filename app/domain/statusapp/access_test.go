package statusapp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
)

// trail is a Eumaeus that answers with whatever the test wants.
type trail struct {
	log disclosurebus.Log
	err error
}

func (t *trail) List(context.Context, int) (disclosurebus.Log, error) { return t.log, t.err }

// heads is the witness table, in memory.
type heads struct {
	kept []disclosurebus.Keep
}

func (h *heads) Remember(_ context.Context, w disclosurebus.Witness, seen time.Time) error {
	for _, k := range h.kept {
		if k.Count == w.Count {
			return nil
		}
	}

	h.kept = append(h.kept, disclosurebus.Keep{Witness: w, Seen: seen})

	return nil
}

func (h *heads) Kept(context.Context) ([]disclosurebus.Keep, error) { return h.kept, nil }

var seenInMarch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

// aRecord is one machine fetch and one person, which is the shape of every
// real machine's record: hundreds of the first and a handful of the second.
func aRecord() disclosurebus.Log {
	return disclosurebus.Log{
		Count: 412,
		Head:  "9f2c",
		Entries: []disclosurebus.Entry{
			{
				Seq: 412, At: time.Date(2026, 9, 9, 1, 14, 2, 0, time.UTC),
				Kind: disclosurebus.KindFetched, Actor: "sion-backup/0.5.3",
				FromIP: "203.0.113.9", CredentialsVersion: 3, ByMachine: true,
				Describe: "This computer collected the password to run a backup.",
				PrevHash: "1a2b", Hash: "9f2c",
			},
			{
				Seq: 411, At: time.Date(2026, 9, 8, 14, 2, 0, 0, time.UTC),
				Kind: disclosurebus.KindRevealed, Actor: "Fr Jeff Roedel",
				FromIP: "198.51.100.4", CredentialsVersion: 3, ByMachine: false,
				Describe: "A person read the password.",
				PrevHash: "", Hash: "1a2b",
			},
		},
	}
}

func withRecord(t *testing.T, log disclosurebus.Log, kept ...disclosurebus.Keep) *harness {
	t.Helper()

	b := disclosurebus.NewBusiness(&trail{log: log}, &heads{kept: kept})

	return harnessWithDisclosures(t, stubSource{}, b)
}

// TestThePersonIsShownAboveTheMachine.
//
// The entries where a human being saw the password are what the record is
// printed for; the machine's own fetches are the backup working. A page that
// buried the two lines that matter in four hundred that do not would be a page
// nobody reads, which is the failure this whole program is built against.
func TestThePersonIsShownAboveTheMachine(t *testing.T) {
	body := withRecord(t, aRecord()).get(t, "/access").Body.String()

	person := strings.Index(body, "Fr Jeff Roedel")
	machine := strings.Index(body, "sion-backup/0.5.3")

	switch {
	case person < 0:
		t.Fatal("the person who read the password is not on the page")
	case machine < 0:
		t.Fatal("this computer's own fetches are not on the page")
	case person > machine:
		t.Error("the person is shown below the machine's routine fetches")
	}
}

// TestThePageSaysWhatItCannotProve.
//
// The one sentence that must never be dropped for being awkward. Anybody who
// can read the server's vault file reads the password without leaving an entry
// here, and a transparency claim that overstates itself spends the trust it
// was meant to earn.
func TestThePageSaysWhatItCannotProve(t *testing.T) {
	body := withRecord(t, aRecord()).get(t, "/access").Body.String()

	if !strings.Contains(body, "does not prove") {
		t.Error("the page does not say what it cannot prove")
	}

	if !strings.Contains(body, "vault") {
		t.Error("the page does not name the way a password can be read without appearing here")
	}
}

// TestTheCheckNamesTheDateItVouchesFrom.
func TestTheCheckNamesTheDateItVouchesFrom(t *testing.T) {
	kept := disclosurebus.Keep{
		Witness: disclosurebus.Witness{Count: 411, Hash: "1a2b"},
		Seen:    seenInMarch,
	}

	body := withRecord(t, aRecord(), kept).get(t, "/access").Body.String()

	if !strings.Contains(body, "has not been altered since") {
		t.Error("the page does not say the record is unaltered")
	}

	if !strings.Contains(body, "14 March") && !strings.Contains(body, "Mar") {
		t.Errorf("the page does not name the date it vouches from:\n%s", body)
	}
}

// TestTamperingIsNotQuiet.
func TestTamperingIsNotQuiet(t *testing.T) {
	kept := disclosurebus.Keep{
		Witness: disclosurebus.Witness{Count: 900, Hash: "whatever"},
		Seen:    seenInMarch,
	}

	body := withRecord(t, aRecord(), kept).get(t, "/access").Body.String()

	if !strings.Contains(body, "verdict bad") {
		t.Error("a record that has lost entries is not shown as bad")
	}

	if !strings.Contains(body, "shorter than it was") {
		t.Error("the page does not say what is wrong")
	}
}

// TestAServerWithoutTheRecordIsNotAnAlarm.
//
// A fleet mid-upgrade is a fleet where this is the normal answer for a while.
// It must not look like a fault in somebody's backups.
func TestAServerWithoutTheRecordIsNotAnAlarm(t *testing.T) {
	// The real source wraps the sentinel, and the page matches on errors.Is,
	// so a wrapped one is what this hands over.
	b := disclosurebus.NewBusiness(&trail{
		err: fmt.Errorf("eumaeusdisclosure: %w", disclosurebus.ErrUnsupported),
	}, &heads{})

	h := harnessWithDisclosures(t, stubSource{}, b)
	rec := h.get(t, "/access")

	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}

	body := rec.Body.String()

	switch {
	case !strings.Contains(body, "does not keep this record yet"):
		t.Error("the page does not explain a server that has not deployed it")
	case strings.Contains(body, "verdict bad"):
		t.Error("a server without the record is shown as an alarm")
	}
}

// TestAMachineWithNoTokenSaysSoRatherThanFailing.
func TestAMachineWithNoTokenSaysSoRatherThanFailing(t *testing.T) {
	rec := notEnrolled(t).get(t, "/access")

	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "not enrolled") {
		t.Error("the page does not say why there is nothing to show")
	}
}

// TestTheLinkIsOnTheStatusPageEvenBeforeAnythingIsKnown.
//
// A link that appears only once there is something to show is a link nobody
// learns to look for, and this one has to be findable by somebody who has been
// told to go and check.
func TestTheLinkIsOnTheStatusPageEvenBeforeAnythingIsKnown(t *testing.T) {
	h := withRecord(t, aRecord())

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/").Body.String()

	if !strings.Contains(body, `href="/access"`) {
		t.Error("the status page does not link to the record")
	}
}

// TestTheStatusLineCostsNoRoundTrip.
//
// The front page refreshes itself every five seconds while a backup runs. The
// line it shows about this record comes from what is already written down on
// this disk, so a source that fails loudly must make no difference to it.
func TestTheStatusLineCostsNoRoundTrip(t *testing.T) {
	kept := disclosurebus.Keep{
		Witness: disclosurebus.Witness{
			Count: 412, Hash: "9f2c",
			At: time.Date(2026, 9, 9, 1, 14, 2, 0, time.UTC),
		},
		Seen: seenInMarch,
	}

	b := disclosurebus.NewBusiness(
		&trail{err: errors.New("the status page must not call the server")},
		&heads{kept: []disclosurebus.Keep{kept}})

	h := harnessWithDisclosures(t, stubSource{}, b)

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/").Body.String()

	if !strings.Contains(body, "Handed out 412 times") {
		t.Errorf("the status page does not say how many times the password has gone out:\n%s", body)
	}
}

// TestACappedListDoesNotCountAsIfItWereTheWholeRecord.
//
// The page shows the two hundred most recent of four hundred entries, and the
// tally above them is a tally of what is visible. Printing "a person saw it 2
// times" over a list that reaches back three weeks would be the same species
// of overstatement as claiming the record proves nobody read the vault file —
// and it is the one a reader is far more likely to believe.
func TestACappedListDoesNotCountAsIfItWereTheWholeRecord(t *testing.T) {
	body := withRecord(t, aRecord()).get(t, "/access").Body.String()

	if !strings.Contains(body, "at least") {
		t.Error("a capped list is counted as though it were the whole record")
	}

	if !strings.Contains(body, "not the whole story") {
		t.Error("the page does not say the tally is of what is shown")
	}
}

// TestAnUncappedRecordIsCountedPlainly, with no hedge to read past.
func TestAnUncappedRecordIsCountedPlainly(t *testing.T) {
	log := aRecord()
	log.Count = int64(len(log.Entries))

	body := withRecord(t, log).get(t, "/access").Body.String()

	if strings.Contains(body, "at least") {
		t.Error("a complete record is hedged as though it were capped")
	}
}
