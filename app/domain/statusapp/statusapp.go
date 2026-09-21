// Package statusapp is the page a person opens to find out whether their
// computer is backing up.
//
// # Why there is a web page on a backup client at all
//
// Because the failure this program is built to prevent is not "the backup
// broke". It is "the backup broke in March and nobody noticed until the laptop
// was stolen in September". Every part of the design points at that: the run
// history rather than a last-success flag, the verification round trip rather
// than an exit code, and this page, which says one sentence in large type at
// the top and puts the evidence underneath it.
//
// It is also where the person using the machine changes what is backed up.
// That belongs to them, not to the administrator: they are the only one who
// knows that the work they care about now lives in a folder that did not exist
// when the machine was set up.
//
// # No JavaScript
//
// Server-rendered, and the only dynamic behaviour is a meta refresh while a
// backup is running. This is not asceticism. The Content-Security-Policy in
// foundation/web forbids scripts entirely, and that policy is worth more here
// than a live progress bar: this page displays filenames read off somebody's
// disk, and a page that cannot execute anything cannot be turned into a
// exfiltration tool by a cleverly named file.
package statusapp

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/app/sdk/loopback"
	"github.com/jroedel/sion-backup/app/sdk/page"
	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/survey/surveybus"
	"github.com/jroedel/sion-backup/foundation/paths"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Config wires the app.
type Config struct {
	// Plan is the backup plan domain.
	Plan *planbus.Business

	// Backups is the run history and the running-state.
	Backups *backupbus.Runner

	// Credentials is consulted only for the sentence it can print about how
	// credentials are handled. This app never obtains one.
	Credentials *credentialbus.Business

	// Disclosures is the record of every time this machine's repository
	// password left the server, and this machine's own check that the record
	// has not been rewritten since it last looked.
	//
	// Nil on a machine that is not enrolled, and on one talking to a Eumaeus
	// that does not serve the record yet. The page says which; it does not
	// disappear, because a link that is sometimes there and sometimes not is
	// a link nobody learns to look for.
	Disclosures *disclosurebus.Business

	// Survey is what this machine could back up, how big it is, and how fast
	// it can upload. The setup page is built out of it.
	Survey *surveybus.Business

	// Background is the daemon's context, for work a request starts and does
	// not wait for: counting a home directory, measuring the connection.
	//
	// A request's own context is the wrong one and the bug is silent. A walk
	// tied to it is cancelled the moment the page it was started from
	// finishes rendering, and what the next refresh sees is a measurement that
	// has mysteriously restarted from zero.
	Background context.Context

	// MeteredKnown reports whether this platform can tell a metered
	// connection from an unmetered one. See foundation/netcost: Linux can,
	// Windows and macOS cannot yet. The setup page says so beside the
	// checkbox rather than offering a protection the machine will not give.
	MeteredKnown bool

	// Machine is what the server says about this machine, and the four calls
	// this machine makes about its own repository.
	//
	// Nil on a machine that is not enrolled. Only the POST handlers use it: a
	// page render reads the stored answer the daemon's poller wrote, because a
	// status page that makes an HTTP call to render is a status page that
	// hangs when the server is down — which is exactly when somebody opens it.
	Machine *machinebus.Business

	// RefreshState asks the server about this machine again and stores the
	// answer.
	//
	// A closure because the conversion between what the server says and what
	// is stored belongs to the composition root, and because nothing that
	// renders HTML should be able to decide when this program talks to
	// Eumaeus. Called only after a button has been pressed, so the page that
	// follows shows what the person just did rather than the poll from four
	// hours ago.
	RefreshState func(context.Context)

	// Metered reports whether somebody is paying for these bytes right now,
	// and who said so.
	//
	// The single most useful thing the fresh-start page can tell a person
	// about to commit to a multi-day upload. A closure for the same reason
	// StartRun is one: it asks the operating system, which is not this
	// package's business.
	Metered func(context.Context) (bool, string)

	// StartRun begins a backup now.
	//
	// A function rather than the backup domain itself, because starting a run
	// means loading credentials, and the composition root is the only place
	// that does that. Handing this app a closure keeps every secret out of the
	// package that renders HTML.
	StartRun func(context.Context) error

	// UpdateSource describes where updates come from and which versions this
	// machine has given up on. Empty source means none come from anywhere,
	// which is a configuration and not a fault.
	UpdateSource func() (source string, refused []string)

	// CheckForUpdate installs a newer build if there is one, now.
	//
	// Now, and not "on the next schedule": the automatic path throttles
	// itself to one check an hour, and somebody who pressed a button means
	// this minute. See cmd/sion-backup/update.go, where the command-line form
	// clears the same throttle for the same reason.
	CheckForUpdate func(context.Context) (UpdateOutcome, error)

	// IssueCard assembles the owner's restore card.
	//
	// It does not record anything: rendering a card is not printing one, and
	// CardPrinted is what says a card exists. See app/domain/statusapp/card.go.
	//
	// A closure for the reason StartRun is one: building a card means one
	// audited credential fetch, and the composition root is the only place
	// that loads a secret. What comes back is a value for a single render --
	// this package does not store it, log it, or put it anywhere a redirect
	// could carry it.
	//
	// Nil in a build that cannot print one, and the page says so rather than
	// offering a button that can only apologise.
	IssueCard func(context.Context) (RestoreCard, error)

	// CardPrinted tells the fleet that somebody is holding a printed card for
	// the repository they name.
	//
	// The repository travels back from the page the card was shown on, so
	// that what is recorded is a fact about that card rather than about
	// wherever this machine happens to be writing when the button is pressed.
	//
	// Nil in a build that cannot tell anybody, and the page says so rather
	// than silently doing nothing.
	CardPrinted func(ctx context.Context, repositoryURL string) error

	// Restart schedules the exit that this machine's service manager turns
	// into a start, and reports how long that is expected to take.
	//
	// It returns as soon as the exit is booked rather than performing it, so
	// that the page saying "your daemon is coming back" is written by a
	// process that still exists. Refusals come back as errors carrying a
	// sentence: a backup in progress, or nothing that would start it again.
	Restart func(context.Context) (time.Duration, error)

	// Guard supplies the form token.
	Guard *loopback.Guard

	// Paths is shown in the footer, so somebody looking for their data does
	// not have to guess.
	Paths paths.Paths

	// Version is this build.
	Version string

	// Executable is the file this program is running from, shown in the
	// footer beside the data directory. Empty leaves it out, which is what a
	// machine that cannot work out its own path gets -- a footer short one
	// fact rather than a page that will not render.
	Executable string

	// StoragePricePerTiBMonth turns reclaimable bytes into money on the
	// rotation card. Zero keeps the card talking in gigabytes, which is the
	// right thing to do when nobody has said what a gigabyte costs.
	StoragePricePerTiBMonth float64

	Log *slog.Logger
}

