package statusapp

import (
	"errors"
	"net/http"
	"time"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
)

// accessLimit is how many entries the page asks for.
//
// Generous rather than minimal, and the reason is the tamper check rather than
// the display: a head this machine wrote down months ago can only be tested if
// the list reaches back far enough to contain the entry it names. At roughly
// one disclosure a day, two hundred is most of a year, and the entries are
// small.
const accessLimit = 200

// accessView is the page at /access.
type accessView struct {
	chrome

	// Available is whether this machine is enrolled and talking to a Eumaeus
	// that serves a trail at all. False fills the page with the reason rather
	// than an empty table.
	Available bool
	Reason    string

	Report disclosurebus.Report

	// Check is the sentence and the colour for what this machine can say about
	// the log it was shown.
	Check accessCheck

	// MachineNewest is when this computer last collected the password, which
	// is the one thing worth saying about the four hundred rows the page does
	// not print.
	MachineNewest time.Time

	// Since is the beginning of the period the chain covers where that is not
	// the beginning of the log. Zero means it covers everything.
	Since time.Time
}

// accessCheck is the verdict, in words a person can act on.
type accessCheck struct {
	Level   string // good | warn | bad
	Heading string
	Detail  string
}

// access serves the disclosure trail.
//
// A page of its own rather than a block on the status page. The status page
// answers "is this computer backing up", and it answers it in one sentence at
// the top; this answers a different question, and the honest caveat it has to
// carry — that a disclosure made by reading the server's vault directly never
// appears here — needs room to be read rather than a line of small print
// underneath a table.
func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	view := accessView{chrome: s.chromeFor("Access", "/access")}

	if s.cfg.Disclosures == nil || !s.cfg.Disclosures.Available() {
		view.Reason = "This computer is not enrolled, so it has never been given " +
			"a password and there is nothing to show."

		s.render(w, r, "access.html", view)

		return
	}

	report, err := s.cfg.Disclosures.Read(r.Context(), accessLimit)

	switch {
	case errors.Is(err, disclosurebus.ErrUnsupported):
		view.Reason = "The server this computer reports to does not keep this " +
			"record yet. Nothing is wrong with your backups."

		s.render(w, r, "access.html", view)

		return

	case errors.Is(err, disclosurebus.ErrNotEnrolled):
		view.Reason = "This computer is no longer enrolled. It cannot read its " +
			"own record, and it cannot back up either — the Status page says more."

		s.render(w, r, "access.html", view)

		return

	case err != nil:
		// Logged in full, shown as a sentence. A person reading this page
		// cannot act on a transport error, and an empty table with no
		// explanation is worse than saying the server could not be reached.
		s.cfg.Log.Warn("could not read the disclosure trail", "err", err)

		view.Reason = "This computer could not reach the server to read the " +
			"record just now. This says nothing about your backups; try again later."

		s.render(w, r, "access.html", view)

		return
	}

	view.Available = true
	view.Report = report
	view.Check = checkSentence(report)
	view.Since = report.ChainedSince

	if len(report.Machines) > 0 {
		view.MachineNewest = report.Machines[0].At
	}

	s.render(w, r, "access.html", view)
}

// vouchDate is the date the check reaches back to, without a time of day.
//
// The page's other dates carry a clock because they name an event — a backup,
// a person reading a password. This one names the start of a period, and
// "unaltered since 14 Mar 2026" is a claim somebody can hold in their head in
// a way that "unaltered since Sat 14 Mar 10:00" is not.
func vouchDate(t time.Time) string { return t.Format("2 Jan 2006") }

// checkSentence turns a verdict into the thing the page says about it.
//
// A free function taking the report, so the wording — which is the part of
// this page somebody's trust actually rests on — is testable without an HTTP
// request, a server or a database.
func checkSentence(r disclosurebus.Report) accessCheck {
	switch r.Verdict {
	case disclosurebus.VerdictAgrees:
		return accessCheck{
			Level:   "good",
			Heading: "This record has not been altered since " + vouchDate(r.Against.Seen),
			Detail: "Each entry is sealed with the one before it, and this computer " +
				"keeps its own copy of that seal every time it looks. The seal it " +
				"wrote down on " + vouchDate(r.Against.Seen) + " still matches, so no entry " +
				"recorded before then has been changed or removed.",
		}

	case disclosurebus.VerdictFirstLook:
		return accessCheck{
			Level:   "warn",
			Heading: "This is the first time this computer has looked",
			Detail: "It has just written down the seal on this record. From now on " +
				"it can tell you whether what you are shown still matches — but " +
				"today it has nothing to compare against.",
		}

	case disclosurebus.VerdictUnproven:
		return accessCheck{
			Level:   "warn",
			Heading: "This record goes back further than this computer can check",
			Detail: "The entries shown do not reach back to the oldest seal this " +
				"computer has written down, so it cannot vouch for the entries " +
				"before them. Everything shown here links up correctly.",
		}

	case disclosurebus.VerdictShrunk:
		return accessCheck{
			Level:   "bad",
			Heading: "This record is shorter than it was",
			Detail: "This computer has seen more entries in this record than it is " +
				"being shown now. Entries only ever get added, so one has been " +
				"removed. Tell whoever looks after your backups, today.",
		}

	case disclosurebus.VerdictRewritten:
		return accessCheck{
			Level:   "bad",
			Heading: "An entry in this record has been changed",
			Detail: "An entry no longer matches the seal this computer wrote down " +
				"for it when it looked before. Either that entry or one before it " +
				"has been altered. Tell whoever looks after your backups, today.",
		}

	case disclosurebus.VerdictBroken:
		return accessCheck{
			Level:   "bad",
			Heading: "This record does not hold together",
			Detail: "Each entry is supposed to be sealed with the one before it, and " +
				"these are not. Tell whoever looks after your backups, today.",
		}

	default:
		return accessCheck{
			Level:   "warn",
			Heading: "This computer cannot say whether the record is intact",
			Detail:  "It did not recognise the result of its own check, which is a fault in this program.",
		}
	}
}
