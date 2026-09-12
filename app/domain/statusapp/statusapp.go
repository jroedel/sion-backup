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

	// StartRun begins a backup now.
	//
	// A function rather than the backup domain itself, because starting a run
	// means loading credentials, and the composition root is the only place
	// that does that. Handing this app a closure keeps every secret out of the
	// package that renders HTML.
	StartRun func(context.Context) error

	// Guard supplies the form token.
	Guard *loopback.Guard

	// Paths is shown in the footer, so somebody looking for their data does
	// not have to guess.
	Paths paths.Paths

	// Version is this build.
	Version string

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
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("GET /setup", s.setup)
	mux.HandleFunc("POST /setup", s.saveSetup)
	mux.HandleFunc("POST /setup/speed", s.retestSpeed)
	mux.HandleFunc("POST /setup/measure", s.remeasure)
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
	Token   string

	// Refresh is the meta-refresh interval in seconds, or 0. Set only while a
	// backup is running: a page that reloads itself every five seconds forever
	// is a page that keeps a laptop's screen awake.
	Refresh int
}

// statusView is the front page.
type statusView struct {
	chrome

	Configured  bool
	Confirmed   bool
	Paused      bool
	Verdict     verdict
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

	// Rotation is the "a fresh start would reclaim this much" suggestion, and
	// the measurement behind it. Rotation.Show decides whether it appears.
	Rotation    planbus.Offer
	Measurement planbus.Measurement
	Saving      string
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

	last, err := s.cfg.Backups.Last(ctx)

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

	if m, err := s.cfg.Plan.Measurement(ctx, plan.Repository, time.Now()); err == nil {
		view.Measurement = m
		view.Rotation = m.ConsiderRotation(time.Now())
		view.Saving = view.Rotation.MonthlySaving(s.cfg.StoragePricePerTiBMonth)
	} else {
		s.cfg.Log.Warn("could not read the repository measurement", "err", err)
	}

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

	view := settingsView{
		chrome:   s.chromeFor("Settings", "/settings"),
		Plan:     plan,
		Targets:  strings.Join(plan.Targets, "\n"),
		Excludes: strings.Join(plan.Excludes, "\n"),
		Times:    strings.Join(plan.Schedule.Times, ", "),
		Saved:    saved,
		Problem:  problem,

		MeteredKnown: s.cfg.MeteredKnown,
	}

	view.NodeID = plan.NodeID

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
			http.Redirect(w, r, "/", http.StatusSeeOther)

			return
		}

		s.fail(w, r, "starting a backup", err)

		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) chromeFor(title, current string) chrome {
	links := []page.Link{
		{Href: "/", Label: "Status"},
		{Href: "/setup", Label: "Set up"},
		{Href: "/settings", Label: "Settings"},
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