// Server serves the status page.
type Server struct {
	cfg     Config
	pages   *template.Template
	handler http.Handler
}

// New parses the templates and builds the route tree.
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		return nil, errors.New("statusapp: no logger")
	}

	if cfg.Guard == nil {
		return nil, errors.New("statusapp: no loopback guard; the page must not be served without one")
	}

	if cfg.Survey == nil {
		return nil, errors.New("statusapp: no survey; the setup page cannot be built without one")
	}

	if cfg.Background == nil {
		return nil, errors.New("statusapp: no background context for the work a request starts")
	}

	pages, err := template.New("layout.html").
		Funcs(funcs).
		ParseFS(page.FS, "templates/chrome.html")
	if err != nil {
		return nil, fmt.Errorf("statusapp: parsing the shared chrome: %w", err)
	}

	pages, err = pages.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("statusapp: parsing the pages: %w", err)
	}

	s := &Server{cfg: cfg, pages: pages}

	mux := http.NewServeMux()
	mux.Handle("GET "+page.StylePath, page.Style())
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /{$}", s.status)
	mux.HandleFunc("GET /access", s.access)
	mux.HandleFunc("GET /card", s.card)
	mux.HandleFunc("POST /card", s.showCard)
	mux.HandleFunc("POST /card/printed", s.cardPrinted)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("POST /settings/update", s.checkForUpdate)
	mux.HandleFunc("POST /settings/restart", s.restart)
	mux.HandleFunc("GET /setup", s.setup)
	mux.HandleFunc("POST /setup", s.saveSetup)
	mux.HandleFunc("POST /setup/speed", s.retestSpeed)
	mux.HandleFunc("POST /setup/measure", s.remeasure)
	mux.HandleFunc("GET /rotation", s.rotation)
	mux.HandleFunc("POST /rotation/request", s.requestRotation)
	mux.HandleFunc("POST /rotation/cutover", s.cutover)
	mux.HandleFunc("POST /run", s.runNow)

	s.handler = mux

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// health answers for the service manager's restart logic. It reads the
// database, because a process that is up but cannot read its own history is
// not healthy in any sense the administrator cares about.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if _, err := s.cfg.Backups.Recent(r.Context(), 1); err != nil {
		http.Error(w, "not ok", http.StatusServiceUnavailable)

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// chrome is what every page needs.
type chrome struct {
	Title   string
	Nav     []page.Link
	NodeID  string
	Version string
	DataDir string
	Exe     string
	Token   string

	// Refresh is the meta-refresh interval in seconds, or 0. Set only while a
	// backup is running: a page that reloads itself every five seconds forever
	// is a page that keeps a laptop's screen awake.
	Refresh int
}

