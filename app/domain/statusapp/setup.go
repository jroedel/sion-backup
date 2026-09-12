package statusapp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/survey/surveybus"
)

// The setup page is the one a person sees once, the moment their computer is
// enrolled, and it is the only page in this program that somebody who did not
// ask for it will read.
//
// # What it is for
//
// Enrolment used to end with a machine that had credentials and a plan
// somebody else had written, and it would start uploading at the next
// scheduled slot. That is wrong in two directions at once. A first backup is
// the largest thing this program ever does — tens of gigabytes, hours of
// somebody's uplink — and it was being started without the person whose
// computer it is having seen what was in it or having been told how long it
// would take. And what was in it was a guess: an administrator's idea of which
// folders matter, made at a desk that is not theirs.
//
// So enrolment now stops, and the scheduler holds, until this page has been
// answered. See planbus.Plan.ConfirmedAt for the gate itself.
//
// # Why it is one page and not a wizard
//
// Because every question on it changes the answer to another one. Turning the
// junk excludes on changes the size; the size and the measured upload rate
// together are the only number anybody actually wants, which is how long this
// is going to take. A four-step wizard would show those four facts on four
// pages and let somebody finish without ever seeing them together.
//
// It is also a page with no JavaScript, like the rest of this server, so
// "changes another answer" means a form post and a re-render. Hence two
// buttons: one that saves and stays, and one that saves and starts.

// setupView is the whole page.
type setupView struct {
	chrome

	// Ready reports a machine that can be set up at all: enrolled, with a
	// repository to write to. When it is false the page says what is missing
	// and offers nothing else.
	Ready   bool
	Missing string

	Plan    planbus.Plan
	Options []styleOption

	// The form's own state, which is not the plan's: it is what the boxes
	// currently hold, including on a re-render after something was refused.
	Style   string
	Targets string
	Junk    bool

	// Excludes are the patterns that are not the junk list: whatever was in
	// the plan before this page was opened. There is no box for them here —
	// the Settings page owns that — and they are shown because a page that
	// silently carries somebody's exclusions along reads exactly like a page
	// that has dropped them.
	Excludes []string

	SkipLargerThanGB int
	Schedule         string
	DailyTime        string
	Times            string
	SkipOnMetered    bool

	// MeteredKnown reports whether this platform can actually tell. On
	// Windows and macOS it cannot, and the page says so beside the checkbox
	// rather than offering a protection the machine will not provide.
	MeteredKnown bool

	Upload surveybus.Upload

	// Chosen is the option the radio is currently on, so the summary at the
	// bottom can talk about a size and a time rather than about all of them.
	Chosen  styleOption
	HasSize bool

	// CustomSizing is the measured size of the list somebody wrote
	// themselves, which is not one of Options and still has to carry a figure
	// — on a machine adopt-enroll set up, it is the only answer on the page.
	CustomSizing   surveybus.Sizing
	CustomEstimate time.Duration

	Saved   bool
	Problem string
}

// styleOption is one backup style as the page shows it.
type styleOption struct {
	Style  string
	Title  string
	Detail string
	Roots  []string

	Sizing surveybus.Sizing

	// Estimate is how long a first backup of this much would take at the
	// measured rate. Zero when nothing has been measured yet.
	Estimate time.Duration
}

