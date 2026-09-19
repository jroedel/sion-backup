package statusapp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// The fresh-start page: everything about moving this machine to a new bucket,
// and the two buttons that are the whole of this program's half of it.
//
// # Why it is a page of its own
//
// Because it is the one place in this program where somebody is asked to spend
// something. Every other page reports. This one says "this will take about
// four days of your connection, the computer has to stay on, and you will lose
// the ability to recover a file as it was last March" — and then asks. That
// does not belong squeezed under the run history.
//
// # What the machine decides and what the person decides
//
// The machine decides whether to raise the subject, and how loudly: the
// thresholds are in planbus and the server holds none of them. Eumaeus
// provisions storage and says what is available, and it has withdrawn every
// field that used to have an opinion about when a machine should move.
//
// The person decides when. Nothing on this page happens without a click, at
// any urgency, and that is deliberate rather than unfinished — see
// planbus.InsistAfter.

// rotationView is the fresh-start page.
type rotationView struct {
	chrome

	Configured bool
	Enrolled   bool
	Repository string

	// Offer is the local assessment: whether to raise the subject, how loudly,
	// and every reason that fired.
	Offer       planbus.Offer
	Measurement planbus.Measurement
	Integrity   planbus.Integrity
	Saving      string

	// State is what the server last said, and when. Its own age is on the page
	// because every button here acts on it: a person looking at a rotation
	// that has not moved for three weeks needs to know whether that is because
	// nobody has provisioned anything or because this machine has not been
	// able to ask.
	State planbus.MachineState

	// Estimate is how long the seeding run would take at the speed this
	// machine last measured to its own bucket, and SpeedKnown says whether
	// anybody has measured.
	Estimate   time.Duration
	Speed      float64
	SpeedKnown bool

	// Metered is whether somebody is paying for these bytes right now, and who
	// said so. The single most useful thing this page can tell a person about
	// to start a multi-day upload.
	Metered       bool
	MeteredSaidBy string

	CardOwed bool
	CardSay  string

	// Notice and Problem are what just happened, if anything did.
	Notice  string
	Problem string
}

// Owed reports whether a bucket is waiting for this machine to accept it.
func (v rotationView) Owed() bool { return v.State.Offer != nil }

// IntegrityLine is the last repository check in one sentence.
//
// A method rather than a template call, because Describe needs the clock and a
// template has no way to say what time it is.
func (v rotationView) IntegrityLine() string { return v.Integrity.Describe(time.Now()) }

// rotation renders the page.
func (s *Server) rotation(w http.ResponseWriter, r *http.Request) {
	s.renderRotation(w, r, "", "")
}