// statusView is the front page.
type statusView struct {
	chrome

	Configured bool
	Confirmed  bool
	Paused     bool
	Verdict    verdict

	// Notice is what just happened, when somebody pressed a button. Empty on
	// an ordinary page load.
	Notice string

	Running     bool
	Progress    backupbus.Progress
	Last        backupbus.Run
	HasLast     bool
	NextRun     time.Time
	Recent      []backupbus.Run
	Credentials string
	Enrolled    bool

	// Glance is the one line under the credentials fact, read from what this
	// machine has written down rather than from the server. See the Access
	// page for the record itself.
	Glance     disclosurebus.Glance
	Repository string
	Targets    []string

	// Rotation is the "a fresh start would reclaim this much" assessment, and
	// the measurement behind it. Rotation.Show decides whether it appears.
	//
	// The front page carries the banner and the whole argument is on
	// /rotation. Two reasons: the argument is long — three kinds of reason,
	// two numbers and a time estimate — and the page somebody opens at nine in
	// the evening to find out whether their laptop is backed up should answer
	// that question first.
	Rotation    planbus.Offer
	Measurement planbus.Measurement
	Saving      string

	// Offered is a bucket waiting for this computer to accept it, or nil. It
	// appears on the front page whatever the assessment says, because
	// somebody has done work and is waiting for an answer.
	Offered *planbus.Offered

	// CuttingOver is a move already under way: the new bucket is being filled
	// and the old one is still readable.
	CuttingOver bool

	// CardOwed and CardSay are the owner's printed restore page.
	CardOwed bool
	CardSay  string
}