// Measuring reports whether anything on the page is still moving.
func (v setupView) Measuring() bool {
	if v.Upload.Measuring() {
		return true
	}

	if v.CustomSizing.Measuring() {
		return true
	}

	for _, o := range v.Options {
		if o.Sizing.Measuring() {
			return true
		}
	}

	return false
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	plan, err := s.cfg.Plan.Get(r.Context())

	switch {
	// Enrolment is checked as well as the repository, because a machine can
	// have one without the other: a repository URL written into config.toml
	// by hand, and no token to fetch a credential with. Such a machine cannot
	// back up — credentialbus refuses before restic is ever started — and
	// before this it was shown the whole page anyway. It counted the folders,
	// failed the speed test with "this machine is not enrolled" in the small
	// print at the bottom, and still offered a button that said "Start backing
	// up".
	case errors.Is(err, planbus.ErrNoPlan),
		err == nil && plan.Repository == "",
		err == nil && !s.cfg.Credentials.Enrolled():
		s.render(w, r, "setup.html", setupView{
			chrome: s.chromeFor("Set up backups", "/setup"),
			// Both commands are named because both lead here. "adopt-enroll"
			// without a code is a deliberate half-step — it writes the plan
			// read out of the backup already running, and stops so the bucket
			// can be adopted in Eumaeus before anybody claims a code — and it
			// leaves exactly this machine: a plan, a repository, and no token.
			// Telling somebody standing in the middle of that to run "enroll"
			// would send them to the wrong command.
			Missing: "This computer has not been enrolled yet, so it has no credentials " +
				"and nothing can be backed up. Whoever is installing it needs a code " +
				"from Eumaeus, and then “sion-backup enroll --code …” — or “sion-backup " +
				"adopt-enroll --code …” on a machine that is already backing up with " +
				"the old script. This page is what they are sent to afterwards.",
		})

		return

	case err != nil:
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	view := s.setupViewFrom(plan)

	// Measuring is started from the render, not from a button, so that the
	// numbers are already arriving by the time somebody has read the first
	// paragraph. Both calls are cheap when there is nothing to do, and both
	// take the daemon's context rather than this request's: a walk of a home
	// directory must not be abandoned because a page was refreshed.
	s.cfg.Survey.Measure(s.cfg.Background, plan.Targets,
		effectiveExcludes(plan, view.Junk), view.SkipLargerThanGB)
	s.cfg.Survey.ProbeOnce(s.cfg.Background)

	view = s.setupViewFrom(plan)
	view.Saved = r.URL.Query().Has("saved")

	// Only on a GET, and that is the point: this page has text boxes in it,
	// and a meta refresh while somebody is typing into one throws away what
	// they typed. Every GET renders the boxes from what is stored, so a refresh
	// costs nothing — including the GET a save redirects to. The one render
	// that carries typing nobody has stored yet is the one that comes back
	// with a problem on it, and saveSetup does not set this.
	if view.Measuring() {
		view.Refresh = 3
	}

	s.render(w, r, "setup.html", view)
}

// setupViewFrom builds the page from the stored plan, filling in the defaults
// a machine that has just been enrolled needs.
func (s *Server) setupViewFrom(plan planbus.Plan) setupView {
	view := setupView{
		chrome:           s.chromeFor("Set up backups", "/setup"),
		Ready:            true,
		Plan:             plan,
		Style:            plan.Style,
		Targets:          strings.Join(plan.Targets, "\n"),
		SkipLargerThanGB: plan.SkipLargerThanGB,
		SkipOnMetered:    plan.SkipOnMetered,
		MeteredKnown:     s.cfg.MeteredKnown,
		Junk:             surveybus.HasJunk(plan.Excludes),
		Excludes:         beyondTheCheckbox(plan.Excludes),
		Schedule:         string(plan.Schedule.Preset()),
		DailyTime:        plan.Schedule.DailyTime(),
		Times:            strings.Join(plan.Schedule.Times, ", "),
		Upload:           s.cfg.Survey.Upload(),
	}

	view.NodeID = plan.NodeID
	view.CustomSizing = s.cfg.Survey.Sizing(surveybus.StyleCustom)
	view.CustomEstimate = view.Upload.Estimate(view.CustomSizing.Result.Bytes)

	for _, c := range s.cfg.Survey.Choices() {
		o := styleOption{
			Style:  string(c.Style),
			Title:  c.Title,
			Detail: c.Detail,
			Roots:  c.Roots,
			Sizing: s.cfg.Survey.Sizing(c.Style),
		}

		o.Estimate = view.Upload.Estimate(o.Sizing.Result.Bytes)

		view.Options = append(view.Options, o)
	}

	// A plan with no style recorded on it, which is three different machines:
	// one enrolled a minute ago, one adopt-enroll read a folder list out of a
	// legacy script for, and — the one this was got wrong for — every machine
	// in the fleet that upgraded into this page. Those last have a plan they
	// have been backing up with for months and no style beside it, because the
	// field did not exist when it was written.
	//
	// All three need the radio put where the stored plan actually is. The
	// alternative is what this did: offer the first choice on the page while
	// somebody's real folder list sat unselected in the box below it, so that
	// one press of "Start backing up" quietly replaced the second with the
	// first. Whether the plan is confirmed has nothing to do with it — that
	// only says somebody has answered this page before, and the migration says
	// yes for every upgraded machine so it would keep backing up.
	if plan.Style == "" {
		switch style := s.cfg.Survey.StyleOf(plan.Targets); {
		case style != "":
			view.Style = string(style)
		case len(view.Options) > 0:
			view.Style = view.Options[0].Style
		}
	}

	// The junk list is offered to a machine nobody has answered for, and never
	// added to a plan that is already running: somebody who has been backing
	// up their whole home directory for a year did not ask, on the day they
	// upgraded, to start leaving parts of it out.
	if !plan.Confirmed() && plan.Style == "" {
		view.Junk = true
	}

	if view.Schedule == string(planbus.PresetDaily) && view.DailyTime == "" {
		view.DailyTime = "13:00"
	}

	for _, o := range view.Options {
		if o.Style == view.Style {
			view.Chosen, view.HasSize = o, o.Sizing.Known()
		}
	}

	if view.Style == string(surveybus.StyleCustom) {
		view.Chosen = styleOption{
			Style:    string(surveybus.StyleCustom),
			Sizing:   view.CustomSizing,
			Estimate: view.CustomEstimate,
		}

		view.HasSize = view.CustomSizing.Known()
	}

	return view
}

// effectiveExcludes is the exclude list the chosen settings would produce.
//
// Computed from the form's state rather than read off the plan, because the
// sizes have to reflect the checkbox as it is now: somebody who has just
// ticked "leave out the usual junk" and pressed Update is asking what that
// did, and answering with the old number would make the checkbox look broken.
//
// The first case is the one that matters and it is not an optimisation. The
// checkbox is all-or-nothing — [surveybus.HasJunk] reports it ticked only when
// the whole set is present — but [surveybus.WithoutJunk] removes any member of
// that set it finds. So a plan carrying one pattern that happens to be on the
// list, "*.iso" say, written by hand years ago, showed the box unticked and
// then deleted the pattern the first time anything on this page was saved.
// Nothing on the screen changed and an exclusion was gone.
//
// So: the list only moves when the checkbox does.
func effectiveExcludes(plan planbus.Plan, junk bool) []string {
	if junk == surveybus.HasJunk(plan.Excludes) {
		return plan.Excludes
	}

	if junk {
		return append(surveybus.WithoutJunk(plan.Excludes), surveybus.JunkExcludes()...)
	}

	return surveybus.WithoutJunk(plan.Excludes)
}

// beyondTheCheckbox is what the page shows under the junk box: the exclusions
// this machine has that the checkbox does not account for.
//
// When the box is ticked those are whatever is left after the junk set; when
// it is not, they are all of them — including a pattern that coincides with
// one on the junk list, which is not being left out by the checkbox and would
// be a strange thing to hide from somebody on the grounds that it might have
// been.
func beyondTheCheckbox(excludes []string) []string {
	if surveybus.HasJunk(excludes) {
		return surveybus.WithoutJunk(excludes)
	}

	return excludes
}

func (s *Server) saveSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	plan, err := s.cfg.Plan.Get(ctx)
	if err != nil {
		// Including ErrNoPlan, which means this arrived at a machine that has
		// not been enrolled. The page offers no form in that state, so getting
		// here means a hand-made request — and the honest answer to it is the
		// same sentence the page shows, not a 500.
		s.setup(w, r)

		return
	}

	confirm := r.PostFormValue("action") == "confirm"

	plan, problem := s.applySetup(plan, r)
	if problem == "" {
		problem = s.store(ctx, plan, confirm)
	}

	if problem != "" {
		// Re-rendered with what they chose still in the boxes, rather than
		// redirected: losing a folder list to a validation error is how
		// somebody decides the page is not worth using.
		view := s.setupViewFrom(plan)
		view.Problem = problem

		s.render(w, r, "setup.html", view)

		return
	}

	if confirm {
		http.Redirect(w, r, "/", http.StatusSeeOther)

		return
	}

	http.Redirect(w, r, "/setup?saved", http.StatusSeeOther)
}