// renderRotation builds the view and writes it, with an optional outcome from
// whatever POST led here.
func (s *Server) renderRotation(w http.ResponseWriter, r *http.Request, notice, problem string) {
	ctx := r.Context()
	now := time.Now()

	view := rotationView{
		chrome:   s.chromeFor("Fresh start", "/rotation"),
		Enrolled: s.cfg.Credentials.Enrolled(),
		Notice:   notice,
		Problem:  problem,
	}

	plan, err := s.cfg.Plan.Get(ctx)

	switch {
	case errors.Is(err, planbus.ErrNoPlan):
		s.render(w, r, "rotation.html", view)

		return

	case err != nil:
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	view.Configured = true
	view.NodeID = plan.NodeID
	view.Repository = plan.Repository

	if m, err := s.cfg.Plan.Measurement(ctx, plan.Repository, now); err == nil {
		view.Measurement = m
	} else {
		s.cfg.Log.Warn("could not read the repository measurement", "err", err)
	}

	if i, err := s.cfg.Plan.Integrity(ctx, plan.Repository); err == nil {
		view.Integrity = i
	} else {
		s.cfg.Log.Warn("could not read the last repository check", "err", err)
	}

	if state, err := s.cfg.Plan.MachineState(ctx); err == nil {
		view.State = state
	} else {
		s.cfg.Log.Warn("could not read the stored machine state", "err", err)
	}

	// The server's facts when there are any, and the empty ones otherwise —
	// which is honest rather than convenient: a machine that has never managed
	// to ask does not know how old its bucket is, and planbus.Consider is
	// written to fall back to the space argument alone rather than to guess.
	//
	// Facts that name a different repository than the plan does are dropped
	// for the same reason the measurement is: they describe a bucket this
	// machine is no longer on.
	var bucket planbus.Bucket
	if view.State.Describes(plan.Repository) {
		bucket = view.State.Bucket
	}

	view.Offer = planbus.Consider(now, bucket, view.Measurement, view.Integrity)
	view.Saving = view.Offer.MonthlySaving(s.cfg.StoragePricePerTiBMonth)
	view.CardOwed, view.CardSay = cardAdvice(view.State.Card)

	if up := s.cfg.Survey.Upload(); up.Known() {
		view.SpeedKnown = true
		view.Speed = up.Result.BytesPerSecond
		view.Estimate = view.Offer.Estimate(up.Result.BytesPerSecond)
	}

	if s.cfg.Metered != nil {
		view.Metered, view.MeteredSaidBy = s.cfg.Metered(ctx)
	}

	s.render(w, r, "rotation.html", view)
}

// requestRotation asks an administrator for a fresh bucket.
//
// There is no permission to check first and this page does not pretend
// otherwise. Eumaeus used to refuse with a 403 when fresh buckets were
// switched off fleet-wide; that switch is gone, and what is left is the
// honest shape of it — ask, and an administrator either mints a bucket or does
// not. Nothing is provisioned by this click and nothing starts uploading.
func (s *Server) requestRotation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.cfg.Machine == nil || !s.cfg.Machine.Available() {
		s.renderRotation(w, r, "",
			"This computer is not enrolled, so there is nobody to ask.")

		return
	}

	plan, err := s.cfg.Plan.Get(ctx)
	if err != nil {
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	m, err := s.cfg.Plan.Measurement(ctx, plan.Repository, time.Now())
	if err != nil {
		s.fail(w, r, "reading the repository measurement", err)

		return
	}

	// The figures travel with the ask so that whoever reads the queue sees
	// what the owner was shown when they clicked, and so that a request made
	// against numbers that have since changed can be recognised as stale.
	err = s.cfg.Machine.RequestRotation(ctx, machinebus.Measured{
		RepositoryURL:    plan.Repository,
		ReclaimableBytes: m.Reclaimable(),
		FreshBytes:       m.Fresh,
		MeasuredAt:       m.MeasuredAt,
	})

	switch {
	case errors.Is(err, machinebus.ErrAlreadyAsked):
		// Not a failure and not worth showing as one. Three states reach here
		// and the third is the one to say out loud: a bucket may already be
		// waiting, in which case what is outstanding is this computer's own
		// decision, which is the button below rather than anybody's queue.
		s.refresh(ctx)
		s.renderRotation(w, r,
			"Somebody has already been asked about this computer. If a new bucket "+
				"has been set up for it, it is shown below.", "")

		return

	case errors.Is(err, machinebus.ErrNoRepository):
		s.renderRotation(w, r, "",
			"The server has no repository for this computer, so there is nothing to "+
				"replace. Whoever looks after your backups needs to fix that first.")

		return

	case err != nil:
		s.cfg.Log.Error("could not ask for a fresh bucket", "err", err)
		s.renderRotation(w, r, "", "The request could not be sent: "+err.Error())

		return
	}

	s.refresh(ctx)
	s.renderRotation(w, r,
		"Asked. Whoever looks after your backups will see this, and will either set "+
			"up a new bucket or not. Nothing on this computer changes until they do, "+
			"and nothing starts uploading until you say so here.", "")
}

// cutover accepts a bucket that has been offered.
//
// The one call in this program that commits somebody to days of their own
// connection, so it is a POST from a confirm page and never anything else.
func (s *Server) cutover(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.cfg.Machine == nil || !s.cfg.Machine.Available() {
		s.renderRotation(w, r, "", "This computer is not enrolled.")

		return
	}

	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "reading the form", err)

		return
	}

	// The URL is named back rather than assumed, and it comes from the form
	// rather than from the stored state: what the person clicked must be what
	// is sent. An offer replaced between the page being rendered and the
	// button being pressed is refused by the server with a sentence naming
	// both buckets, which is the right outcome — it sends this machine back to
	// read the current offer instead of moving it somewhere nobody was shown.
	url := r.PostFormValue("repository_url")
	if url == "" {
		s.renderRotation(w, r, "", "That form named no bucket, so nothing was accepted.")

		return
	}

	err := s.cfg.Machine.Cutover(ctx, url)

	switch {
	case errors.Is(err, machinebus.ErrNothingOffered):
		s.refresh(ctx)
		s.renderRotation(w, r, "",
			"There is no bucket waiting for this computer any more. Nothing has changed.")

		return

	case err != nil:
		// A 422 arrives carrying the server's own sentence, which names both
		// buckets; it is shown as it stands, because the difference is usually
		// one word of a bucket name and nothing here can say it better.
		var refused *eumaeusapi.BadRequest

		if errors.As(err, &refused) && refused.Message != "" {
			s.refresh(ctx)
			s.renderRotation(w, r, "", refused.Message)

			return
		}

		s.cfg.Log.Error("could not accept the offered bucket", "err", err)
		s.renderRotation(w, r, "", "The new bucket could not be accepted: "+err.Error())

		return
	}

	s.cfg.Log.Info("this computer accepted a new bucket", "repository", url)

	s.refresh(ctx)
	s.renderRotation(w, r,
		"Accepted. The next backup fills the new bucket from scratch and will take "+
			"much longer than usual — leave this computer on and awake while it runs. "+
			"Your old backups stay readable the whole time, and nothing is deleted "+
			"until a person retires the old bucket.", "")
}

// refresh asks the server for this machine's state again, so the page the
// person is about to see reflects what they just did.
//
// Best effort. The stored answer is refreshed by the daemon's poller anyway;
// this only saves somebody from looking at a page that still shows the offer
// they have just accepted.
func (s *Server) refresh(ctx context.Context) {
	if s.cfg.RefreshState == nil {
		return
	}

	s.cfg.RefreshState(ctx)
}

// cardAdvice turns the server's card state into what to tell the owner.
//
// The state is the signal and its date is not. `never` means nobody has
// printed a card for the bucket this machine writes to now, which is what a
// machine reads immediately after a cutover. `superseded` arrives later, when
// an administrator retires the old bucket, and is the only moment at which it
// is safe to say "destroy the old one" — until then the old card is the only
// way into the only bucket holding any history.
func cardAdvice(c planbus.Card) (owed bool, say string) {
	switch c.State {
	case planbus.CardNever:
		return true, "Nobody has printed the page that lets this computer's files be " +
			"restored without us. Ask whoever set this up to run “sion-backup card”."

	case planbus.CardSuperseded:
		return true, "The printed page you are holding no longer opens anything — the " +
			"bucket it names has been retired. Ask for a new one with “sion-backup " +
			"card”, and destroy the old page."
	}

	return false, ""
}
