package statusapp

import (
	"context"
	"net/http"
	"time"
)

// The owner's restore card, on paper.
//
// # Why this page exists
//
// The card is the one artefact in this system that has to survive everything
// else in it: the server, the fleet, this program, the person who set it up.
// It is four values and eight lines of instructions, and it only works if it
// is printed and filed.
//
// Until now the only way to produce one was `sion-backup card`, which writes
// it to a terminal. Nobody prints a terminal. The command is right for the
// person enrolling a machine and wrong for the owner of one, and the owner is
// who the card is for -- so the status page told them to go and find somebody
// who knows what a shell is. That is how a fleet ends up with a card state of
// `never` on machines that have been backing up for a year.
//
// So the same card, rendered as a page, with a print stylesheet and a
// sentence saying which keys to press. What comes out of the printer is the
// card and nothing else: no navigation, no buttons, no advice about what to
// do next, because the page in somebody's filing cabinet is read years later
// by somebody who has never seen this program.
//
// # Why revealing it is a POST
//
// Building a card is an audited credential fetch -- the same read a backup
// makes, recorded in the trail the Access page shows. A GET that does that is
// a GET performed by a bookmark, a prefetch, a history restore and every
// accidental reload, each one a line in the owner's record saying their
// password left the server. It is also a page carrying a restic password that
// a browser will happily put back on screen when somebody hits Back.
//
// The button is the consent. GET /card explains what the card is and offers
// it; POST /card fetches the credentials, renders them once, and tells the
// fleet the card has been issued.
//
// # What is not done here
//
// The secrets are assembled by the composition root and handed over for one
// render, exactly as StartRun loads credentials outside this package. This
// app still obtains nothing itself and stores nothing: the RestoreCard lives
// in the request and dies with it.

// RestoreCard is one owner's printable card.
//
// Every field is on the paper. The three credentials are the reason the page
// is built behind a button, and the reason nothing here is ever written to
// the log, the database or a redirect.
type RestoreCard struct {
	NodeID     string
	OwnerEmail string

	RepositoryURL    string
	RepositoryBucket string

	ResticPassword   string
	RestoreKeyID     string
	RestoreKeySecret string

	// DestroyOld says the card this one replaces opens nothing any more: the
	// bucket it names has been retired and its keys deleted.
	//
	// Only ever true when an administrator has retired the old bucket, which
	// is the one moment it is safe to tell somebody to destroy the paper they
	// are holding. Before that, the old card is the only way into the only
	// bucket with any history in it.
	DestroyOld bool

	// Unrecorded says the card was built but the fleet was not told, in a
	// sentence fit to show somebody. Empty when the server was told.
	//
	// Not an error, because the card in their hands is complete and correct.
	// What it costs is the status page continuing to ask for a card that has
	// been printed, which is worth a line on the page and not a refusal.
	Unrecorded string
}

// Issued is the date on the card, which is the day it was printed.
func (c RestoreCard) Issued() string { return time.Now().Format("2 January 2006") }

// cardView is the restore card page, before and after the button.
type cardView struct {
	chrome

	// Card is the card itself, and nil until somebody has asked for one. The
	// page before that is an explanation and a button.
	Card *RestoreCard

	// Owed and Say are the server's opinion, carried over from the status
	// page so that somebody who followed the banner here sees the same
	// sentence that sent them.
	Owed bool
	Say  string

	// Problem is why there is no card to show, in the person's own words:
	// not enrolled, no read-only key pair, the server unreachable.
	Problem string
}

// card explains the card and offers it.
func (s *Server) card(w http.ResponseWriter, r *http.Request) {
	s.renderCard(w, r, cardView{})
}

// showCard builds the card and renders it once.
func (s *Server) showCard(w http.ResponseWriter, r *http.Request) {
	view := cardView{}

	if s.cfg.IssueCard == nil {
		view.Problem = "This build cannot print a card from this page."

		s.renderCard(w, r, view)

		return
	}

	// A deadline of its own rather than the request's. This talks to Eumaeus
	// twice -- the credential fetch and the record of it -- and a browser tab
	// closed mid-fetch should not leave the second half undone.
	ctx, cancel := context.WithTimeout(context.Background(), cardDeadline)
	defer cancel()

	card, err := s.cfg.IssueCard(ctx)
	if err != nil {
		// A sentence, not a stack. Every way this fails is a true fact about
		// the machine -- it is not enrolled, the server is down, Eumaeus
		// returned no read-only key -- and the composition root writes them
		// for a person to read.
		s.cfg.Log.Warn("could not build the restore card", "err", err)

		view.Problem = err.Error()

		s.renderCard(w, r, view)

		return
	}

	view.Card = &card

	// Said here rather than relied on. foundation/web sets the same header on
	// every response this server writes, and this is the one page where a
	// wrapper quietly changing would put a restic password in a disk cache --
	// so the handler that produces the secret asks for it itself.
	w.Header().Set("Cache-Control", "no-store, max-age=0")

	s.renderCard(w, r, view)
}

// cardDeadline bounds the two calls behind the button. Long enough for a
// domestic line to reach a server on the other side of an ocean, short enough
// that a hung one does not hold the page open until somebody gives up.
const cardDeadline = 2 * time.Minute

// renderCard fills in the chrome and the server's opinion.
//
// The opinion is read from the stored state the daemon's poller wrote, like
// every other page render here: a card page that makes an HTTP call to render
// is a card page that hangs when the server is down, which is one of the
// moments somebody most wants their card.
func (s *Server) renderCard(w http.ResponseWriter, r *http.Request, view cardView) {
	view.chrome = s.chromeFor("Restore card", "/card")

	if s.cfg.Credentials != nil && !s.cfg.Credentials.Enrolled() && view.Problem == "" {
		view.Problem = "This computer is not enrolled, so there is no card to print."
	}

	if state, err := s.cfg.Plan.MachineState(r.Context()); err == nil {
		view.Owed, view.Say = cardAdvice(state.Card)
	}

	s.render(w, r, "card.html", view)
}