// store writes the plan, and confirms it when that is what was asked.
//
// Two calls rather than one field, because they are two different acts and
// planbus is deliberate about the difference: Put is "here is a plan", Confirm
// is "somebody at this machine said yes to it". Only this handler, reached
// from a button on this page, is allowed to do the second.
func (s *Server) store(ctx context.Context, plan planbus.Plan, confirm bool) string {
	now := time.Now()

	if err := s.cfg.Plan.Put(ctx, plan, now); err != nil {
		return err.Error()
	}

	if !confirm {
		return ""
	}

	if err := s.cfg.Plan.Confirm(ctx, now); err != nil {
		return err.Error()
	}

	s.cfg.Log.Info("the backup plan was confirmed on the setup page",
		"node", plan.NodeID, "style", plan.Style, "targets", len(plan.Targets),
		"schedule", plan.Schedule.Times)

	// Started at once rather than left to the next scheduled slot. Somebody
	// has just pressed a button that says "start backing up", and a machine
	// that then does nothing until one in the afternoon has not done what the
	// button said. It also puts the first run in front of the person who chose
	// it, which is where a failure is cheapest to find.
	if err := s.cfg.StartRun(ctx); err != nil {
		s.cfg.Log.Warn("the first backup did not start", "err", err)
	}

	return ""
}