// verdict is the sentence at the top of the page and the colour behind it.
type verdict struct {
	Level   string // good | warn | bad
	Heading string
	Detail  string
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	view := statusView{
		chrome:      s.chromeFor("Status", "/"),
		Credentials: s.cfg.Credentials.Describe(),
		Enrolled:    s.cfg.Credentials.Enrolled(),
	}

	if s.cfg.Disclosures != nil {
		// Best effort, and deliberately not fatal: a fault in reading a
		// footnote must not take down the page that says whether this
		// computer is backing up.
		if glance, err := s.cfg.Disclosures.Glance(ctx); err == nil {
			view.Glance = glance
		} else {
			s.cfg.Log.Warn("could not read the kept disclosure heads", "err", err)
		}
	}

	plan, err := s.cfg.Plan.Get(ctx)

	switch {
	case errors.Is(err, planbus.ErrNoPlan):
		view.Verdict = verdict{
			Level:   "bad",
			Heading: "This computer is not backing up",
			Detail: "It has not been set up yet. Whoever installed it needs to run " +
				"“sion-backup enroll”.",
		}

		s.render(w, r, "status.html", view)

		return

	case err != nil:
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	view.Configured = true
	view.Confirmed = plan.Confirmed()
	view.Paused = plan.Paused
	view.NodeID = plan.NodeID
	view.Repository = plan.Repository
	view.Targets = plan.Targets

	progress, running := s.cfg.Backups.Running()
	view.Running, view.Progress = running, progress

	if running {
		view.Refresh = 5
	}

	// What the button just did.
	//
	// "started" is the gap this exists for. Starting a run means fetching
	// credentials over the network before anything registers as running, so
	// the redirect from the button lands here while Running is still false --
	// and the page that came back was byte-for-byte the one already on the
	// screen. No progress bar, no refresh, nothing to say a press had
	// registered, on the one page whose whole job is to say what is
	// happening. The notice says it, and a short refresh carries the page
	// across the gap to the progress bar.
	switch {
	case r.URL.Query().Has("started") && !running:
		view.Notice = "Starting. This page will follow along in a moment."

		view.Refresh = 2

	case r.URL.Query().Has("started"):
		view.Notice = "Started."

	case r.URL.Query().Has("running"):
		view.Notice = "A backup is already running, so this did not start another. " +
			"It is shown above."
	}

	// The last run with an outcome, never the row a run in progress is still
	// writing into. That row holds OutcomeFailed as its placeholder, and this
	// block used to render it: "failed, 0 files, 0 B, verified no", under a
	// banner saying a backup was running.
	last, err := s.cfg.Backups.LastFinished(ctx)

	switch {
	case err == nil:
		view.Last, view.HasLast = last, true
	case !errors.Is(err, backupbus.ErrNoRuns):
		s.fail(w, r, "reading the run history", err)

		return
	}

	if recent, err := s.cfg.Backups.Recent(ctx, 20); err == nil {
		view.Recent = recent
	} else {
		s.cfg.Log.Warn("could not read the run history", "err", err)
	}

	now := time.Now()

	if m, err := s.cfg.Plan.Measurement(ctx, plan.Repository, now); err == nil {
		view.Measurement = m
	} else {
		s.cfg.Log.Warn("could not read the repository measurement", "err", err)
	}

	var integrity planbus.Integrity
	if i, err := s.cfg.Plan.Integrity(ctx, plan.Repository); err == nil {
		integrity = i
	}

	// The stored answer the poller wrote, never a call from here. Facts naming
	// a different repository than the plan does are dropped, by the same rule
	// the measurement follows: they describe a bucket this machine has left.
	var bucket planbus.Bucket

	if state, err := s.cfg.Plan.MachineState(ctx); err == nil {
		view.Offered = state.Offer
		view.CardOwed, view.CardSay = cardAdvice(state.Card)

		if state.Describes(plan.Repository) {
			bucket = state.Bucket
			view.CuttingOver = state.Bucket.CuttingOver()
		}
	} else {
		s.cfg.Log.Warn("could not read the stored machine state", "err", err)
	}

	view.Rotation = planbus.Consider(now, bucket, view.Measurement, integrity)
	view.Saving = view.Rotation.MonthlySaving(s.cfg.StoragePricePerTiBMonth)

	view.NextRun = plan.Schedule.Next(plan.NodeID, time.Now())
	view.Verdict = verdictFor(plan, view.Last, view.HasLast, running, time.Now())

	s.render(w, r, "status.html", view)
}

// staleAfter is how long a machine may go without a good backup before the
// page stops calling it healthy.
//
// Two days rather than one. A daily schedule plus a weekend, a public holiday,
// or a day working from a café means one missed night is normal, and a page
// that cries wolf on a Monday morning is a page people stop reading — which is
// the actual failure this whole program is designed against.
const staleAfter = 48 * time.Hour

// verdictFor turns the state of the machine into one sentence.
//
// It is a free function taking everything it needs, so the wording — which is
// the part of this program a person actually reads — is testable without an
// HTTP request or a database.
func verdictFor(plan planbus.Plan, last backupbus.Run, hasLast, running bool, now time.Time) verdict {
	if running {
		return verdict{"good", "Backing up now", "This page refreshes itself while it runs."}
	}

	// Before the pause check and before the history: a machine nobody has set
	// up has no history to be stale, and saying "backups have stopped" about
	// one that has not started yet sends somebody looking for a fault. See
	// planbus.Plan.ConfirmedAt for why it waits at all.
	if !plan.Confirmed() {
		return verdict{"warn", "Waiting for you to say what to back up",
			"This computer is enrolled and ready. It is deliberately not backing " +
				"anything up until somebody using it has chosen what should be in it."}
	}

	if plan.Paused {
		return verdict{"warn", "Backups are paused",
			"Nothing is being backed up. Turn them back on in Settings."}
	}

	if !hasLast {
		return verdict{"warn", "No backup has run yet",
			"The first one will start at the next scheduled time, or press “Back up now”."}
	}

	age := now.Sub(last.StartedAt)

	switch last.Outcome {
	case backupbus.OutcomeSuccess:
		if age > staleAfter {
			return verdict{"bad", "Backups have stopped",
				fmt.Sprintf("The last good backup was %s ago, and nothing has run since.",
					humanDuration(age))}
		}

		return verdict{"good", "This computer is backed up",
			fmt.Sprintf("Last verified %s ago.", humanDuration(age))}

	case backupbus.OutcomeDegraded:
		return verdict{"warn", "Backed up, with a gap",
			"Files that were open at the time may be missing. " + last.Message}

	case backupbus.OutcomeIncomplete:
		return verdict{"warn", "Backed up, but some files were missed",
			last.Message + " They are listed below."}

	case backupbus.OutcomeUnverified:
		return verdict{"bad", "The backup could not be read back",
			"A snapshot was written but nothing came out of it. " + last.Message}

	default:
		return verdict{"bad", "The last backup failed", last.Message}
	}
}

// settingsView is the configuration form.
type settingsView struct {
	chrome

	Plan     planbus.Plan
	Targets  string
	Excludes string
	Times    string
	Saved    bool
	Problem  string

	// MeteredKnown reports whether this platform can tell. See Config.
	MeteredKnown bool

	// Program is the section about the software rather than the plan.
	Program programView
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	plan, err := s.cfg.Plan.Get(r.Context())
	if err != nil && !errors.Is(err, planbus.ErrNoPlan) {
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	if errors.Is(err, planbus.ErrNoPlan) {
		plan.Schedule = planbus.DefaultSchedule()
	}

	s.renderSettings(w, r, plan, r.URL.Query().Has("saved"), "")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request,
	plan planbus.Plan, saved bool, problem string) {

	s.renderSettingsWith(w, r, plan, saved, problem, s.program())
}

// renderSettingsWith is renderSettings with the program section already
// having something to say — a version installed, a restart booked, a refusal.
func (s *Server) renderSettingsWith(w http.ResponseWriter, r *http.Request,
	plan planbus.Plan, saved bool, problem string, program programView) {

	view := settingsView{
		chrome:   s.chromeFor("Settings", "/settings"),
		Plan:     plan,
		Targets:  strings.Join(plan.Targets, "\n"),
		Excludes: strings.Join(plan.Excludes, "\n"),
		Times:    strings.Join(plan.Schedule.Times, ", "),
		Saved:    saved,
		Problem:  problem,

		MeteredKnown: s.cfg.MeteredKnown,
		Program:      program,
	}

	view.NodeID = plan.NodeID

	// The one page that reloads itself, and only while a restart is pending.
	// A redirect would be served by a process that is about to stop existing.
	if secs := program.RestartSeconds(); secs > 0 {
		view.Refresh = secs
	}

	s.render(w, r, "settings.html", view)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	plan, err := s.cfg.Plan.Get(ctx)
	if err != nil && !errors.Is(err, planbus.ErrNoPlan) {
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	// The form carries only what a person on this machine should decide. The
	// node ID and the repository are not on it: those identify the machine to
	// the fleet and name the bucket it writes to, and getting either wrong
	// silently detaches a computer from its own backup history. They are
	// changed by re-enrolling, deliberately, by whoever is administering the
	// fleet.
	plan.Targets = lines(r.PostFormValue("targets"))
	plan.Excludes = lines(r.PostFormValue("excludes"))
	plan.Schedule.Times = splitTimes(r.PostFormValue("times"))
	plan.Paused = r.PostFormValue("paused") == "on"
	plan.SkipOnMetered = r.PostFormValue("skip_on_metered") == "on"

	// An empty box is "no limit" rather than an error, and a bad one leaves
	// the stored value alone — the same shape as the two tuning numbers below,
	// which have always behaved this way.
	switch raw := strings.TrimSpace(r.PostFormValue("skip_larger_than_gb")); {
	case raw == "":
		plan.SkipLargerThanGB = 0

	default:
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			plan.SkipLargerThanGB = n
		}
	}

	if n, err := strconv.Atoi(r.PostFormValue("read_concurrency")); err == nil {
		plan.ReadConcurrency = n
	}

	if n, err := strconv.Atoi(r.PostFormValue("pack_size_mib")); err == nil {
		plan.PackSizeMiB = n
	}

	// The same check the setup page makes, for the same reason and on the same
	// box: a folder that is not there is backed up silently and successfully
	// as nothing at all. It is here as well as there because this page edits
	// the same list, and a check one of two doors enforces is not a check.
	if problem := unusable(plan.Targets); problem != "" {
		s.renderSettings(w, r, plan, false, problem)

		return
	}

	if err := s.cfg.Plan.Put(ctx, plan, time.Now()); err != nil {
		// Re-rendered with what they typed still in the boxes. Losing a
		// carefully assembled exclude list to a validation error is the kind
		// of thing that stops somebody maintaining it.
		s.renderSettings(w, r, plan, false, err.Error())

		return
	}

	http.Redirect(w, r, "/settings?saved", http.StatusSeeOther)
}

func (s *Server) runNow(w http.ResponseWriter, r *http.Request) {
	// A short deadline of its own: this returns as soon as the run has
	// started, and the run continues under the daemon's context rather than
	// this request's. A backup tied to a request context would be cancelled
	// the moment somebody closed the tab.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.cfg.StartRun(ctx); err != nil {
		if errors.Is(err, backupbus.ErrAlreadyRunning) {
			// Said out loud. A silent redirect to a page that already showed a
			// backup running is the same page again, and somebody who pressed
			// a button and saw nothing change presses it again.
			http.Redirect(w, r, "/?running", http.StatusSeeOther)

			return
		}

		s.fail(w, r, "starting a backup", err)

		return
	}

	http.Redirect(w, r, "/?started", http.StatusSeeOther)
}

func (s *Server) chromeFor(title, current string) chrome {
	links := []page.Link{
		{Href: "/", Label: "Status"},
		{Href: "/setup", Label: "Set up"},
		{Href: "/settings", Label: "Settings"},
		{Href: "/rotation", Label: "Fresh start"},
		{Href: "/card", Label: "Restore card"},
		{Href: "/access", Label: "Access"},
	}

	for i := range links {
		links[i].Current = links[i].Href == current
	}

	return chrome{
		Title:   title,
		Nav:     links,
		Version: s.cfg.Version,
		DataDir: s.cfg.Paths.DataDir,
		Exe:     s.cfg.Executable,
		Token:   s.cfg.Guard.Token(),
	}
}

// render writes a page, buffering first.
//
// Buffered because a template that fails halfway has already written a partial
// document and a 200 status, and the browser shows half a page with no
// indication that anything went wrong.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf strings.Builder

	if err := s.pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, r, "rendering "+name, err)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(buf.String()))
}

// fail logs the detail and shows the person something honest but plain.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, doing string, err error) {
	s.cfg.Log.Error("the status page failed", "doing", doing, "path", r.URL.Path, "err", err)

	http.Error(w, "Something went wrong "+doing+". The details are in the log:\n"+
		s.cfg.Paths.Log, http.StatusInternalServerError)
}

// lines splits a textarea into non-empty trimmed lines.
func lines(s string) []string {
	var out []string

	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}

	return out
}

// splitTimes accepts "13:00, 22:00" or one per line, because both are things
// people type into a box.
func splitTimes(s string) []string {
	var out []string

	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == '\r'
	}) {
		out = append(out, f)
	}

	return out
}