// applySetup folds the form into the plan, and reports the first thing on it
// that cannot be used.
func (s *Server) applySetup(plan planbus.Plan, r *http.Request) (planbus.Plan, string) {
	style := r.PostFormValue("style")

	switch style {
	case string(surveybus.StyleCustom):
		plan.Targets = lines(r.PostFormValue("targets"))

		if len(plan.Targets) == 0 {
			return plan, "No folders were listed, so there would be nothing to back up. " +
				"Choose one of the options above, or write the folders one per line."
		}

	default:
		found := false

		for _, c := range s.cfg.Survey.Choices() {
			if string(c.Style) == style {
				plan.Targets, found = c.Roots, true
			}
		}

		if !found {
			return plan, "That is not one of the choices on this page. Pick one, or " +
				"choose the folders yourself."
		}
	}

	plan.Style = style
	plan.Excludes = effectiveExcludes(plan, r.PostFormValue("junk") == "on")
	plan.SkipOnMetered = r.PostFormValue("skip_on_metered") == "on"

	switch n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("skip_larger_than_gb"))); {
	case r.PostFormValue("skip_larger_than_gb") == "":
		plan.SkipLargerThanGB = 0

	case err != nil || n < 0:
		return plan, "The size limit has to be a whole number of gigabytes, or empty for no limit."

	default:
		plan.SkipLargerThanGB = n
	}

	schedule, problem := schedule(plan.Schedule, r)
	if problem != "" {
		return plan, problem
	}

	plan.Schedule = schedule

	return plan, ""
}

// schedule turns the "how often" answer into times.
func schedule(current planbus.Schedule, r *http.Request) (planbus.Schedule, string) {
	switch planbus.Preset(r.PostFormValue("schedule")) {
	case planbus.PresetHourly:
		return planbus.Hourly(), ""

	case planbus.PresetThriceDaily:
		return planbus.ThriceDaily(), ""

	case planbus.PresetDaily:
		at := strings.TrimSpace(r.PostFormValue("daily_time"))
		if at == "" {
			return current, "Choose a time of day for the backup to run at."
		}

		return planbus.DailyAt(at), ""

	default:
		times := splitTimes(r.PostFormValue("times"))
		if len(times) == 0 {
			return current, "Write at least one time of day, as HH:MM."
		}

		// The jitter and the floor are kept from whatever the plan had, so
		// that somebody writing their own times does not silently lose the
		// spread that keeps forty machines off one bucket at once.
		if current.JitterMinutes == 0 && current.MinInterval == 0 {
			current = planbus.DefaultSchedule()
		}

		current.Times = times

		return current, ""
	}
}

// retestSpeed measures the connection again, on request.
//
// A button rather than something the page does on every render, because each
// test sends real megabytes up somebody's connection. It is here because the
// first measurement is taken on whatever the connection happened to be doing
// at the time — a video call, a colleague's upload, a hotel — and the honest
// response to "that cannot be right" is to let them check.
func (s *Server) retestSpeed(w http.ResponseWriter, r *http.Request) {
	s.cfg.Survey.Probe(s.cfg.Background)

	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// remeasure counts the folders again, on request.
func (s *Server) remeasure(w http.ResponseWriter, r *http.Request) {
	plan, err := s.cfg.Plan.Get(r.Context())
	if err != nil && !errors.Is(err, planbus.ErrNoPlan) {
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	s.cfg.Survey.Remeasure()
	s.cfg.Survey.Measure(s.cfg.Background, plan.Targets,
		effectiveExcludes(plan, surveybus.HasJunk(plan.Excludes)), plan.SkipLargerThanGB)

	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}
